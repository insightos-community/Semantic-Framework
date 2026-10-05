// Package simulation 管理外部仿真 Runtime、场景构建、运行实例和 Studio 数据流。
//
// 本包只负责编排，不导入物理引擎，也不计算 IK、导航路径或控制轨迹。
// 这些计算由 Robot SDK 完成；Runtime 只执行 SDK 已经生成的低层轨迹。
package simulation

import (
	"encoding/json"
	"time"

	"insightos.cn/semantic-framework/internal/robotruntime"
)

// RuntimeCapability 描述 Runtime profile 能提供的功能。
// Studio 和 Framework 必须依据这些字段显示或拒绝功能，不能根据名称猜测。
type RuntimeCapability struct {
	ScenePreviews     bool     `json:"scene_previews" yaml:"scene_previews"`
	EditableScene     bool     `json:"editable_scene" yaml:"editable_scene"`
	NativeEvaluator   bool     `json:"native_evaluator" yaml:"native_evaluator"`
	Viewer            bool     `json:"viewer" yaml:"viewer"`
	ViewerCameraModes []string `json:"viewer_camera_modes" yaml:"viewer_camera_modes"`
	SceneStep         bool     `json:"scene_step" yaml:"scene_step"`
	SceneReset        bool     `json:"scene_reset" yaml:"scene_reset"`
	RobotModels       []string `json:"robot_models" yaml:"robot_models"`
	SensorKinds       []string `json:"sensor_kinds" yaml:"sensor_kinds"`
}

// RuntimeProfile 是一类可按需启动的仿真环境说明。
//
// Framework 启动时先登记期望的 engine、loader 和 API 版本；连接 Runtime 后，
// Environment、Available 和 Capabilities 必须使用 Runtime 实际返回的结果，不能
// 用 Framework 的静态配置把缺失依赖或未安装 Loader 伪装成可用。
type RuntimeProfile struct {
	RuntimeProfileID string   `json:"runtime_profile_id" yaml:"runtime_profile_id"`
	Name             string   `json:"name" yaml:"name"`
	Engine           string   `json:"engine" yaml:"engine"`
	Loader           string   `json:"loader" yaml:"loader"`
	APIVersion       string   `json:"api_version" yaml:"api_version"`
	SceneKinds       []string `json:"scene_kinds" yaml:"scene_kinds"`
	// SceneStartTimeoutSeconds 是原生场景加载预算；未声明时保留现有三分钟。
	// 由 Runtime Profile 声明，Framework 不按引擎或任务名称猜测加载成本。
	SceneStartTimeoutSeconds int               `json:"scene_start_timeout_seconds,omitempty" yaml:"scene_start_timeout_seconds,omitempty"`
	Capabilities             RuntimeCapability `json:"capabilities" yaml:"capabilities"`
	Environment              string            `json:"environment,omitempty" yaml:"environment,omitempty"`
	EnvironmentReady         bool              `json:"environment_ready" yaml:"environment_ready"`
	Available                bool              `json:"available" yaml:"available"`
	AvailabilityKnown        bool              `json:"availability_known" yaml:"availability_known"`
	UnavailableReason        string            `json:"unavailable_reason,omitempty" yaml:"unavailable_reason,omitempty"`
}

// RuntimeInfo 是 Studio 可见的 Runtime 当前状态。
// Endpoint 和 AssetRoot 只供 Server 内部连接使用，不允许序列化到浏览器。
type RuntimeInfo struct {
	RuntimeInstallationID string            `json:"runtime_installation_id,omitempty"`
	RuntimeID             string            `json:"runtime_id,omitempty"`
	RuntimeProfileID      string            `json:"runtime_profile_id"`
	State                 string            `json:"state"`
	Engine                string            `json:"engine"`
	Version               string            `json:"version,omitempty"`
	APIVersion            string            `json:"api_version,omitempty"`
	ActiveInstanceID      string            `json:"active_instance_id,omitempty"`
	Capabilities          RuntimeCapability `json:"capabilities"`
	AssetRoot             string            `json:"-"`
	Endpoint              string            `json:"-"`
	Managed               bool              `json:"managed"`
}

// SceneDescriptor 描述 Runtime 可加载的场景版本或只读原生环境。
type SceneDescriptor struct {
	SceneKey                  string   `json:"scene_key"`
	Name                      string   `json:"name"`
	SceneKind                 string   `json:"scene_kind"`
	Layouts                   []string `json:"layouts"`
	RobotModels               []string `json:"robot_models"`
	CompatibleRuntimeProfiles []string `json:"compatible_runtime_profiles"`
	ReadOnly                  bool     `json:"read_only"`
}

// SceneStartRequest 是 Framework 接收的场景启动参数。
type SceneStartRequest struct {
	SceneContentRoot      string `json:"scene_content_root,omitempty"`
	RequestID             string `json:"request_id"`
	RuntimeProfileID      string `json:"runtime_profile_id,omitempty"`
	RuntimeInstallationID string `json:"runtime_installation_id,omitempty"`
	RuntimeBundleID       string `json:"runtime_bundle_id,omitempty"`
	Layout                string `json:"layout"`
	Seed                  int64  `json:"seed"`
	Headless              bool   `json:"headless"`
	RenderBackend         string `json:"render_backend"`
}

// SceneInstance 是某个已发布场景版本的一次运行。
type SceneInstance struct {
	InstanceID       string    `json:"instance_id"`
	RuntimeID        string    `json:"runtime_id,omitempty"`
	RuntimeProfileID string    `json:"runtime_profile_id"`
	RuntimeBundleID  string    `json:"runtime_bundle_id,omitempty"`
	SceneKey         string    `json:"scene_key"`
	Layout           string    `json:"layout"`
	Seed             int64     `json:"seed"`
	Headless         bool      `json:"headless"`
	RenderBackend    string    `json:"render_backend"`
	Generation       int64     `json:"generation"`
	State            string    `json:"state"`
	Progress         float64   `json:"progress,omitempty"`
	SimTime          float64   `json:"sim_time"`
	StepCount        int64     `json:"step_count"`
	RequestID        string    `json:"request_id"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	FailureReason    string    `json:"failure_reason,omitempty"`
}

type ViewerSceneCamera struct {
	CameraID string `json:"camera_id"`
	Name     string `json:"name"`
	// RenderNodeID 可选：相机跟随 GLB 中的动态节点；为空保留原有世界坐标相机。
	RenderNodeID   string     `json:"render_node_id,omitempty"`
	Position       [3]float64 `json:"position"`
	QuaternionXYZW [4]float64 `json:"quaternion_xyzw"`
	FOVY           float64    `json:"fovy"`
}

// ViewerScene 是一次加载、持续更新位姿的 GLB 场景，不创建服务端 Viewer 会话。
type ViewerScene struct {
	Generation       int64               `json:"generation"`
	SceneRevision    string              `json:"scene_revision"`
	CoordinateFrame  string              `json:"coordinate_frame"`
	ContentURL       string              `json:"content_url"`
	PoseStreamURL    string              `json:"pose_stream_url"`
	DynamicNodeOrder []string            `json:"dynamic_node_order"`
	Cameras          []ViewerSceneCamera `json:"cameras"`
	DefaultCameraID  string              `json:"default_camera_id,omitempty"`
}

type SourceLink struct {
	MapID      string `json:"map_id"`
	Generation int64  `json:"generation"`
	SourceID   string `json:"source_id"`
	EntityID   string `json:"entity_id"`
}

type RobotCommandType = robotruntime.RobotCommandType

const (
	RobotCommandJointTrajectory = robotruntime.RobotCommandJointTrajectory
	RobotCommandBaseTrajectory  = robotruntime.RobotCommandBaseTrajectory
	RobotCommandGripper         = robotruntime.RobotCommandGripper
	RobotCommandStop            = robotruntime.RobotCommandStop
	RobotCommandHold            = robotruntime.RobotCommandHold
)

type RobotCapability = robotruntime.RobotCapability

// VirtualRobotDescriptor 直接复用 Robot Runtime 编排的唯一描述。Simulation
// 不再维护一份相似结构，从而避免场景返回的工具、SDK 和 backend profile 在
// 转换过程中被遗漏。
type VirtualRobotDescriptor = robotruntime.VirtualRobotDescriptor

// TrajectoryPoint 使用相对命令开始时间。旋转关节使用弧度，移动关节使用米。
type TrajectoryPoint struct {
	TimeFromStartSeconds float64            `json:"time_from_start_seconds"`
	Positions            map[string]float64 `json:"positions"`
	Velocities           map[string]float64 `json:"velocities,omitempty"`
}

type JointTrajectory struct {
	Resources []string          `json:"resources"`
	FrameID   string            `json:"frame_id"`
	Points    []TrajectoryPoint `json:"points"`
}

type BaseTrajectory struct {
	FrameID string            `json:"frame_id"`
	Points  []TrajectoryPoint `json:"points"`
}

type GripperCommand struct {
	GripperID string  `json:"gripper_id"`
	Position  float64 `json:"position"`
	MaxEffort float64 `json:"max_effort,omitempty"`
}

// RobotDebugCommand 只能携带 Type 对应的一个目标；Service 在转发前检查能力和 generation。
type RobotDebugCommand struct {
	CommandID       string           `json:"command_id"`
	SceneGeneration int64            `json:"scene_generation"`
	Type            RobotCommandType `json:"type"`
	TimeoutSeconds  float64          `json:"timeout_seconds,omitempty"`
	Joint           *JointTrajectory `json:"joint_trajectory,omitempty"`
	Base            *BaseTrajectory  `json:"base_trajectory,omitempty"`
	Gripper         *GripperCommand  `json:"gripper_command,omitempty"`
}

type RobotCommandStatus string

const (
	RobotCommandAccepted  RobotCommandStatus = "accepted"
	RobotCommandRunning   RobotCommandStatus = "running"
	RobotCommandSucceeded RobotCommandStatus = "succeeded"
	RobotCommandFailed    RobotCommandStatus = "failed"
	RobotCommandCancelled RobotCommandStatus = "cancelled"
	RobotCommandUnknown   RobotCommandStatus = "unknown"
)

type RobotCommandRecord struct {
	CommandID  string             `json:"command_id"`
	RobotID    string             `json:"robot_id"`
	Generation int64              `json:"scene_generation"`
	Type       RobotCommandType   `json:"type"`
	Status     RobotCommandStatus `json:"status"`
	Progress   float64            `json:"progress,omitempty"`
	Message    string             `json:"message,omitempty"`
	Details    json.RawMessage    `json:"details,omitempty"`
	UpdatedAt  time.Time          `json:"updated_at"`
}

// PoseSnapshot 是 Runtime、Framework 和 Studio 共用的 Robot 位姿。
// 公共单位固定为米，四元数顺序固定为 xyzw；Runtime 内部若使用 wxyz，必须在边界转换。
type PoseSnapshot struct {
	Position       [3]float64 `json:"position"`
	QuaternionXYZW [4]float64 `json:"quaternion_xyzw"`
	FrameID        string     `json:"frame_id"`
}

// JointStateSnapshot 保留调试 Robot 和后续 Ability 需要的实际关节反馈。
// effort 在 Runtime 无法提供时可以省略，不能用目标值冒充实际反馈。
type JointStateSnapshot struct {
	Position float64  `json:"position"`
	Velocity float64  `json:"velocity"`
	Effort   *float64 `json:"effort,omitempty"`
}

// RobotStateSnapshot 直接对应 Runtime 的 Robot 状态，不再套一层不透明 state。
// generation 属于场景实例；reset 后旧状态不能再用于生成控制命令。
type RobotStateSnapshot struct {
	RobotID       string                        `json:"robot_id"`
	Generation    int64                         `json:"generation"`
	ObservedAt    time.Time                     `json:"observed_at"`
	BasePose      *PoseSnapshot                 `json:"base_pose,omitempty"`
	Joints        map[string]JointStateSnapshot `json:"joints"`
	EndEffectors  map[string]PoseSnapshot       `json:"end_effectors"`
	Grippers      map[string]float64            `json:"grippers"`
	HoldingObject *string                       `json:"holding_object,omitempty"`
	InHold        bool                          `json:"in_hold"`
}

type SensorDescriptor struct {
	SensorID string `json:"sensor_id"`
	Kind     string `json:"kind"`
	FrameID  string `json:"frame_id"`
	Encoding string `json:"encoding,omitempty"`
}

// SceneSnapshot 是 Runtime 的引擎无关场景快照。
// RawMessage 是给未合并 Semantic Map 分支预留的接缝，不允许包含引擎内部标识。
type SceneSnapshot struct {
	SceneKey        string            `json:"scene_key"`
	InstanceID      string            `json:"instance_id"`
	Generation      int64             `json:"generation"`
	CoordinateFrame string            `json:"coordinate_frame"`
	Robots          []json.RawMessage `json:"robots"`
	Objects         []json.RawMessage `json:"objects"`
	Regions         []json.RawMessage `json:"regions"`
	Sensors         []json.RawMessage `json:"sensors"`
	ObservedAt      time.Time         `json:"observed_at"`
}

type ProjectSimulationSnapshot struct {
	ProjectID            string                   `json:"project_id"`
	Revision             int64                    `json:"revision"`
	RuntimeInstallation  *RuntimeInstallationView `json:"runtime_installation,omitempty"`
	Profiles             []RuntimeProfile         `json:"profiles"`
	Runtimes             []RuntimeInfo            `json:"runtimes"`
	Scenes               []SceneDescriptor        `json:"scenes"`
	Documents            []SceneDocument          `json:"scene_documents"`
	RuntimeBundles       []RuntimeBundle          `json:"runtime_bundles"`
	Instance             *SceneInstance           `json:"instance,omitempty"`
	Robots               []VirtualRobotDescriptor `json:"robots"`
	CatalogSceneID       string                   `json:"catalog_scene_id,omitempty"`
	SceneVersion         string                   `json:"scene_version,omitempty"`
	VariantID            string                   `json:"variant_id,omitempty"`
	EvaluationDescriptor *EvaluationDescriptor    `json:"evaluation_descriptor,omitempty"`
	Evaluation           *SceneEvaluation         `json:"evaluation,omitempty"`
	Recovery             string                   `json:"recovery,omitempty"`
	RecoveryInfo         *RecoveryInfo            `json:"recovery_info,omitempty"`
	UpdatedAt            time.Time                `json:"updated_at"`
}

// RecoveryInfo 描述 Framework 根据 Runtime 连通性核对出的“派生状态”。
//
// Instance 始终保留 Runtime 最后一次真实返回的状态；例如最后状态是 running，
// Runtime 离线后也不能把它改写成 failed。Studio 使用 DerivedState=interrupted
// 禁止新的控制请求，同时仍可向用户展示最后状态和本次诊断。
type RecoveryInfo struct {
	Code               string    `json:"code"`
	DerivedState       string    `json:"derived_state"`
	Message            string    `json:"message"`
	RuntimeProfileID   string    `json:"runtime_profile_id,omitempty"`
	InstanceID         string    `json:"instance_id,omitempty"`
	LastKnownState     string    `json:"last_known_state,omitempty"`
	LastKnownUpdatedAt time.Time `json:"last_known_updated_at,omitempty"`
	ObservedAt         time.Time `json:"observed_at"`
}

// SceneEvaluation 保留原生环境的评测结果，作为测试证据而不是 Task 或 Skill 终态。
// Metrics 只允许可序列化的环境信息，不得带出引擎对象、指针或宿主路径。
type SceneEvaluation struct {
	SceneKey         string         `json:"scene_key"`
	InstanceID       string         `json:"instance_id"`
	Generation       int64          `json:"generation"`
	RuntimeProfileID string         `json:"runtime_profile_id"`
	Reward           float64        `json:"reward"`
	Success          bool           `json:"success"`
	Terminated       bool           `json:"terminated"`
	Language         string         `json:"language,omitempty"`
	Metrics          map[string]any `json:"metrics"`
	ObservedAt       time.Time      `json:"observed_at"`
}
