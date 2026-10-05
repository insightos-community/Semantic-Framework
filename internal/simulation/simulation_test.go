// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package simulation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestNormalizeExitAcceptsRuntimeSignalExitCodes(t *testing.T) {
	for _, code := range []string{"130", "143"} {
		err := exec.Command("sh", "-c", "exit "+code).Run()
		if normalized := normalizeExit(err); normalized != nil {
			t.Fatalf("受控退出码 %s 被误判为失败: %v", code, normalized)
		}
	}
}

type memoryRuntimeStateStore struct {
	values map[string]ProjectRuntimeState
}

func (s *memoryRuntimeStateStore) LoadRuntimeState(projectID string) (ProjectRuntimeState, error) {
	value, ok := s.values[projectID]
	if !ok {
		return ProjectRuntimeState{}, ErrNotFound
	}
	return value, nil
}

func (s *memoryRuntimeStateStore) SaveRuntimeState(value ProjectRuntimeState) error {
	s.values[value.ProjectID] = value
	return nil
}

type fakeRuntimeClient struct {
	healthy         bool
	info            RuntimeInfo
	instance        SceneInstance
	calls           []string
	builds          []RuntimeBundle
	robotReads      int
	evaluationReads int
}

func (f *fakeRuntimeClient) Health(context.Context) error {
	if !f.healthy {
		return ErrRuntimeUnavailable
	}
	return nil
}

func (f *fakeRuntimeClient) Runtime(context.Context) (RuntimeInfo, error) {
	info := f.info
	if info.Engine == "" {
		info.Engine = "mujoco"
	}
	if info.APIVersion == "" {
		info.APIVersion = "v1"
	}
	return info, nil
}

func (f *fakeRuntimeClient) RuntimeProfiles(context.Context) ([]RuntimeProfile, error) {
	capabilities := RuntimeCapability{EditableScene: true, NativeEvaluator: true, Viewer: true, SceneStep: true, SceneReset: true}
	return []RuntimeProfile{
		{RuntimeProfileID: "native-mujoco", Engine: "mujoco", Loader: "native", APIVersion: "v1", SceneKinds: []string{"scene_document", "asset_scene"}, Capabilities: capabilities, EnvironmentReady: true, Available: true},
		{RuntimeProfileID: "robosuite-1.5", Engine: "mujoco", Loader: "robosuite", APIVersion: "v1", SceneKinds: []string{"robosuite"}, Capabilities: capabilities, EnvironmentReady: true, Available: true},
		{RuntimeProfileID: "libero-robosuite-1.4", Engine: "mujoco", Loader: "libero", APIVersion: "v1", SceneKinds: []string{"libero", "libero_pro"}, Capabilities: capabilities, EnvironmentReady: true, Available: true},
	}, nil
}

func (f *fakeRuntimeClient) ListScenes(context.Context) ([]SceneDescriptor, error) {
	return []SceneDescriptor{{SceneKey: "depalletizing", Layouts: []string{"layout001"}}}, nil
}

func (f *fakeRuntimeClient) RegisterRuntimeBundle(
	_ context.Context, bundle RuntimeBundle,
) (RuntimeBundleResult, error) {
	f.builds = append(f.builds, bundle)
	descriptor := &SceneDescriptor{
		SceneKey: bundle.SceneKey, SceneKind: "scene_document",
		Layouts: []string{"version-1"}, ReadOnly: false,
	}
	return RuntimeBundleResult{
		RuntimeBundleID: bundle.RuntimeBundleID,
		SceneKey:        bundle.SceneKey, RuntimeProfileID: bundle.RuntimeProfileID,
		RuntimeSceneKey: "version-1", Valid: true, Descriptor: descriptor,
	}, nil
}

func (f *fakeRuntimeClient) StartScene(
	_ context.Context,
	sceneKey string,
	request SceneStartRequest,
) (SceneInstance, error) {
	if f.info.ActiveInstanceID != "" && request.RequestID != f.instance.RequestID {
		return SceneInstance{}, ErrConflict
	}
	f.instance = SceneInstance{
		InstanceID: "instance-1", SceneKey: sceneKey, Layout: request.Layout,
		RequestID: request.RequestID, Generation: 1, State: "running",
	}
	f.info.ActiveInstanceID = f.instance.InstanceID
	return f.instance, nil
}

func (f *fakeRuntimeClient) Scene(context.Context, string) (SceneInstance, error) {
	return f.instance, nil
}

func (f *fakeRuntimeClient) SceneOperation(
	_ context.Context,
	_ string,
	operation string,
	_ any,
) (SceneInstance, error) {
	f.calls = append(f.calls, operation)
	switch operation {
	case "pause":
		f.instance.State = "paused"
	case "resume":
		f.instance.State = "running"
	case "step":
		f.instance.StepCount++
	case "reset":
		f.instance.Generation++
		f.instance.State = "running"
	case "stop":
		f.instance.State = "stopped"
		f.info.ActiveInstanceID = ""
	}
	return f.instance, nil
}

func (f *fakeRuntimeClient) SceneSnapshot(context.Context, string) (SceneSnapshot, error) {
	return SceneSnapshot{
		SceneKey: f.instance.SceneKey, InstanceID: f.instance.InstanceID,
		Generation: f.instance.Generation, CoordinateFrame: "world",
	}, nil
}

func (f *fakeRuntimeClient) SceneEvaluation(context.Context, string) (SceneEvaluation, error) {
	f.evaluationReads++
	return SceneEvaluation{
		SceneKey: f.instance.SceneKey, InstanceID: f.instance.InstanceID,
		Generation: f.instance.Generation, Reward: 1.25, Success: true,
	}, nil
}

func (f *fakeRuntimeClient) Robots(context.Context, string) ([]VirtualRobotDescriptor, error) {
	f.robotReads++
	return []VirtualRobotDescriptor{{
		RobotID: "r1", Model: "r1pro", SDKPackage: "robot-sdk-r1pro",
		BackendProfile: "mujoco", Capabilities: RobotCapability{
			Commands: []RobotCommandType{
				RobotCommandJointTrajectory, RobotCommandBaseTrajectory, RobotCommandGripper,
			},
			Sensors: []string{"rgb", "depth"},
		},
	}}, nil
}

func (f *fakeRuntimeClient) ViewerScene(
	_ context.Context, instanceID string,
) (ViewerScene, error) {
	return ViewerScene{
		Generation:       f.instance.Generation,
		SceneRevision:    "scene-revision-1",
		CoordinateFrame:  "world",
		ContentURL:       "/api/v1/scene-instances/" + instanceID + "/viewer-scene/content",
		PoseStreamURL:    "/api/v1/scene-instances/" + instanceID + "/pose-stream",
		DynamicNodeOrder: []string{"node-000000"},
	}, nil
}

func (f *fakeRuntimeClient) ViewerSceneContent(
	context.Context, string,
) ([]byte, string, error) {
	return []byte("glTF00000000"), "model/gltf-binary", nil
}

func (f *fakeRuntimeClient) ScenePoseURL(instanceID string) string {
	return "ws://runtime.test/scenes/" + instanceID + "/pose-stream"
}
func (f *fakeRuntimeClient) SensorFramesURL(robotID, sensorID string) string {
	return "ws://runtime.test/robots/" + robotID + "/sensors/" + sensorID
}

func (f *fakeRuntimeClient) RobotState(context.Context, string) (RobotStateSnapshot, error) {
	return RobotStateSnapshot{RobotID: "r1", Generation: f.instance.Generation}, nil
}
func (f *fakeRuntimeClient) RobotSensors(context.Context, string) ([]SensorDescriptor, error) {
	return []SensorDescriptor{{SensorID: "camera", Kind: "rgb", FrameID: "camera"}}, nil
}
func (f *fakeRuntimeClient) SubmitRobotCommand(
	_ context.Context, robotID string, command RobotDebugCommand,
) (RobotCommandRecord, error) {
	return RobotCommandRecord{CommandID: command.CommandID, RobotID: robotID,
		Generation: command.SceneGeneration, Type: command.Type, Status: RobotCommandAccepted}, nil
}
func (f *fakeRuntimeClient) RobotCommand(
	context.Context, string, string,
) (RobotCommandRecord, error) {
	return RobotCommandRecord{CommandID: "command-1", Status: RobotCommandRunning}, nil
}
func (f *fakeRuntimeClient) StopRobotCommand(
	context.Context, string, string,
) (RobotCommandRecord, error) {
	return RobotCommandRecord{CommandID: "command-1", Status: RobotCommandCancelled}, nil
}
func (f *fakeRuntimeClient) HoldRobot(
	_ context.Context, robotID string, generation int64,
) (RobotCommandRecord, error) {
	f.calls = append(f.calls, "hold")
	return RobotCommandRecord{CommandID: "hold-1", RobotID: robotID,
		Generation: generation, Type: RobotCommandHold, Status: RobotCommandSucceeded}, nil
}

type fakeProcess struct {
	alive   bool
	stopped bool
}

func (p *fakeProcess) Alive() bool { return p.alive }
func (p *fakeProcess) Stop(context.Context) error {
	p.stopped = true
	p.alive = false
	return nil
}

type fakeLauncher struct {
	client  *fakeRuntimeClient
	process *fakeProcess
	starts  int
}

func (l *fakeLauncher) Start(context.Context) (RuntimeProcess, error) {
	l.starts++
	l.client.healthy = true
	l.process.alive = true
	return l.process, nil
}

func TestServiceStartsRuntimeAndRestoresProjectSnapshot(t *testing.T) {
	client := &fakeRuntimeClient{
		info: RuntimeInfo{State: "ready", Engine: "mujoco"},
	}
	state := &memoryRuntimeStateStore{values: make(map[string]ProjectRuntimeState)}
	launcher := &fakeLauncher{client: client, process: &fakeProcess{}}
	service := NewService(client, launcher, state)
	service.supervisor.startTimeout = time.Second

	ctx := context.Background()
	instance, err := service.StartScene(ctx, "project-1", "depalletizing", SceneStartRequest{
		RequestID: "request-1", Layout: "layout001", Headless: true,
	})
	if err != nil {
		t.Fatalf("启动场景失败: %v", err)
	}
	if launcher.starts != 1 || instance.InstanceID == "" {
		t.Fatalf("Runtime 未按需启动: starts=%d instance=%+v", launcher.starts, instance)
	}
	viewerScene, err := service.ViewerScene(ctx, "project-1", instance.InstanceID)
	if err != nil {
		t.Fatalf("读取 Viewer Scene 失败: %v", err)
	}
	if viewerScene.Generation != 1 || len(viewerScene.DynamicNodeOrder) != 1 {
		t.Fatalf("Viewer Scene 描述错误: %+v", viewerScene)
	}
	if strings.Contains(viewerScene.ContentURL, "runtime.test") ||
		strings.Contains(viewerScene.PoseStreamURL, "runtime.test") {
		t.Fatalf("Viewer Scene 暴露 Runtime 地址: %+v", viewerScene)
	}
	content, mediaType, err := service.ViewerSceneContent(
		ctx, "project-1", instance.InstanceID,
	)
	if err != nil || mediaType != "model/gltf-binary" ||
		!strings.HasPrefix(string(content), "glTF") {
		t.Fatalf("Viewer Scene 内容错误: media=%s err=%v", mediaType, err)
	}
	if poseURL, err := service.ScenePoseURL("project-1", instance.InstanceID); err != nil || !strings.Contains(poseURL, "/pose-stream") {
		t.Fatalf("Scene Pose URL 错误: %s err=%v", poseURL, err)
	}
	if _, err := service.SceneOperation(
		ctx, "project-1", instance.InstanceID, "pause", nil,
	); err != nil {
		t.Fatalf("暂停场景失败: %v", err)
	}
	if _, err := service.SceneOperation(
		ctx, "project-1", instance.InstanceID, "step", map[string]int{"steps": 1},
	); err != nil {
		t.Fatalf("单步失败: %v", err)
	}

	snapshot, err := service.Snapshot(ctx, "project-1")
	if err != nil {
		t.Fatalf("读取 Studio Snapshot 失败: %v", err)
	}
	if snapshot.Instance == nil || snapshot.Instance.State != "paused" {
		t.Fatalf("场景状态没有恢复: %+v", snapshot.Instance)
	}
	reset, err := service.SceneOperation(ctx, "project-1", instance.InstanceID, "reset", nil)
	if err != nil {
		t.Fatalf("重置场景失败: %v", err)
	}
	if reset.Generation != 2 {
		t.Fatalf("reset 未推进 generation: %+v", reset)
	}
	if _, err := service.SceneOperation(
		ctx, "project-1", instance.InstanceID, "stop", nil,
	); err != nil {
		t.Fatalf("停止场景失败: %v", err)
	}
	stoppedSnapshot, err := service.Snapshot(ctx, "project-1")
	if err != nil {
		t.Fatalf("停止后读取 Snapshot 失败: %v", err)
	}
	if stoppedSnapshot.Instance != nil {
		t.Fatalf("停止历史不应占用活动实例: %+v", stoppedSnapshot.Instance)
	}
}

func TestSceneStartAndSnapshotRefreshRuntimeInstallationObservation(t *testing.T) {
	client := &fakeRuntimeClient{
		info: RuntimeInfo{State: "ready", Engine: "mujoco"},
	}
	service := NewService(client, &fakeLauncher{client: client, process: &fakeProcess{}},
		&memoryRuntimeStateStore{values: make(map[string]ProjectRuntimeState)})
	service.supervisor.startTimeout = time.Second
	service.installations = &RuntimeInstallationCatalog{
		items: map[string]RuntimeInstallation{
			"native-mujoco": {
				InstallationID: "native-mujoco", Enabled: true, Status: "offline",
			},
		},
		order: []string{"native-mujoco"},
	}

	if _, err := service.StartScene(context.Background(), "project-1", "depalletizing",
		SceneStartRequest{RequestID: "request-observation", Layout: "layout001"}); err != nil {
		t.Fatal(err)
	}
	view, err := service.installations.View("native-mujoco")
	if err != nil || view.Status != "ready" {
		t.Fatalf("场景启动后安装状态没有同步为 ready: view=%+v err=%v", view, err)
	}

	service.installations.ObserveStatus("native-mujoco", "offline", "旧观测")
	if _, err := service.Snapshot(context.Background(), "project-1"); err != nil {
		t.Fatal(err)
	}
	view, err = service.installations.View("native-mujoco")
	if err != nil || view.Status != "ready" || view.Diagnostic != "" {
		t.Fatalf("正常快照没有刷新安装状态: view=%+v err=%v", view, err)
	}
}

func TestServiceShutdownStopsManagedRuntime(t *testing.T) {
	client := &fakeRuntimeClient{info: RuntimeInfo{State: "ready", Engine: "mujoco"}}
	process := &fakeProcess{}
	service := NewService(client, &fakeLauncher{client: client, process: process},
		&memoryRuntimeStateStore{values: make(map[string]ProjectRuntimeState)})
	service.supervisor.startTimeout = time.Second
	if _, err := service.EnsureRuntime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !process.stopped || process.alive {
		t.Fatalf("Server退出没有回收自己启动的Runtime: %+v", process)
	}
}

func TestServicePreventsTwoProjectsUsingOneRuntime(t *testing.T) {
	client := &fakeRuntimeClient{
		healthy: true, info: RuntimeInfo{State: "ready", Engine: "mujoco"},
	}
	state := &memoryRuntimeStateStore{values: make(map[string]ProjectRuntimeState)}
	service := NewService(client, nil, state)
	if _, err := service.StartScene(context.Background(), "project-1", "scene",
		SceneStartRequest{RequestID: "request-1", Layout: "layout001"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartScene(context.Background(), "project-2", "scene",
		SceneStartRequest{RequestID: "request-2", Layout: "layout001"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("第二个 Project 应冲突，实际: %v", err)
	}
}

func TestServiceRegistersRuntimeBundleWithRuntimeValidation(t *testing.T) {
	client := &fakeRuntimeClient{
		healthy: true, info: RuntimeInfo{State: "ready", Engine: "mujoco"},
	}
	service := NewService(client, nil,
		&memoryRuntimeStateStore{values: make(map[string]ProjectRuntimeState)})
	document := SceneDocument{
		ID: "scene-doc-1", ProjectID: "project-1", Name: "草稿",
		SceneKind: "scene_document",
	}
	bundle := RuntimeBundle{
		RuntimeBundleID: "runtime-bundle-1", SceneKey: document.ID,
		RuntimeProfileID: "native-mujoco", SceneVersion: 2, Document: NewRuntimeSceneDocument(document),
	}
	result, err := service.RegisterRuntimeBundle(context.Background(), bundle)
	if err != nil {
		t.Fatalf("注册 RuntimeBundle 失败: %v", err)
	}
	if result.SceneKey != document.ID || result.RuntimeSceneKey != "version-1" {
		t.Fatalf("Runtime 构建结果错误: %+v", result)
	}
	if len(client.builds) != 1 || client.builds[0].SceneVersion != 2 {
		t.Fatalf("Runtime 没有收到完整构建输入: %+v", client.builds)
	}
}

type fixedWorkspace struct{ root string }

func (w fixedWorkspace) ProjectWorkspace(string) (string, error) { return w.root, nil }

func TestSceneAuthoringValidatesBuildsAndForksPublishedVersion(t *testing.T) {
	store := NewFileStore(fixedWorkspace{root: t.TempDir()})
	authoring := NewSceneAuthoringService(store)
	document, err := authoring.Create("project-1", "空场景")
	if err != nil {
		t.Fatal(err)
	}
	robotCatalog := authoring.AssetCatalog()[0]
	document.Assets = []SceneAsset{robotCatalog.Asset}
	document.Nodes = []SceneNode{{
		ID: "robot-1", Name: "R1 Pro", Kind: "robot", AssetID: robotCatalog.Asset.ID,
		Properties: robotCatalog.DefaultProperties,
		Transform: Transform{
			QuaternionXYZW: [4]float64{0, 0, 0, 1},
			Scale:          [3]float64{1, 1, 1},
		},
	}}
	document, err = authoring.Update("project-1", document.ID, document.Revision, document)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntimeBundle("project-1", RuntimeBundle{
		RuntimeBundleID: "runtime-bundle-test", DocumentID: document.ID,
		Revision: document.Revision, SceneVersion: 1,
		RuntimeProfileID: "native-mujoco", Document: NewRuntimeSceneDocument(document),
	}); err != nil {
		t.Fatalf("保存 RuntimeBundle 失败: %v", err)
	}
	published, err := authoring.Publish("project-1", document.ID, document.Revision)
	if err != nil {
		t.Fatalf("发布场景失败: %v", err)
	}
	if published.Status != "published" || published.Version != 1 {
		t.Fatalf("发布状态不正确: %+v", published)
	}
	if _, err := authoring.Update("project-1", document.ID, published.Revision, published); !errors.Is(err, ErrConflict) {
		t.Fatalf("已发布版本不应可覆盖: %v", err)
	}
	forked, err := authoring.ForkPublished("project-1", document.ID, "新草稿")
	if err != nil || forked.Status != "draft" || forked.ID == document.ID {
		t.Fatalf("创建新草稿失败: document=%+v err=%v", forked, err)
	}
}

func TestSceneAuthoringRejectsHostPathAndBrokenReference(t *testing.T) {
	store := NewFileStore(fixedWorkspace{root: t.TempDir()})
	authoring := NewSceneAuthoringService(store)
	document, _ := authoring.Create("project-1", "非法场景")
	document.Assets = []SceneAsset{{ID: "asset-1", AssetKey: "/etc/passwd", Kind: "mesh"}}
	document.Nodes = []SceneNode{{
		ID: "box-1", Name: "Box", Kind: "object", AssetID: "missing",
		Transform: Transform{},
	}}
	result := authoring.Validate(document)
	if result.Valid || len(result.Issues) < 3 {
		t.Fatalf("非法场景未被完整拒绝: %+v", result)
	}
}

func TestStudioSnapshotHidesRuntimeAddresses(t *testing.T) {
	payload, err := json.Marshal(ProjectSimulationSnapshot{
		Runtimes: []RuntimeInfo{{Endpoint: "http://runtime", AssetRoot: "/srv/assets"}},
		Robots:   []VirtualRobotDescriptor{{RobotID: "r1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	if strings.Contains(text, "http://runtime") || strings.Contains(text, "/srv/assets") {
		t.Fatalf("Studio Snapshot 暴露了 Plugin 地址或资产路径: %s", text)
	}
}

func TestHTTPRuntimeClientOmitsFrameworkInstallationIdentity(t *testing.T) {
	var received map[string]any
	client := NewHTTPRuntimeClient("https://runtime.example", &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
				t.Fatal(err)
			}
			return &http.Response{
				StatusCode: http.StatusCreated,
				Status:     "201 Created",
				Body: io.NopCloser(strings.NewReader(
					`{"instance_id":"instance-1","runtime_profile_id":"native-mujoco",` +
						`"scene_key":"scene","layout":"layout001","generation":1,` +
						`"state":"starting","request_id":"request-1",` +
						`"created_at":"2026-08-18T00:00:00Z","updated_at":"2026-08-18T00:00:00Z"}`)),
				Header: make(http.Header),
			}, nil
		}),
	})

	_, err := client.StartScene(context.Background(), "scene", SceneStartRequest{
		RequestID: "request-1", RuntimeProfileID: "native-mujoco",
		RuntimeInstallationID: "dev-native-mujoco", Layout: "layout001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := received["runtime_installation_id"]; exists {
		t.Fatalf("Framework 私有 installation identity 泄漏到 Runtime: %#v", received)
	}
	if received["runtime_profile_id"] != "native-mujoco" {
		t.Fatalf("Runtime profile 应继续透传，实际: %#v", received)
	}
	if received["render_backend"] != "auto" {
		t.Fatalf("空渲染后端应使用 Runtime 正式默认值 auto，实际: %#v", received)
	}
}

func TestHTTPRuntimeClientMapsConflictAndBuildsPoseURL(t *testing.T) {
	client := NewHTTPRuntimeClient("https://runtime.example", &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusConflict,
				Status:     "409 Conflict",
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"busy"}}`)),
				Header:     make(http.Header),
			}, nil
		}),
	})
	_, err := client.StartScene(context.Background(), "scene", SceneStartRequest{})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Runtime 参数拒绝应映射为 ErrConflict，实际: %v", err)
	}
	if got := client.ScenePoseURL("instance-1"); got != "wss://runtime.example/api/v1/scene-instances/instance-1/pose-stream" {
		t.Fatalf("Scene Pose URL 不正确: %s", got)
	}
}

// 小型接口适配避免为一个 HTTP 错误测试引入第三方 mock。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
