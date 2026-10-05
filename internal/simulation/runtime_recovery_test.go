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
	"errors"
	"fmt"
	"testing"
	"time"
)

func readyRuntimeProfile(profileID, loader string, capabilities RuntimeCapability) RuntimeProfile {
	return RuntimeProfile{
		RuntimeProfileID: profileID, Name: profileID,
		Engine: "mujoco", Loader: loader, APIVersion: "v1",
		Capabilities: capabilities, SceneKinds: []string{"scene_document"},
		Environment: profileID, EnvironmentReady: true, Available: true,
	}
}

type recoveryRuntimeClient struct {
	*fakeRuntimeClient
	profiles      []RuntimeProfile
	profileCalls  int
	startCalls    int
	registered    map[string]bool
	registerReply *RuntimeBundleResult
}

func (c *recoveryRuntimeClient) RuntimeProfiles(context.Context) ([]RuntimeProfile, error) {
	c.profileCalls++
	return c.profiles, nil
}

func (c *recoveryRuntimeClient) RegisterRuntimeBundle(
	ctx context.Context, bundle RuntimeBundle,
) (RuntimeBundleResult, error) {
	if c.registerReply != nil {
		return *c.registerReply, nil
	}
	if c.registered == nil {
		c.registered = make(map[string]bool)
	}
	c.registered[bundle.RuntimeBundleID] = true
	return c.fakeRuntimeClient.RegisterRuntimeBundle(ctx, bundle)
}

func (c *recoveryRuntimeClient) StartScene(
	ctx context.Context, sceneKey string, request SceneStartRequest,
) (SceneInstance, error) {
	c.startCalls++
	if request.RuntimeBundleID != "" && !c.registered[request.RuntimeBundleID] {
		return SceneInstance{}, fmt.Errorf("bundle %s 尚未注册", request.RuntimeBundleID)
	}
	return c.fakeRuntimeClient.StartScene(ctx, sceneKey, request)
}

func TestRuntimeProbeUsesEndpointProfileCapabilities(t *testing.T) {
	actualCapabilities := RuntimeCapability{Viewer: true, SceneStep: true, SensorKinds: []string{"rgb"}}
	client := &recoveryRuntimeClient{
		fakeRuntimeClient: &fakeRuntimeClient{
			healthy: true, info: RuntimeInfo{State: "ready", Engine: "mujoco", APIVersion: "v1"},
		},
		profiles: []RuntimeProfile{readyRuntimeProfile("native-mujoco", "native", actualCapabilities)},
	}
	registry, err := NewRuntimeRegistry(RuntimeBinding{
		Profile: RuntimeProfile{RuntimeProfileID: "native-mujoco", Engine: "mujoco", Loader: "native", APIVersion: "v1"},
		Client:  client,
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := NewRuntimeSupervisor(registry).Probe(context.Background(), "native-mujoco")
	if err != nil {
		t.Fatal(err)
	}
	if client.profileCalls != 1 || !info.Capabilities.Viewer || len(info.Capabilities.SensorKinds) != 1 {
		t.Fatalf("未使用 endpoint 的真实能力: calls=%d info=%+v", client.profileCalls, info)
	}
	observed := registry.Profiles()
	if len(observed) != 1 || !observed[0].Available || !observed[0].EnvironmentReady || !observed[0].AvailabilityKnown {
		t.Fatalf("Registry 未保存真实 Profile: %+v", observed)
	}
}

func TestRuntimeProbeRejectsUnavailableOrMismatchedProfile(t *testing.T) {
	expected := RuntimeProfile{RuntimeProfileID: "native-mujoco", Engine: "mujoco", Loader: "native", APIVersion: "v1"}
	unavailable := readyRuntimeProfile("native-mujoco", "native", RuntimeCapability{})
	unavailable.Available = false
	cases := []struct {
		name     string
		profiles []RuntimeProfile
	}{
		{name: "missing", profiles: nil},
		{name: "unavailable", profiles: []RuntimeProfile{unavailable}},
		{name: "wrong-loader", profiles: []RuntimeProfile{readyRuntimeProfile("native-mujoco", "libero", RuntimeCapability{})}},
		{name: "wrong-api", profiles: []RuntimeProfile{{RuntimeProfileID: "native-mujoco", Engine: "mujoco", Loader: "native", APIVersion: "v2", EnvironmentReady: true, Available: true}}},
	}
	for _, test := range cases {
		test := test
		t.Run(test.name, func(t *testing.T) {
			client := &recoveryRuntimeClient{
				fakeRuntimeClient: &fakeRuntimeClient{
					healthy: true, info: RuntimeInfo{State: "ready", Engine: "mujoco", APIVersion: "v1"},
				},
				profiles: test.profiles,
			}
			_, _, err := probeRuntime(context.Background(), RuntimeBinding{Profile: expected, Client: client})
			if !errors.Is(err, ErrRuntimeUnavailable) {
				t.Fatalf("错误 Profile 应被拒绝，实际: %v", err)
			}
		})
	}
}

func saveRuntimeBundle(t *testing.T, store *FileStore) RuntimeBundle {
	t.Helper()
	bundle := RuntimeBundle{
		RuntimeBundleID: "runtime-bundle-recovery", DocumentID: "document-1",
		Revision: 1, SceneVersion: 1, SceneKey: "scene-recovery",
		RuntimeProfileID: "native-mujoco",
		Document:         RuntimeSceneDocument{ID: "document-1", SceneKind: "scene_document"},
		Validation:       ValidationResult{Valid: true},
	}
	if err := store.SaveRuntimeBundle("project-1", bundle); err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestStartSceneRestoresBundleAndReleasesManagedRuntimeWithProject(t *testing.T) {
	store := NewFileStore(fixedWorkspace{root: t.TempDir()})
	bundle := saveRuntimeBundle(t, store)
	base := &fakeRuntimeClient{
		info: RuntimeInfo{State: "ready", Engine: "mujoco", APIVersion: "v1"},
	}
	client := &recoveryRuntimeClient{
		fakeRuntimeClient: base,
		profiles: []RuntimeProfile{readyRuntimeProfile(
			"native-mujoco", "native", RuntimeCapability{EditableScene: true},
		)},
	}
	process := &fakeProcess{}
	launcher := &fakeLauncher{client: base, process: process}
	service := NewService(client, launcher, store)
	service.supervisor.startTimeout = 200 * time.Millisecond

	instance, err := service.StartScene(context.Background(), "project-1", bundle.SceneKey,
		SceneStartRequest{RequestID: "request-recovery", RuntimeBundleID: bundle.RuntimeBundleID})
	if err != nil {
		t.Fatal(err)
	}
	if client.startCalls != 1 || len(base.builds) != 1 ||
		!client.registered[bundle.RuntimeBundleID] {
		t.Fatalf("RuntimeBundle 未在启动前恢复: builds=%d starts=%d",
			len(base.builds), client.startCalls)
	}
	if _, err := service.SceneOperation(
		context.Background(), "project-1", instance.InstanceID, "stop", nil,
	); err != nil {
		t.Fatal(err)
	}
	if process.stopped || service.supervisor.slots["native-mujoco"].process == nil {
		t.Fatalf("停止场景不应回收 Project 后续仍会复用的 Runtime: process=%+v", process)
	}
	state, err := store.LoadRuntimeState("project-1")
	if err != nil || state.InstanceID != "" || state.LastInstance == nil ||
		state.LastInstance.State != "stopped" {
		t.Fatalf("stop 终态没有先持久化: state=%+v err=%v", state, err)
	}
	if err := service.ReleaseProject(context.Background(), "project-1"); err != nil {
		t.Fatal(err)
	}
	if !process.stopped || service.supervisor.slots["native-mujoco"].process != nil {
		t.Fatalf("退出 Project 后受管 Runtime 未回收: process=%+v", process)
	}
}

type failingRuntimeStateStore struct {
	*memoryRuntimeStateStore
	fail bool
}

func (s *failingRuntimeStateStore) SaveRuntimeState(value ProjectRuntimeState) error {
	if s.fail {
		return errors.New("injected state save failure")
	}
	return s.memoryRuntimeStateStore.SaveRuntimeState(value)
}

func TestStopSceneDoesNotCloseExternalRuntime(t *testing.T) {
	base := &fakeRuntimeClient{
		healthy: true,
		info:    RuntimeInfo{State: "ready", Engine: "mujoco", APIVersion: "v1"},
	}
	client := &recoveryRuntimeClient{
		fakeRuntimeClient: base,
		profiles: []RuntimeProfile{readyRuntimeProfile(
			"native-mujoco", "native", RuntimeCapability{},
		)},
	}
	service := NewService(client, nil,
		&memoryRuntimeStateStore{values: make(map[string]ProjectRuntimeState)})
	instance, err := service.StartScene(context.Background(), "project-1", "scene",
		SceneStartRequest{RequestID: "external-runtime"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SceneOperation(
		context.Background(), "project-1", instance.InstanceID, "stop", nil,
	); err != nil {
		t.Fatal(err)
	}
	managed, err := service.supervisor.StopManaged(context.Background(), "native-mujoco")
	if err != nil || managed || !base.healthy {
		t.Fatalf("外部 Runtime 不应被关闭: managed=%v healthy=%v err=%v",
			managed, base.healthy, err)
	}
}

func TestStopSceneKeepsManagedProcessWhenStateSaveFails(t *testing.T) {
	base := &fakeRuntimeClient{
		info: RuntimeInfo{State: "ready", Engine: "mujoco", APIVersion: "v1"},
	}
	client := &recoveryRuntimeClient{
		fakeRuntimeClient: base,
		profiles: []RuntimeProfile{readyRuntimeProfile(
			"native-mujoco", "native", RuntimeCapability{},
		)},
	}
	process := &fakeProcess{}
	state := &failingRuntimeStateStore{
		memoryRuntimeStateStore: &memoryRuntimeStateStore{
			values: make(map[string]ProjectRuntimeState),
		},
	}
	service := NewService(client, &fakeLauncher{client: base, process: process}, state)
	service.supervisor.startTimeout = 200 * time.Millisecond
	instance, err := service.StartScene(context.Background(), "project-1", "scene",
		SceneStartRequest{RequestID: "managed-save-failure"})
	if err != nil {
		t.Fatal(err)
	}
	state.fail = true
	if _, err := service.SceneOperation(
		context.Background(), "project-1", instance.InstanceID, "stop", nil,
	); err == nil {
		t.Fatal("状态保存失败必须返回错误")
	}
	if process.stopped || service.supervisor.slots["native-mujoco"].process != process {
		t.Fatalf("状态未保存时不应丢失受管进程句柄: process=%+v", process)
	}
}

func TestStartSceneRejectsInvalidBundleRegistrationResult(t *testing.T) {
	store := NewFileStore(fixedWorkspace{root: t.TempDir()})
	bundle := saveRuntimeBundle(t, store)
	base := &fakeRuntimeClient{
		healthy: true,
		info:    RuntimeInfo{State: "ready", Engine: "mujoco", APIVersion: "v1"},
	}
	invalid := RuntimeBundleResult{
		RuntimeBundleID:  bundle.RuntimeBundleID,
		SceneKey:         bundle.SceneKey,
		RuntimeProfileID: bundle.RuntimeProfileID,
		Valid:            true,
	}
	client := &recoveryRuntimeClient{
		fakeRuntimeClient: base,
		profiles: []RuntimeProfile{readyRuntimeProfile(
			"native-mujoco", "native", RuntimeCapability{EditableScene: true},
		)},
		registerReply: &invalid,
	}
	service := NewService(client, nil, store)
	_, err := service.StartScene(context.Background(), "project-1", bundle.SceneKey,
		SceneStartRequest{RequestID: "invalid-registration", RuntimeBundleID: bundle.RuntimeBundleID})
	if !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("不完整的 Bundle 注册结果应被拒绝: %v", err)
	}
	if client.startCalls != 0 {
		t.Fatalf("Bundle 恢复失败后不应启动场景: starts=%d", client.startCalls)
	}
}

type crashedRuntimeProcess struct {
	stopCalled bool
}

func (*crashedRuntimeProcess) Alive() bool { return false }
func (p *crashedRuntimeProcess) Stop(context.Context) error {
	p.stopCalled = true
	return errors.New("already crashed")
}

func TestStopManagedClearsCrashedRuntimeWithoutStoppingItAgain(t *testing.T) {
	base := &fakeRuntimeClient{
		healthy: true,
		info:    RuntimeInfo{State: "ready", Engine: "mujoco", APIVersion: "v1"},
	}
	service := NewService(base, nil,
		&memoryRuntimeStateStore{values: make(map[string]ProjectRuntimeState)})
	process := &crashedRuntimeProcess{}
	service.supervisor.slots["native-mujoco"].process = process

	managed, err := service.supervisor.StopManaged(context.Background(), "native-mujoco")
	if err != nil || !managed {
		t.Fatalf("异常退出的受管 Runtime 应可清理: managed=%v err=%v", managed, err)
	}
	if process.stopCalled || service.supervisor.slots["native-mujoco"].process != nil {
		t.Fatalf("不应再次停止已退出进程: process=%+v", process)
	}
}

func (c *recoveryRuntimeClient) SceneOperation(
	ctx context.Context, instanceID, operation string, body any,
) (SceneInstance, error) {
	// 真实 HTTP Runtime 离线时不会伪造 stop 成功。这个 Fake 保持相同边界，
	// 用于覆盖崩溃后由 Framework 清理实例槽并拉起全新进程的恢复路径。
	if !c.healthy {
		return SceneInstance{}, ErrRuntimeUnavailable
	}
	return c.fakeRuntimeClient.SceneOperation(ctx, instanceID, operation, body)
}

type restartingRuntimeLauncher struct {
	client  *fakeRuntimeClient
	process *fakeProcess
	starts  int
}

func (l *restartingRuntimeLauncher) Start(context.Context) (RuntimeProcess, error) {
	l.starts++
	// 新进程不能继承已崩溃进程的活动实例。这里模拟 Runtime 的真实进程边界，
	// 避免测试通过一个仍携带旧 instance_id 的内存客户端。
	l.client.healthy = true
	l.client.info.ActiveInstanceID = ""
	l.client.instance = SceneInstance{}
	l.process.alive = true
	l.process.stopped = false
	return l.process, nil
}

func TestRecoverInterruptedManagedRuntimeAllowsStartingAnotherScene(t *testing.T) {
	base := &fakeRuntimeClient{
		info: RuntimeInfo{State: "ready", Engine: "mujoco", APIVersion: "v1"},
	}
	profile := readyRuntimeProfile("native-mujoco", "native", RuntimeCapability{Viewer: true})
	client := &recoveryRuntimeClient{
		fakeRuntimeClient: base,
		profiles:          []RuntimeProfile{profile},
	}
	process := &fakeProcess{}
	launcher := &restartingRuntimeLauncher{client: base, process: process}
	state := &memoryRuntimeStateStore{values: make(map[string]ProjectRuntimeState)}
	registry, err := NewRuntimeRegistry(RuntimeBinding{
		InstallationID: "native-local", Profile: profile, Client: client, Launcher: launcher,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewServiceWithRegistry(registry, state, nil)
	service.ConfigureProjectResources(
		&RuntimeInstallationCatalog{
			items: map[string]RuntimeInstallation{
				"native-local": {
					SchemaVersion:  runtimeInstallationSchemaVersion,
					InstallationID: "native-local", Profile: profile,
					LaunchMode: "process", Enabled: true,
				},
			},
			order: []string{"native-local"},
		}, nil, fixedRuntimeBinding("native-local"),
	)
	service.supervisor.startTimeout = 200 * time.Millisecond

	first, err := service.StartScene(
		context.Background(), "project-crash", "scene-before-crash",
		SceneStartRequest{RequestID: "before-crash"},
	)
	if err != nil {
		t.Fatal(err)
	}
	process.alive = false
	base.healthy = false

	info, err := service.RecoverInterruptedProject(
		context.Background(), "project-crash", first.InstanceID,
	)
	if err != nil {
		t.Fatalf("清理崩溃 Runtime 并重新拉起失败: %v", err)
	}
	if info.State != "ready" || launcher.starts != 2 || !process.alive || process.stopped {
		t.Fatalf("Runtime 未以新进程恢复: info=%+v starts=%d process=%+v",
			info, launcher.starts, process)
	}
	persisted, err := state.LoadRuntimeState("project-crash")
	if err != nil || persisted.InstanceID != "" || persisted.LastInstance == nil ||
		persisted.LastInstance.State != "interrupted" {
		t.Fatalf("崩溃实例槽没有释放并保留诊断: state=%+v err=%v", persisted, err)
	}

	next, err := service.StartScene(
		context.Background(), "project-crash", "scene-after-recovery",
		SceneStartRequest{RequestID: "after-recovery"},
	)
	if err != nil || next.State != "running" || next.SceneKey != "scene-after-recovery" {
		t.Fatalf("恢复后仍不能启动新场景: instance=%+v err=%v", next, err)
	}
}
