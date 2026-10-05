package simulation

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
)

// ProjectRuntimeState 保存 Project 与 Runtime 的恢复关系。
// LastInstance 让 Runtime 离线时 Studio 仍能显示最后已知状态，但 Framework 不会
// 据此重放任何 Robot 命令。
type ProjectRuntimeState struct {
	ProjectID             string                `json:"project_id"`
	RuntimeInstallationID string                `json:"runtime_installation_id,omitempty"`
	RuntimeProfileID      string                `json:"runtime_profile_id,omitempty"`
	CatalogSceneID        string                `json:"catalog_scene_id,omitempty"`
	SceneVersion          string                `json:"scene_version,omitempty"`
	VariantID             string                `json:"variant_id,omitempty"`
	Evaluation            *EvaluationDescriptor `json:"evaluation,omitempty"`
	InstanceID            string                `json:"instance_id,omitempty"`
	LastInstance          *SceneInstance        `json:"last_instance,omitempty"`
	Revision              int64                 `json:"revision"`
	UpdatedAt             time.Time             `json:"updated_at"`
}

type RuntimeStateStore interface {
	LoadRuntimeState(string) (ProjectRuntimeState, error)
	SaveRuntimeState(ProjectRuntimeState) error
}

// RuntimeBundleReader 由 FileStore 提供，用来在启动前检查构建确实属于当前 Project。
type RuntimeBundleReader interface {
	LoadRuntimeBundle(projectID, bundleID string) (RuntimeBundle, error)
}

// SceneSnapshotConsumer 是 Semantic Map 分支的接入点。
// simulation 包只提供引擎无关快照，不复制或依赖 Map 域类型。
type SceneSnapshotConsumer interface {
	ApplySimulationSnapshot(context.Context, string, SceneSnapshot) error
}

// Service 编排多个 Runtime profile、Project 场景实例、视觉内容和类型化 Robot 调试。

// SceneCheckpointConsumer 区分新地图版本检查点与普通 revision 检查点。
// 高频物理帧和传感器帧不会调用该接口。
type SceneCheckpointConsumer interface {
	ApplySimulationCheckpoint(
		context.Context, string, SceneSnapshot, string, bool,
	) error
}

type SceneSourceLinkResolver interface {
	ResolveSimulationSource(projectID string, generation int64, sourceID string) (string, error)
	ResolveSimulationEntity(projectID string, generation int64, entityID string) (string, error)
}

// 一个具体 Runtime 仍只允许一个活动场景；Framework 不实现物理或控制算法。
type Service struct {
	previewMu       sync.Mutex
	previewJobs     sync.Map
	registry        *RuntimeRegistry
	supervisor      *RuntimeSupervisor
	state           RuntimeStateStore
	mapSink         SceneSnapshotConsumer
	installations   *RuntimeInstallationCatalog
	sceneCatalog    *SceneCatalogService
	projectBindings ProjectRuntimePreferenceResolver
	robotLifecycle  RobotLifecycleCoordinator
	stateMu         sync.Mutex
}

// NewService 保留单 native profile 装配入口，便于现有部署平滑迁移。
func NewService(client RuntimeClient, launcher RuntimeLauncher, state RuntimeStateStore) *Service {
	profile := RuntimeProfile{
		RuntimeProfileID: "native-mujoco", Name: "Native MuJoCo",
		Engine: "mujoco", Loader: "native", APIVersion: "v1",
		SceneKinds: []string{"scene_document", "asset_scene"},
		Capabilities: RuntimeCapability{
			EditableScene: true, Viewer: true, SceneStep: true, SceneReset: true,
			ViewerCameraModes: []string{"free", "fixed"},
			RobotModels:       []string{"r1pro"},
			SensorKinds:       []string{"rgb", "depth", "contact", "holding", "robot_state"},
		},
	}
	registry, _ := NewRuntimeRegistry(RuntimeBinding{Profile: profile, Client: client, Launcher: launcher})
	return NewServiceWithRegistry(registry, state, nil)
}

func NewServiceWithRegistry(
	registry *RuntimeRegistry, state RuntimeStateStore, mapSink SceneSnapshotConsumer,
) *Service {
	return &Service{
		registry: registry, supervisor: NewRuntimeSupervisor(registry),
		state: state, mapSink: mapSink,
	}
}

func (s *Service) Profiles() []RuntimeProfile { return s.registry.Profiles() }

// Shutdown 只回收由Framework启动的Runtime进程。外部安装没有Supervisor
// process句柄，因此不会被关闭。App必须在受管Robot实例取得hold并退出之后
// 调用这里，避免Runtime先消失而Pilot无法形成安全停止证据。
func (s *Service) Shutdown(ctx context.Context) error {
	return s.supervisor.Close(ctx)
}

func (s *Service) EnsureRuntime(ctx context.Context, profileIDs ...string) (RuntimeInfo, error) {
	profileID := ""
	if len(profileIDs) > 0 {
		profileID = profileIDs[0]
	}
	if profileID == "" {
		var err error
		profileID, err = s.registry.DefaultProfileID()
		if err != nil {
			return RuntimeInfo{}, err
		}
	}
	return s.supervisor.Ensure(ctx, profileID)
}

// ListScenes 汇总可连接 profile 的场景目录。单个 profile 离线不会隐藏其他环境。
func (s *Service) ListScenes(ctx context.Context) ([]SceneDescriptor, error) {
	var result []SceneDescriptor
	var lastErr error
	for _, profile := range s.registry.Profiles() {
		// 列目录是只读发现操作，不能因为打开 Studio 就启动所有隔离 Python 环境。
		if _, err := s.supervisor.Probe(ctx, profile.RuntimeProfileID); err != nil {
			lastErr = err
			continue
		}
		binding, _ := s.registry.Binding(profile.RuntimeProfileID)
		scenes, err := binding.Client.ListScenes(ctx)
		if err != nil {
			lastErr = err
			continue
		}
		for index := range scenes {
			if len(scenes[index].CompatibleRuntimeProfiles) == 0 {
				scenes[index].CompatibleRuntimeProfiles = []string{profile.RuntimeProfileID}
			}
		}
		result = append(result, scenes...)
	}
	if len(result) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return result, nil
}

func (s *Service) ValidateDocumentProfile(document SceneDocument, profileID string) error {
	binding, err := s.registry.Binding(profileID)
	if err != nil {
		return err
	}
	if !binding.Profile.Capabilities.EditableScene {
		return fmt.Errorf("%w: Runtime profile %s 不支持可编辑场景", ErrConflict, profileID)
	}
	sceneKind := document.SceneKind
	if sceneKind == "" {
		sceneKind = "scene_document"
	}
	if !s.registry.Compatible(profileID, sceneKind) {
		return fmt.Errorf("%w: 场景类型 %s 与 Runtime profile %s 不兼容",
			ErrConflict, sceneKind, profileID)
	}
	return nil
}

func (s *Service) RegisterRuntimeBundle(
	ctx context.Context, bundle RuntimeBundle,
) (RuntimeBundleResult, error) {
	binding, err := s.registry.Binding(bundle.RuntimeProfileID)
	if err != nil {
		return RuntimeBundleResult{}, err
	}
	if !binding.Profile.Capabilities.EditableScene ||
		!s.registry.Compatible(bundle.RuntimeProfileID, bundle.Document.SceneKind) {
		return RuntimeBundleResult{}, fmt.Errorf(
			"%w: RuntimeBundle 场景类型与 profile 不兼容", ErrConflict)
	}
	if _, err := s.supervisor.Ensure(ctx, bundle.RuntimeProfileID); err != nil {
		return RuntimeBundleResult{}, err
	}
	binding, _ = s.registry.Binding(bundle.RuntimeProfileID)
	result, err := binding.Client.RegisterRuntimeBundle(ctx, bundle)
	if err != nil {
		return RuntimeBundleResult{}, err
	}
	if err := validateRuntimeBundleResult(bundle, result); err != nil {
		return result, err
	}
	return result, nil
}

func validateRuntimeBundleResult(bundle RuntimeBundle, result RuntimeBundleResult) error {
	if !result.Valid {
		return fmt.Errorf("%w: Runtime 拒绝场景构建: %v", ErrConflict, result.Issues)
	}
	if result.RuntimeBundleID != bundle.RuntimeBundleID ||
		result.SceneKey != bundle.SceneKey ||
		result.RuntimeProfileID != bundle.RuntimeProfileID {
		return fmt.Errorf(
			"%w: RuntimeBundle 注册结果与请求身份不一致", ErrRuntimeUnavailable)
	}
	if strings.TrimSpace(result.RuntimeSceneKey) == "" {
		return fmt.Errorf("%w: RuntimeBundle 注册结果缺少 runtime_scene_key", ErrRuntimeUnavailable)
	}
	if result.Descriptor != nil && result.Descriptor.SceneKey != bundle.SceneKey {
		return fmt.Errorf("%w: RuntimeBundle 注册结果包含错误的场景描述", ErrRuntimeUnavailable)
	}
	return nil
}

func (s *Service) StartScene(
	ctx context.Context, projectID, sceneKey string, request SceneStartRequest,
) (SceneInstance, error) {
	if projectID == "" || sceneKey == "" || request.RequestID == "" {
		return SceneInstance{}, errors.New("project_id、scene_key 和 request_id 不能为空")
	}
	profileID, err := s.resolveProjectProfile(projectID, request.RuntimeProfileID)
	if err != nil {
		return SceneInstance{}, err
	}
	runtimeID, err := s.runtimeIdentity(projectID, profileID, request.RuntimeInstallationID)
	if err != nil {
		return SceneInstance{}, err
	}
	request.RuntimeProfileID = profileID
	// 本机内容引用只来自已安装场景目录，覆盖客户端输入，避免任意宿主路径进入 Runtime。
	request.SceneContentRoot = ""
	if s.sceneCatalog != nil {
		request.SceneContentRoot, err = s.sceneCatalog.contentRoot(profileID, sceneKey)
		if err != nil {
			return SceneInstance{}, err
		}
	}
	var persistedBundle *RuntimeBundle
	if request.RuntimeBundleID != "" {
		reader, ok := s.state.(RuntimeBundleReader)
		if !ok {
			return SceneInstance{}, errors.New("当前 RuntimeStateStore 不支持 RuntimeBundle 归属检查")
		}
		bundle, err := reader.LoadRuntimeBundle(projectID, request.RuntimeBundleID)
		if err != nil {
			return SceneInstance{}, err
		}
		if bundle.SceneKey != sceneKey || bundle.RuntimeProfileID != profileID {
			return SceneInstance{}, fmt.Errorf("%w: RuntimeBundle 与场景或 profile 不一致", ErrConflict)
		}
		persistedBundle = &bundle
	}
	if s.installations != nil {
		s.installations.ObserveStatus(runtimeID, "starting", "")
	}
	info, err := s.supervisor.Ensure(ctx, runtimeID)
	if err != nil {
		if s.installations != nil {
			s.installations.ObserveStatus(runtimeID, "failed", err.Error())
		}
		return SceneInstance{}, err
	}
	// StartScene 能继续到 Runtime API，已经证明这套 Installation 可连接。
	// 必须同步更新进程内观测，否则场景真实运行时安装卡仍保留启动期 offline。
	if s.installations != nil {
		s.installations.ObserveStatus(runtimeID, "ready", "")
	}
	if persistedBundle != nil {
		binding, bindingErr := s.registry.Binding(runtimeID)
		if bindingErr != nil {
			return SceneInstance{}, bindingErr
		}
		result, registerErr := binding.Client.RegisterRuntimeBundle(ctx, *persistedBundle)
		if registerErr != nil {
			return SceneInstance{}, registerErr
		}
		if resultErr := validateRuntimeBundleResult(*persistedBundle, result); resultErr != nil {
			return SceneInstance{}, resultErr
		}
	}

	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	current, err := s.state.LoadRuntimeState(projectID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return SceneInstance{}, err
	}
	if current.InstanceID != "" && current.RuntimeProfileID != profileID {
		return SceneInstance{}, fmt.Errorf("%w: Project 已有活动场景", ErrConflict)
	}
	if info.ActiveInstanceID != "" && current.InstanceID != info.ActiveInstanceID {
		return SceneInstance{}, fmt.Errorf("%w: Runtime 已由另一个 Project 使用", ErrConflict)
	}
	binding, _ := s.registry.Binding(runtimeID)
	instance, err := binding.Client.StartScene(ctx, sceneKey, request)
	if err != nil {
		return SceneInstance{}, err
	}
	instance.RuntimeProfileID = profileID
	if instance.RuntimeBundleID == "" {
		instance.RuntimeBundleID = request.RuntimeBundleID
	}
	current.ProjectID = projectID
	current.RuntimeInstallationID = runtimeID
	current.RuntimeProfileID = profileID
	current.InstanceID = instance.InstanceID
	current.LastInstance = &instance
	current.Revision++
	current.UpdatedAt = time.Now().UTC()
	if err := s.state.SaveRuntimeState(current); err != nil {
		return SceneInstance{}, err
	}
	return instance, nil
}

func (s *Service) Scene(ctx context.Context, projectID, instanceID string) (SceneInstance, error) {
	state, binding, err := s.projectRuntime(projectID, instanceID)
	if err != nil {
		return SceneInstance{}, err
	}
	instance, err := binding.Client.Scene(ctx, state.InstanceID)
	if err == nil {
		instance.RuntimeProfileID = state.RuntimeProfileID
	}
	return instance, err
}

func (s *Service) SceneOperation(
	ctx context.Context, projectID, instanceID, operation string, body any,
) (SceneInstance, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	state, binding, err := s.projectRuntime(projectID, instanceID)
	if err != nil {
		return SceneInstance{}, err
	}
	switch operation {
	case "pause", "resume", "step", "reset", "stop":
	default:
		return SceneInstance{}, fmt.Errorf("不支持的场景操作: %s", operation)
	}
	if operation == "step" && !binding.Profile.Capabilities.SceneStep {
		return SceneInstance{}, fmt.Errorf("%w: 当前 Runtime 不支持单步", ErrConflict)
	}
	if operation == "reset" && !binding.Profile.Capabilities.SceneReset {
		return SceneInstance{}, fmt.Errorf("%w: 当前 Runtime 不支持 reset", ErrConflict)
	}
	if operation == "reset" {
		if state.LastInstance == nil {
			return SceneInstance{}, fmt.Errorf("%w: 场景缺少当前 generation", ErrConflict)
		}
		if err := s.holdSceneRobots(ctx, projectID, instanceID,
			state.LastInstance.Generation, binding.Client, "scene_reset"); err != nil {
			return SceneInstance{}, err
		}
	}
	if operation == "stop" {
		if state.LastInstance == nil {
			return SceneInstance{}, fmt.Errorf("%w: 场景缺少当前 generation", ErrConflict)
		}
		// stop 与 reset 共享同一个物理安全边界：先由 Runtime 同步确认底盘、
		// 双臂和工具已 hold，再退出受管 Robot 进程。否则 Pilot 已断线时
		// supervisor 只能进入 interrupted，七个 Ability 会被保留，下一次
		// 场景启动又会被旧 Robot ID 阻塞。
		if err := s.holdSceneRobots(ctx, projectID, instanceID,
			state.LastInstance.Generation, binding.Client, "scene_stop"); err != nil {
			return SceneInstance{}, err
		}
		if err := s.stopSceneRobots(ctx, projectID, instanceID, "scene_stop"); err != nil {
			return SceneInstance{}, err
		}
	}
	var checkpointErr error
	if operation == "stop" && (state.LastInstance.State == "running" || state.LastInstance.State == "paused") {
		checkpointErr = s.syncSceneCheckpoint(ctx, projectID, instanceID,
			"scene_stop_checkpoint", false)
	}
	instance, err := binding.Client.SceneOperation(ctx, state.InstanceID, operation, body)
	if err != nil {
		return SceneInstance{}, err
	}
	if operation == "stop" && instance.State != "stopped" {
		return SceneInstance{}, fmt.Errorf("%w: Runtime stop 返回状态 %s", ErrConflict, instance.State)
	}
	instance.RuntimeProfileID = state.RuntimeProfileID
	state.LastInstance = &instance
	state.Revision++
	state.UpdatedAt = time.Now().UTC()
	if operation == "stop" {
		state.InstanceID = ""
	}
	if err := s.state.SaveRuntimeState(state); err != nil {
		return SceneInstance{}, err
	}
	if operation == "reset" {
		checkpointErr = errors.Join(checkpointErr,
			s.syncSceneCheckpoint(ctx, projectID, instanceID, "scene_reset", true))
	}
	if checkpointErr != nil {
		return instance, fmt.Errorf("场景操作已完成，但地图检查点同步失败: %w", checkpointErr)
	}
	return instance, nil
}

func (s *Service) SceneSnapshot(
	ctx context.Context, projectID, instanceID string,
) (SceneSnapshot, error) {
	state, binding, err := s.projectRuntime(projectID, instanceID)
	if err != nil {
		return SceneSnapshot{}, err
	}
	snapshot, err := binding.Client.SceneSnapshot(ctx, state.InstanceID)
	if err != nil {
		return SceneSnapshot{}, err
	}
	if state.LastInstance != nil && snapshot.Generation != state.LastInstance.Generation {
		return SceneSnapshot{}, fmt.Errorf("%w: 场景快照 generation 与实例不一致", ErrConflict)
	}
	return snapshot, nil
}

// SyncSceneMap 是用户显式同步入口。SceneSnapshot 本身必须保持纯读取，避免
// Viewer 刷新、相机读取或状态轮询意外把连续物理位姿写入 Semantic Map。
func (s *Service) SyncSceneMap(
	ctx context.Context, projectID, instanceID string,
) (SceneSnapshot, error) {
	snapshot, err := s.SceneSnapshot(ctx, projectID, instanceID)
	if err != nil {
		return SceneSnapshot{}, err
	}
	if err := s.applySceneCheckpoint(ctx, projectID, snapshot, "manual", false); err != nil {
		return snapshot, fmt.Errorf("更新仿真地图失败: %w", err)
	}
	return snapshot, nil
}

// SyncSceneMapCheckpoint 在 Robot Skill 已完成独立物理验证后写入同一 Map
// generation 的新 revision。reset/layout 才创建新 generation；抓取和放置
// 不能把 Runtime generation 当成外部 Map generation。
func (s *Service) SyncSceneMapCheckpoint(
	ctx context.Context, projectID, instanceID, reason string,
) error {
	return s.syncSceneCheckpoint(ctx, projectID, instanceID, reason, false)
}

func (s *Service) SceneEvaluation(
	ctx context.Context, projectID, instanceID string,
) (SceneEvaluation, error) {
	state, binding, err := s.projectRuntime(projectID, instanceID)
	if err != nil {
		return SceneEvaluation{}, err
	}
	if !binding.Profile.Capabilities.NativeEvaluator {
		return SceneEvaluation{}, fmt.Errorf("%w: 当前 Runtime 不提供原生评测", ErrConflict)
	}
	if s.sceneCatalog != nil && state.Evaluation == nil {
		return SceneEvaluation{}, fmt.Errorf("%w: 当前场景版本没有声明评测能力", ErrConflict)
	}
	evaluation, err := binding.Client.SceneEvaluation(ctx, state.InstanceID)
	if err != nil {
		return SceneEvaluation{}, err
	}
	if state.LastInstance != nil && evaluation.Generation != state.LastInstance.Generation {
		return SceneEvaluation{}, fmt.Errorf("%w: 评测结果 generation 与实例不一致", ErrConflict)
	}
	if err := s.syncSceneCheckpoint(ctx, projectID, instanceID, "evaluation_completed", false); err != nil {
		return evaluation, fmt.Errorf("评测完成，但地图检查点同步失败: %w", err)
	}
	return evaluation, nil
}

func (s *Service) Robots(ctx context.Context, projectID, instanceID string) ([]VirtualRobotDescriptor, error) {
	state, binding, err := s.projectRuntime(projectID, instanceID)
	if err != nil {
		return nil, err
	}
	robots, err := binding.Client.Robots(ctx, state.InstanceID)
	if err != nil {
		return nil, err
	}
	runtimeInfo, err := binding.Client.Runtime(ctx)
	if err != nil {
		return nil, err
	}
	for index := range robots {
		robots[index].SceneInstanceID = state.InstanceID
		if robots[index].Endpoint == "" {
			robots[index].Endpoint = runtimeInfo.Endpoint
		}
	}
	return robots, nil
}

func (s *Service) ResolveSourceLink(
	projectID string, generation int64, sourceID, entityID string,
) (SourceLink, error) {
	resolver, ok := s.mapSink.(SceneSourceLinkResolver)
	if !ok {
		return SourceLink{}, fmt.Errorf("%w: Semantic Map 映射尚未启用", ErrNotFound)
	}
	if (sourceID == "") == (entityID == "") {
		return SourceLink{}, fmt.Errorf("%w: 必须且只能指定 source_id 或 entity_id", ErrConflict)
	}
	var err error
	if sourceID != "" {
		entityID, err = resolver.ResolveSimulationSource(projectID, generation, sourceID)
	} else {
		sourceID, err = resolver.ResolveSimulationEntity(projectID, generation, entityID)
	}
	if err != nil {
		return SourceLink{}, err
	}
	return SourceLink{
		MapID: "simulation_map", Generation: generation,
		SourceID: sourceID, EntityID: entityID,
	}, nil
}

func (s *Service) SensorFramesURL(projectID, instanceID, robotID, sensorID string) (string, error) {
	_, binding, err := s.projectRuntime(projectID, instanceID)
	if err != nil {
		return "", err
	}
	if _, err := s.findRobot(context.Background(), binding, instanceID, robotID); err != nil {
		return "", err
	}
	sensors, err := binding.Client.RobotSensors(context.Background(), robotID)
	if err != nil {
		return "", err
	}
	found := false
	for _, sensor := range sensors {
		if sensor.SensorID == sensorID {
			found = true
			break
		}
	}
	if !found {
		return "", ErrNotFound
	}
	return binding.Client.SensorFramesURL(robotID, sensorID), nil
}

func (s *Service) RobotState(
	ctx context.Context, projectID, instanceID, robotID string,
) (RobotStateSnapshot, error) {
	state, binding, err := s.projectRuntime(projectID, instanceID)
	if err != nil {
		return RobotStateSnapshot{}, err
	}
	if _, err := s.findRobot(ctx, binding, state.InstanceID, robotID); err != nil {
		return RobotStateSnapshot{}, err
	}
	return binding.Client.RobotState(ctx, robotID)
}

func (s *Service) RobotSensors(
	ctx context.Context, projectID, instanceID, robotID string,
) ([]SensorDescriptor, error) {
	state, binding, err := s.projectRuntime(projectID, instanceID)
	if err != nil {
		return nil, err
	}
	if _, err := s.findRobot(ctx, binding, state.InstanceID, robotID); err != nil {
		return nil, err
	}
	return binding.Client.RobotSensors(ctx, robotID)
}

func (s *Service) SubmitRobotCommand(
	ctx context.Context, projectID, instanceID, robotID string, command RobotDebugCommand,
) (RobotCommandRecord, error) {
	state, binding, err := s.projectRuntime(projectID, instanceID)
	if err != nil {
		return RobotCommandRecord{}, err
	}
	if state.LastInstance == nil || command.SceneGeneration != state.LastInstance.Generation {
		return RobotCommandRecord{}, fmt.Errorf("%w: Robot 命令 generation 与场景不一致", ErrConflict)
	}
	robot, err := s.findRobot(ctx, binding, state.InstanceID, robotID)
	if err != nil {
		return RobotCommandRecord{}, err
	}
	if err := validateRobotDebugCommand(robot, command); err != nil {
		return RobotCommandRecord{}, err
	}
	return binding.Client.SubmitRobotCommand(ctx, robotID, command)
}

func (s *Service) RobotCommand(
	ctx context.Context, projectID, instanceID, robotID, commandID string,
) (RobotCommandRecord, error) {
	_, binding, err := s.projectRuntime(projectID, instanceID)
	if err != nil {
		return RobotCommandRecord{}, err
	}
	return binding.Client.RobotCommand(ctx, robotID, commandID)
}

func (s *Service) StopRobotCommand(
	ctx context.Context, projectID, instanceID, robotID, commandID string,
) (RobotCommandRecord, error) {
	_, binding, err := s.projectRuntime(projectID, instanceID)
	if err != nil {
		return RobotCommandRecord{}, err
	}
	return binding.Client.StopRobotCommand(ctx, robotID, commandID)
}

func (s *Service) HoldRobot(
	ctx context.Context, projectID, instanceID, robotID string, generation int64,
) (RobotCommandRecord, error) {
	state, binding, err := s.projectRuntime(projectID, instanceID)
	if err != nil {
		return RobotCommandRecord{}, err
	}
	if state.LastInstance == nil || generation != state.LastInstance.Generation {
		return RobotCommandRecord{}, fmt.Errorf("%w: hold generation 与场景不一致", ErrConflict)
	}
	return binding.Client.HoldRobot(ctx, robotID, generation)
}

func validateRobotDebugCommand(robot VirtualRobotDescriptor, command RobotDebugCommand) error {
	if command.CommandID == "" || command.SceneGeneration < 1 {
		return errors.New("command_id 和 scene_generation 不能为空")
	}
	if command.TimeoutSeconds < 0 || math.IsNaN(command.TimeoutSeconds) ||
		math.IsInf(command.TimeoutSeconds, 0) {
		return errors.New("timeout_seconds 必须是非负有限数")
	}
	if !slices.Contains(robot.Capabilities.Commands, command.Type) {
		return fmt.Errorf("%w: Robot 不支持命令 %s", ErrConflict, command.Type)
	}
	targets := 0
	if command.Joint != nil {
		targets++
	}
	if command.Base != nil {
		targets++
	}
	if command.Gripper != nil {
		targets++
	}
	if targets != 1 {
		return errors.New("Robot 调试命令必须且只能包含一个目标")
	}
	switch command.Type {
	case RobotCommandJointTrajectory:
		if command.Joint == nil {
			return errors.New("joint_trajectory 必须包含关节轨迹")
		}
		if err := validateTrajectory(command.Joint.Points, robot.JointNames); err != nil {
			if len(command.Joint.Resources) == 0 {
				return errors.New("joint_trajectory 必须声明至少一个冲突资源")
			}
			seenResources := make(map[string]struct{}, len(command.Joint.Resources))
			for _, resource := range command.Joint.Resources {
				resource = strings.TrimSpace(resource)
				if resource == "" {
					return errors.New("joint_trajectory resource 不能为空")
				}
				if _, exists := seenResources[resource]; exists {
					return errors.New("joint_trajectory resources 不能重复")
				}
				seenResources[resource] = struct{}{}
			}
			return fmt.Errorf("关节轨迹无效: %w", err)
		}
	case RobotCommandBaseTrajectory:
		if command.Base == nil {
			return errors.New("base_trajectory 必须包含底盘轨迹")
		}
		if err := validateTrajectory(command.Base.Points, nil); err != nil {
			return fmt.Errorf("底盘轨迹无效: %w", err)
		}
	case RobotCommandGripper:
		if command.Gripper == nil ||
			!slices.Contains(robot.Grippers, command.Gripper.GripperID) {
			return errors.New("gripper_command 必须指定 Robot 声明的 gripper_id")
		}
		if math.IsNaN(command.Gripper.Position) || math.IsInf(command.Gripper.Position, 0) {
			return errors.New("夹爪目标必须是有限数")
		}
	default:
		return errors.New("commands 端点只接受 joint_trajectory、base_trajectory 或 gripper_command")
	}
	return nil
}

// validateTrajectory 只检查跨 Runtime 都应满足的格式和数值条件。
// Robot SDK 仍负责限位、碰撞和轨迹生成；Framework 不重复运动学计算。
func validateTrajectory(points []TrajectoryPoint, allowedResources []string) error {
	if len(points) == 0 {
		return errors.New("轨迹点不能为空")
	}
	previous := -1.0
	for index, point := range points {
		if point.TimeFromStartSeconds < 0 ||
			math.IsNaN(point.TimeFromStartSeconds) || math.IsInf(point.TimeFromStartSeconds, 0) ||
			point.TimeFromStartSeconds <= previous {
			return fmt.Errorf("第 %d 个轨迹点时间必须严格递增且为有限数", index+1)
		}
		if len(point.Positions) == 0 {
			return fmt.Errorf("第 %d 个轨迹点没有目标", index+1)
		}
		for resource, value := range point.Positions {
			if resource == "" || math.IsNaN(value) || math.IsInf(value, 0) {
				return fmt.Errorf("第 %d 个轨迹点包含无效目标", index+1)
			}
			if len(allowedResources) > 0 && !slices.Contains(allowedResources, resource) {
				return fmt.Errorf("Robot 未声明资源 %s", resource)
			}
		}
		previous = point.TimeFromStartSeconds
	}
	return nil
}

func (s *Service) Snapshot(ctx context.Context, projectID string) (ProjectSimulationSnapshot, error) {
	// Snapshot 同时承担重启核对，必须与场景/Viewer 状态写入串行，避免用旧恢复数据
	// 覆盖刚完成的 reset 或 Viewer 创建。
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	state, err := s.state.LoadRuntimeState(projectID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ProjectSimulationSnapshot{}, err
	}
	result := ProjectSimulationSnapshot{
		ProjectID: projectID, Revision: state.Revision,
		UpdatedAt: time.Now().UTC(),
	}
	runtimeIDs, installation, viewErr := s.projectRuntimeViews(projectID)
	if viewErr != nil {
		return ProjectSimulationSnapshot{}, viewErr
	}
	result.RuntimeInstallation = installation
	for _, runtimeID := range runtimeIDs {
		binding, bindingErr := s.registry.Binding(runtimeID)
		if bindingErr != nil {
			continue
		}
		profile := binding.Profile
		result.Profiles = append(result.Profiles, profile)
		info, probeErr := s.supervisor.Probe(ctx, runtimeID)
		if probeErr != nil {
			if s.installations != nil {
				s.installations.ObserveStatus(runtimeID, "offline", probeErr.Error())
			}
			info = RuntimeInfo{RuntimeInstallationID: runtimeID, RuntimeProfileID: profile.RuntimeProfileID,
				Engine: profile.Engine, State: "offline", Capabilities: profile.Capabilities}
		} else if s.installations != nil {
			s.installations.ObserveStatus(runtimeID, "ready", "")
		}
		result.Runtimes = append(result.Runtimes, info)
		if probeErr == nil {
			if scenes, listErr := binding.Client.ListScenes(ctx); listErr == nil {
				result.Scenes = append(result.Scenes, scenes...)
			}
		}
	}
	// projectRuntimeViews 在探测前读取安装视图。探测已经得到更新事实后重读一次，
	// 保证同一个 Snapshot 中 runtime 与 runtime_installation 不会互相矛盾。
	if result.RuntimeInstallation != nil && s.installations != nil {
		if latest, latestErr := s.installations.View(
			result.RuntimeInstallation.InstallationID,
		); latestErr == nil {
			result.RuntimeInstallation = &latest
		}
	}
	if state.InstanceID == "" {
		// LastInstance 只用于服务端恢复诊断；活动 ID 清空时必须返回 instance=null。
		return result, nil
	}
	result.CatalogSceneID = state.CatalogSceneID
	result.SceneVersion = state.SceneVersion
	result.VariantID = state.VariantID
	result.EvaluationDescriptor = state.Evaluation
	runtimeID := state.RuntimeInstallationID
	if runtimeID == "" {
		var identityErr error
		runtimeID, identityErr = s.runtimeIdentity(projectID, state.RuntimeProfileID, "")
		if identityErr != nil {
			setInterruptedRecovery(&result, state, "runtime_installation_missing", identityErr.Error())
			return result, nil
		}
	}
	binding, bindErr := s.registry.Binding(runtimeID)
	if bindErr != nil {
		setInterruptedRecovery(&result, state, "runtime_profile_missing", "Framework 找不到场景实例对应的 Runtime Profile")
		return result, nil
	}
	info, runtimeErr := s.supervisor.Probe(ctx, runtimeID)
	if runtimeErr != nil {
		setInterruptedRecovery(&result, state, "runtime_offline", "Runtime 当前不可连接，场景实例状态无法继续确认")
		return result, nil
	}
	if info.State == "offline" || info.State == "unknown" {
		code := "runtime_" + info.State
		setInterruptedRecovery(&result, state, code, fmt.Sprintf("Runtime 返回 %s，场景实例状态无法继续确认", info.State))
		return result, nil
	}
	if info.ActiveInstanceID != state.InstanceID {
		setInterruptedRecovery(&result, state, "instance_mismatch", "Runtime 当前实例与 Framework 最后记录不一致")
		return result, nil
	}
	instance, err := binding.Client.Scene(ctx, state.InstanceID)
	if err != nil {
		setInterruptedRecovery(&result, state, "instance_missing", "Runtime 无法确认 Framework 最后记录的场景实例")
		return result, nil
	}
	instance.RuntimeProfileID = state.RuntimeProfileID
	if state.LastInstance != nil && instance.Generation < state.LastInstance.Generation {
		setInterruptedRecovery(&result, state, "generation_regressed", "Runtime 返回的 generation 小于 Framework 最后记录")
		return result, nil
	}
	result.Instance = &instance
	// 原生引擎加载期间，Robot/评测读取需要等待仿真线程。状态快照必须先返回
	// starting 等生命周期状态，不能排队等资产加载，令已接受的启动请求在 Web
	// 上再次超时。只有 running/paused 才读取引擎数据；状态变更仍在下方持久化。
	if instance.State == "running" || instance.State == "paused" {
		result.Robots, _ = binding.Client.Robots(ctx, state.InstanceID)
		// 原生评测用于重连后的证据回看；读取失败不应使场景和停止入口一起消失。
		if binding.Profile.Capabilities.NativeEvaluator && (s.sceneCatalog == nil || state.Evaluation != nil) {
			if evaluation, evaluationErr := binding.Client.SceneEvaluation(ctx, state.InstanceID); evaluationErr == nil && evaluation.Generation == instance.Generation {
				result.Evaluation = &evaluation
			}
		}
	}
	stateChanged := state.LastInstance == nil ||
		state.LastInstance.Generation != instance.Generation ||
		state.LastInstance.State != instance.State
	if stateChanged {
		state.LastInstance = &instance
		state.Revision++
		state.UpdatedAt = time.Now().UTC()
		if saveErr := s.state.SaveRuntimeState(state); saveErr != nil {
			return ProjectSimulationSnapshot{}, saveErr
		}
		result.Revision = state.Revision
	}
	return result, nil
}

func setInterruptedRecovery(
	result *ProjectSimulationSnapshot, state ProjectRuntimeState, code, message string,
) {
	// Recovery 是现有客户端使用的简短代码；RecoveryInfo 提供给 Studio 做明确
	// 展示和禁用判断。两者都只是本次快照的派生结果，不写回 LastInstance。
	result.Recovery = code
	result.Instance = lastKnownInstance(state)
	info := &RecoveryInfo{
		Code: code, DerivedState: "interrupted", Message: message,
		RuntimeProfileID: state.RuntimeProfileID, InstanceID: state.InstanceID,
		ObservedAt: time.Now().UTC(),
	}
	if state.LastInstance != nil {
		info.LastKnownState = state.LastInstance.State
		info.LastKnownUpdatedAt = state.LastInstance.UpdatedAt
	}
	result.RecoveryInfo = info
}

func lastKnownInstance(state ProjectRuntimeState) *SceneInstance {
	if state.LastInstance == nil {
		// Framework 只知道实例身份，无法伪造 Runtime 从未返回过的运行状态。
		return &SceneInstance{InstanceID: state.InstanceID,
			RuntimeProfileID: state.RuntimeProfileID, State: "unknown"}
	}
	copy := *state.LastInstance
	return &copy
}

func (s *Service) Close(ctx context.Context) error {
	s.previewJobs.Range(func(_, value any) bool { value.(context.CancelFunc)(); return true })
	return s.supervisor.Close(ctx)
}

func (s *Service) projectRuntime(
	projectID, instanceID string,
) (ProjectRuntimeState, RuntimeBinding, error) {
	state, err := s.state.LoadRuntimeState(projectID)
	if err != nil {
		return ProjectRuntimeState{}, RuntimeBinding{}, err
	}
	if state.InstanceID == "" || state.InstanceID != instanceID {
		return ProjectRuntimeState{}, RuntimeBinding{}, ErrNotFound
	}
	runtimeID := state.RuntimeInstallationID
	if runtimeID == "" {
		runtimeID, err = s.runtimeIdentity(projectID, state.RuntimeProfileID, "")
		if err != nil {
			return ProjectRuntimeState{}, RuntimeBinding{}, err
		}
		state.RuntimeInstallationID = runtimeID
	}
	binding, err := s.registry.Binding(runtimeID)
	return state, binding, err
}

func (s *Service) findRobot(
	ctx context.Context, binding RuntimeBinding, instanceID, robotID string,
) (VirtualRobotDescriptor, error) {
	robots, err := binding.Client.Robots(ctx, instanceID)
	if err != nil {
		return VirtualRobotDescriptor{}, err
	}
	for _, robot := range robots {
		if robot.RobotID == robotID {
			return robot, nil
		}
	}
	return VirtualRobotDescriptor{}, ErrNotFound
}
