package robotruntime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

type memoryRuntimeStore struct {
	mu    sync.Mutex
	items map[string]RuntimeInstance
}

func newMemoryRuntimeStore() *memoryRuntimeStore {
	return &memoryRuntimeStore{items: make(map[string]RuntimeInstance)}
}

func (s *memoryRuntimeStore) SaveRuntimeInstance(_ context.Context, item RuntimeInstance) error {
	s.mu.Lock()
	s.items[item.InstanceID] = item
	s.mu.Unlock()
	return nil
}

func (s *memoryRuntimeStore) GetRuntimeInstance(_ context.Context, id string) (RuntimeInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[id]
	if !ok {
		return RuntimeInstance{}, ErrInstanceNotFound
	}
	return item, nil
}

func (s *memoryRuntimeStore) GetActiveRuntimeByRobot(_ context.Context, robotID string) (RuntimeInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.items {
		if item.RobotID == robotID && item.Status.Active() {
			return item, nil
		}
	}
	return RuntimeInstance{}, ErrInstanceNotFound
}

func (s *memoryRuntimeStore) ListRuntimeInstances(context.Context) ([]RuntimeInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]RuntimeInstance, 0, len(s.items))
	for _, item := range s.items {
		items = append(items, item)
	}
	return items, nil
}

type memoryPorts struct {
	mu       sync.Mutex
	byOwner  map[string]int
	byPort   map[int]string
	released []string
}

func newMemoryPorts() *memoryPorts {
	return &memoryPorts{byOwner: make(map[string]int), byPort: make(map[int]string)}
}

func (p *memoryPorts) AcquireAbilityFrameworkPort(_ context.Context, owner string, first, last int) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if port := p.byOwner[owner]; port > 0 {
		return port, nil
	}
	for port := first; port <= last; port++ {
		if p.byPort[port] == "" {
			p.byOwner[owner] = port
			p.byPort[port] = owner
			return port, nil
		}
	}
	return 0, ErrPortUnavailable
}

func (p *memoryPorts) ReleaseAbilityFrameworkPort(_ context.Context, owner string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	port := p.byOwner[owner]
	delete(p.byOwner, owner)
	delete(p.byPort, port)
	p.released = append(p.released, owner)
	return nil
}

type fakeLauncher struct {
	mu                   sync.Mutex
	starts               []LaunchRequest
	steps                []string
	unconfirmedSafeRobot string
	reclaims             []string
	reclaimConfirmed     bool
}

func (l *fakeLauncher) ReclaimInterruptedSimulation(
	_ context.Context, item RuntimeInstance, _ string,
) (StopEvidence, error) {
	l.mu.Lock()
	l.reclaims = append(l.reclaims, item.InstanceID)
	l.mu.Unlock()
	return StopEvidence{Confirmed: l.reclaimConfirmed}, nil
}

func (l *fakeLauncher) Start(_ context.Context, request LaunchRequest) (LaunchResult, error) {
	l.mu.Lock()
	l.starts = append(l.starts, request)
	l.mu.Unlock()
	return LaunchResult{AbilityFrameworkEndpoint: fmt.Sprintf("http://127.0.0.1:%d", request.Instance.AbilityFrameworkPort)}, nil
}

func (l *fakeLauncher) Stop(_ context.Context, item RuntimeInstance, _ string) (StopEvidence, error) {
	l.mu.Lock()
	l.steps = append(l.steps, "instance:"+item.RobotID)
	l.mu.Unlock()
	return StopEvidence{Confirmed: item.RobotID != l.unconfirmedSafeRobot}, nil
}

type blockingLauncher struct {
	*fakeLauncher
	entered chan string
	release chan struct{}
}

func (l *blockingLauncher) Start(ctx context.Context, request LaunchRequest) (LaunchResult, error) {
	select {
	case l.entered <- request.Instance.RobotID:
	case <-ctx.Done():
		return LaunchResult{}, ctx.Err()
	}
	select {
	case <-l.release:
		return l.fakeLauncher.Start(ctx, request)
	case <-ctx.Done():
		return LaunchResult{}, ctx.Err()
	}
}

type memoryEvents struct {
	mu     sync.Mutex
	events []Event
}

func (s *memoryEvents) PublishRuntimeEvent(_ context.Context, event Event) error {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
	return nil
}

func testBundle() Bundle {
	return Bundle{Name: "r1pro-standard", Version: "0.5.0", Path: "/bundles/r1pro-standard",
		Match: MatchKey{RobotModel: "R1Pro", Backend: "fake", BackendProfile: "fake"}}
}

func testDescriptor(robotID string) VirtualRobotDescriptor {
	match := testBundle().Match
	return VirtualRobotDescriptor{RobotID: robotID, SceneInstanceID: "scene-test", Model: match.RobotModel,
		Backend: match.Backend, Kind: "simulation", BackendProfile: match.BackendProfile,
		Endpoint: "fake://" + robotID}
}
func TestCatalogMatchesCompleteRobotRuntimeKey(t *testing.T) {
	match := testBundle().Match
	catalog, err := NewCatalog(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Resolve(MatchKey{RobotModel: match.RobotModel, Backend: "mujoco",
		BackendProfile: match.BackendProfile}); !errors.Is(err, ErrBundleNotFound) {
		t.Fatalf("后端不匹配不应降级选择: %v", err)
	}
	other := testBundle()
	other.Name = "another-r1pro"
	if err := catalog.Register(other); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Resolve(match); !errors.Is(err, ErrBundleAmbiguous) {
		t.Fatalf("同一完整 key 有两个 Bundle 时必须拒绝: %v", err)
	}
}

func TestOrchestratorStartsDifferentRobotsInParallel(t *testing.T) {
	catalog, _ := NewCatalog(testBundle())
	launcher := &blockingLauncher{fakeLauncher: &fakeLauncher{},
		entered: make(chan string, 2), release: make(chan struct{})}
	orchestrator, err := NewOrchestrator(OrchestratorConfig{Catalog: catalog,
		Store: newMemoryRuntimeStore(), Ports: newMemoryPorts(),
		Launcher: launcher, DataRoot: t.TempDir(), PortFirst: 19200, PortLast: 19210})
	if err != nil {
		t.Fatal(err)
	}
	errorsByRobot := make(chan error, 2)
	for _, robotID := range []string{"robot-a", "robot-b"} {
		robotID := robotID
		go func() {
			_, runErr := orchestrator.Start(context.Background(), StartRequest{
				InstanceID: "instance-" + robotID, PilotInstanceID: "pilot-" + robotID,
				Descriptor: testDescriptor(robotID)})
			errorsByRobot <- runErr
		}()
	}
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case robotID := <-launcher.entered:
			seen[robotID] = true
		case <-time.After(time.Second):
			t.Fatalf("不同 Robot 被全局锁串行化，仅进入: %v", seen)
		}
	}
	close(launcher.release)
	for range 2 {
		if err := <-errorsByRobot; err != nil {
			t.Fatal(err)
		}
	}
}
func TestOrchestratorIsolatesTwoRobotsSharingOneBundle(t *testing.T) {
	catalog, err := NewCatalog(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	store := newMemoryRuntimeStore()
	ports := newMemoryPorts()
	launcher := &fakeLauncher{}
	events := &memoryEvents{}
	clock := time.Date(2027, 2, 1, 8, 0, 0, 0, time.UTC)
	orchestrator, err := NewOrchestrator(OrchestratorConfig{Catalog: catalog, Store: store, Ports: ports,
		Launcher: launcher, Events: events, DataRoot: t.TempDir(),
		PortFirst: 19000, PortLast: 19010, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	request := StartRequest{PilotInstanceID: "pilot-a", InstanceID: "instance-a",
		Descriptor: testDescriptor("robot-a")}
	first, err := orchestrator.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.PilotInstanceID = "pilot-b"
	request.Descriptor = testDescriptor("robot-b")
	request.InstanceID = "instance-b"
	second, err := orchestrator.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.BundleName != second.BundleName || first.BundleVersion != second.BundleVersion {
		t.Fatalf("两台同型 Robot 应复用一个 Bundle: %#v %#v", first, second)
	}
	if first.RobotID == second.RobotID || first.AbilityFrameworkPort == second.AbilityFrameworkPort ||
		first.DataDirectory == second.DataDirectory || first.AbilityFrameworkEndpoint == second.AbilityFrameworkEndpoint {
		t.Fatalf("Robot ID、AF 端口、数据目录和 endpoint 必须隔离: %#v %#v", first, second)
	}
	if filepath.Base(filepath.Dir(first.DataDirectory)) != "robot-a" ||
		filepath.Base(first.DataDirectory) != "instance-a" ||
		filepath.Base(filepath.Dir(second.DataDirectory)) != "robot-b" ||
		filepath.Base(second.DataDirectory) != "instance-b" {
		t.Fatalf("数据目录没有按 Robot 和 Runtime Instance 隔离: %s %s", first.DataDirectory, second.DataDirectory)
	}
	if len(launcher.starts) != 2 || launcher.starts[0].Descriptor.Endpoint == launcher.starts[1].Descriptor.Endpoint {
		t.Fatalf("Launcher 未收到两个独立 Robot endpoint: %#v", launcher.starts)
	}
	stopped, err := orchestrator.Stop(context.Background(), first.InstanceID, "operator requested")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Status != StateStopped {
		t.Fatalf("Robot A 未安全停止: %#v", stopped)
	}
	wantSteps := []string{"instance:robot-a"}
	if !reflect.DeepEqual(launcher.steps, wantSteps) {
		t.Fatalf("停止顺序错误: got=%v want=%v", launcher.steps, wantSteps)
	}
	stillReady, err := store.GetRuntimeInstance(context.Background(), second.InstanceID)
	if err != nil || stillReady.Status != StateReady {
		t.Fatalf("停止 Robot A 不应影响 Robot B: %#v err=%v", stillReady, err)
	}
	if ports.byOwner[first.InstanceID] != 0 || ports.byOwner[second.InstanceID] != second.AbilityFrameworkPort {
		t.Fatalf("端口租约释放范围错误: %#v", ports.byOwner)
	}
	if len(events.events) < 6 || events.events[0].Instance.Status != StateStarting {
		t.Fatalf("没有持久发布 Runtime 状态事件: %#v", events.events)
	}
}

func TestOrchestratorUnknownSafeStopInterruptsAndKeepsResources(t *testing.T) {
	catalog, _ := NewCatalog(testBundle())
	store := newMemoryRuntimeStore()
	ports := newMemoryPorts()
	launcher := &fakeLauncher{unconfirmedSafeRobot: "robot-a"}
	orchestrator, err := NewOrchestrator(OrchestratorConfig{Catalog: catalog, Store: store, Ports: ports,
		Launcher: launcher, DataRoot: t.TempDir(),
		PortFirst: 19100, PortLast: 19110})
	if err != nil {
		t.Fatal(err)
	}
	started, err := orchestrator.Start(context.Background(), StartRequest{InstanceID: "instance-a",
		PilotInstanceID: "pilot-a", Descriptor: testDescriptor("robot-a")})
	if err != nil {
		t.Fatal(err)
	}
	result, err := orchestrator.Stop(context.Background(), started.InstanceID, "test unknown stop")
	if err == nil || result.Status != StateInterrupted {
		t.Fatalf("没有停止证据时必须 interrupted: result=%#v err=%v", result, err)
	}
	if ports.byOwner[started.InstanceID] != started.AbilityFrameworkPort {
		t.Fatalf("interrupted 不能释放端口租约: %#v", ports.byOwner)
	}
	if !reflect.DeepEqual(launcher.steps, []string{"instance:robot-a"}) {
		t.Fatalf("安全点未知后不应继续杀 Ability/AF: %v", launcher.steps)
	}
	if _, err := orchestrator.Start(context.Background(), StartRequest{InstanceID: "replacement",
		PilotInstanceID: "pilot-a", Descriptor: testDescriptor("robot-a")}); !errors.Is(err, ErrRobotAlreadyInUse) {
		t.Fatalf("interrupted 期间不得启动替换实例: %v", err)
	}
}

func TestOrchestratorOnlyReclaimsInterruptedManagedSimulationForAnotherScene(t *testing.T) {
	bundle := testBundle()
	bundle.Match.Backend = "mujoco"
	bundle.Match.BackendProfile = "mujoco-tote"
	catalog, _ := NewCatalog(bundle)
	store := newMemoryRuntimeStore()
	ports := newMemoryPorts()
	launcher := &fakeLauncher{unconfirmedSafeRobot: "robot-a", reclaimConfirmed: true}
	orchestrator, err := NewOrchestrator(OrchestratorConfig{Catalog: catalog, Store: store, Ports: ports,
		Launcher: launcher, DataRoot: t.TempDir(), PortFirst: 19300, PortLast: 19310})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := testDescriptor("robot-a")
	descriptor.Backend = "mujoco"
	descriptor.BackendProfile = "mujoco-tote"
	descriptor.SceneInstanceID = "scene-old"
	started, err := orchestrator.Start(context.Background(), StartRequest{InstanceID: "instance-old",
		PilotInstanceID: "pilot-a", Descriptor: descriptor})
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := orchestrator.Stop(context.Background(), started.InstanceID, "runtime disappeared")
	if err == nil || interrupted.Status != StateInterrupted {
		t.Fatalf("测试前置实例没有进入 interrupted: result=%#v err=%v", interrupted, err)
	}
	if _, err := orchestrator.ReclaimInterruptedSimulation(
		context.Background(), interrupted.InstanceID, "scene-old", false, "same scene",
	); err == nil {
		t.Fatal("同一 Scene 不得绕过普通安全停止")
	}
	reclaimed, err := orchestrator.ReclaimInterruptedSimulation(
		context.Background(), interrupted.InstanceID, "scene-new", false, "old managed scene removed",
	)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.Status != StateStopped || len(launcher.reclaims) != 1 ||
		launcher.reclaims[0] != interrupted.InstanceID {
		t.Fatalf("旧受管仿真实例没有精确回收: result=%#v reclaims=%v", reclaimed, launcher.reclaims)
	}
	if ports.byOwner[interrupted.InstanceID] != 0 {
		t.Fatalf("回收完成后未释放端口: %#v", ports.byOwner)
	}
}

func TestOrchestratorReclaimsSameManagedSceneOnlyAfterRuntimeHold(t *testing.T) {
	for _, backend := range []string{"mujoco", "isaac"} {
		t.Run(backend, func(t *testing.T) {
			bundle := testBundle()
			bundle.Match.Backend = backend
			bundle.Match.BackendProfile = "mujoco-tote"
			catalog, _ := NewCatalog(bundle)
			store := newMemoryRuntimeStore()
			ports := newMemoryPorts()
			launcher := &fakeLauncher{unconfirmedSafeRobot: "robot-hold", reclaimConfirmed: true}
			orchestrator, err := NewOrchestrator(OrchestratorConfig{
				Catalog: catalog, Store: store, Ports: ports, Launcher: launcher,
				DataRoot: t.TempDir(), PortFirst: 19400, PortLast: 19410,
			})
			if err != nil {
				t.Fatal(err)
			}
			descriptor := testDescriptor("robot-hold")
			descriptor.Backend = backend
			descriptor.BackendProfile = "mujoco-tote"
			descriptor.SceneInstanceID = "scene-held"
			started, err := orchestrator.Start(context.Background(), StartRequest{
				InstanceID: "instance-held", PilotInstanceID: "pilot-held", Descriptor: descriptor,
			})
			if err != nil {
				t.Fatal(err)
			}
			interrupted, err := orchestrator.Stop(context.Background(), started.InstanceID, "pilot offline")
			if err == nil || interrupted.Status != StateInterrupted {
				t.Fatalf("测试前置实例没有进入 interrupted: result=%#v err=%v", interrupted, err)
			}
			if _, err := orchestrator.ReclaimInterruptedSimulation(
				context.Background(), interrupted.InstanceID, interrupted.SceneInstanceID,
				false, "no hold",
			); err == nil {
				t.Fatal("同一 Scene 没有 Runtime hold 证据时不得回收")
			}
			reclaimed, err := orchestrator.ReclaimInterruptedSimulation(
				context.Background(), interrupted.InstanceID, interrupted.SceneInstanceID,
				true, "runtime hold confirmed",
			)
			if err != nil {
				t.Fatal(err)
			}
			if reclaimed.Status != StateStopped || len(launcher.reclaims) != 1 {
				t.Fatalf("Runtime hold 已确认后没有回收受管实例: result=%#v reclaims=%v",
					reclaimed, launcher.reclaims)
			}
		})
	}
}

func TestSimulationReclaimStillRejectsRealRobot(t *testing.T) {
	catalog, _ := NewCatalog(testBundle())
	st := newMemoryRuntimeStore()
	launcher := &fakeLauncher{reclaimConfirmed: true}
	orchestrator, _ := NewOrchestrator(OrchestratorConfig{Catalog: catalog, Store: st,
		Ports: newMemoryPorts(), Launcher: launcher, DataRoot: t.TempDir(), PortFirst: 19400, PortLast: 19410})
	if err := st.SaveRuntimeInstance(context.Background(), RuntimeInstance{InstanceID: "real", RobotID: "real",
		Backend: "real", Status: StateInterrupted, SceneInstanceID: "scene"}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.ReclaimInterruptedSimulation(context.Background(), "real", "scene", true, "test"); err == nil {
		t.Fatal("仿真恢复不能用于真机")
	}
	if len(launcher.reclaims) != 0 {
		t.Fatal("真机被错误传给进程回收器")
	}
}

func TestOrchestratorPersistsBundleMismatchAsFailedRuntime(t *testing.T) {
	catalog, _ := NewCatalog(testBundle())
	st := newMemoryRuntimeStore()
	ports := newMemoryPorts()
	launcher := &fakeLauncher{}
	orchestrator, err := NewOrchestrator(OrchestratorConfig{
		Catalog: catalog, Store: st, Ports: ports, Launcher: launcher,
		DataRoot: t.TempDir(), PortFirst: 19500, PortLast: 19510,
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := testDescriptor("robot-mismatch")
	descriptor.Backend = "mujoco"
	failed, err := orchestrator.Start(context.Background(), StartRequest{
		InstanceID: "instance-mismatch", PilotInstanceID: "pilot-mismatch",
		Descriptor: descriptor, ProjectID: "project-mismatch",
	})
	if !errors.Is(err, ErrBundleNotFound) {
		t.Fatalf("Bundle 不匹配没有返回明确错误: %v", err)
	}
	stored, storeErr := st.GetRuntimeInstance(context.Background(), failed.InstanceID)
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	if failed.Status != StateFailed || stored.Status != StateFailed ||
		stored.FailureReason == "" || len(launcher.starts) != 0 {
		t.Fatalf("Bundle 不匹配没有形成可见 failed Runtime: failed=%#v stored=%#v starts=%d",
			failed, stored, len(launcher.starts))
	}
}
