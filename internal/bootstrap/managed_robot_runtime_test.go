package bootstrap

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	robotdomain "insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/robotruntime"
	"insightos.cn/semantic-framework/internal/simulation"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/log"
)

type shutdownEvidenceLauncher struct{ confirmed bool }

func (l shutdownEvidenceLauncher) Start(
	context.Context, robotruntime.LaunchRequest,
) (robotruntime.LaunchResult, error) {
	return robotruntime.LaunchResult{}, nil
}

func (l shutdownEvidenceLauncher) Stop(
	context.Context, robotruntime.RuntimeInstance, string,
) (robotruntime.StopEvidence, error) {
	return robotruntime.StopEvidence{Confirmed: l.confirmed}, nil
}

func TestManagedRobotLauncherWritesCanonicalInstanceConfig(t *testing.T) {
	root := t.TempDir()
	launcher := &managedRobotInstanceLauncher{
		serverHTTP: "http://127.0.0.1:8080",
		serverWS:   "ws://127.0.0.1:8081/ws/pilot",
	}
	request := robotruntime.LaunchRequest{
		Bundle: robotruntime.Bundle{Path: filepath.Join(root, "bundle")},
		Instance: robotruntime.RuntimeInstance{
			InstanceID:           "runtime-1",
			PilotInstanceID:      "pilot-r1-pro-1",
			RobotID:              "r1-pro-1",
			AbilityFrameworkPort: 18123,
			DataDirectory:        filepath.Join(root, "robots", "r1-pro-1"),
		},
		Descriptor: robotruntime.VirtualRobotDescriptor{
			RobotID:            "r1-pro-1",
			SceneInstanceID:    "scene-1",
			Model:              "r1_pro_chassis",
			Backend:            "mujoco",
			SDKPackage:         "semantic-robot-sdk-r1pro",
			BackendProfile:     "r1pro-tote-mujoco-v1",
			Endpoint:           "http://127.0.0.1:8090",
			URDFPath:           "/bundle/assets/r1pro.urdf",
			PackageDirectories: []string{"/bundle/assets/robot"},
			CoordinateFrame:    "world",
			Tools: []robotruntime.ToolDescriptor{{
				ToolRef:       "component://tool/left",
				Side:          "left",
				Kind:          "tote_clamp",
				Frame:         "left_tote_clamp",
				Joint:         "left_tote_clamp_joint",
				TravelM:       0.08,
				NormalForceN:  80,
				MaximumForceN: 120,
			}},
		},
	}
	configPath, err := launcher.writeInstanceConfig(request, "pilot_credential")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("含 Pilot credential 的实例配置权限错误: %o", info.Mode().Perm())
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var document robotInstanceConfig
	if err := yaml.Unmarshal(content, &document); err != nil {
		t.Fatal(err)
	}
	if document.Spec.SemanticServer.AccessToken != "pilot_credential" ||
		document.Spec.Robot.Model != "r1_pro_chassis" ||
		document.Spec.Robot.BackendProfile != "r1pro-tote-mujoco-v1" ||
		document.Spec.Robot.SceneInstanceID != "scene-1" {
		t.Fatalf("生成的受管实例配置不完整: %+v", document.Spec)
	}
	if len(document.Spec.Robot.Tools) != 1 ||
		document.Spec.Robot.Tools[0].ToolRef != "component://tool/left" {
		t.Fatalf("工具描述没有进入 RobotDeployment: %#v", document.Spec.Robot.Tools)
	}
	if len(document.Spec.Robot.PackageDirectories) != 1 ||
		document.Spec.Robot.PackageDirectories[0] != "/bundle/assets/robot" {
		t.Fatalf("URDF package目录没有进入 RobotDeployment: %#v", document.Spec.Robot.PackageDirectories)
	}
}

func TestManagedRobotShutdownConvergesExecutionOnlyWithSafeEvidence(t *testing.T) {
	for _, test := range []struct {
		name      string
		confirmed bool
		want      string
	}{
		{name: "confirmed", confirmed: true, want: "stopped"},
		{name: "unconfirmed", confirmed: false, want: "running"},
	} {
		t.Run(test.name, func(t *testing.T) {
			logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
			st := storetest.OpenMigrated(t, logger)
			project, err := st.CreateProject("user-shutdown", "shutdown")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			execution := store.RobotExecution{
				ID: "rex-shutdown", ProjectID: project.ID, RobotID: "robot-shutdown",
				PilotInstanceID: "pilot-shutdown", SkillName: "semantic-navigation",
				SkillVersion: "0.5.0", RequestKey: "shutdown", Status: "running",
				Revision: 1, CreatedAt: now, UpdatedAt: now,
			}
			if err := st.SaveRobotExecution(execution); err != nil {
				t.Fatal(err)
			}
			if err := st.SaveRobotPilot(store.RobotPilot{
				PilotInstanceID: execution.PilotInstanceID, RobotID: execution.RobotID,
				Status: "online", RobotStatus: "busy", CurrentExecutionID: execution.ID,
				Revision: 1, LastSeenAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			instance := robotruntime.RuntimeInstance{
				InstanceID: "runtime-shutdown", PilotInstanceID: execution.PilotInstanceID,
				RobotID: execution.RobotID, ProjectID: project.ID,
				Status: robotruntime.StateReady, Revision: 1, CreatedAt: now, UpdatedAt: now,
			}
			if err := st.SaveRuntimeInstance(context.Background(), instance); err != nil {
				t.Fatal(err)
			}
			catalog, err := robotruntime.NewCatalog()
			if err != nil {
				t.Fatal(err)
			}
			orchestrator, err := robotruntime.NewOrchestrator(robotruntime.OrchestratorConfig{
				Catalog: catalog, Store: st, Ports: st,
				Launcher: shutdownEvidenceLauncher{confirmed: test.confirmed},
				DataRoot: t.TempDir(),
			})
			if err != nil {
				t.Fatal(err)
			}
			lifecycle := &managedSceneRobotLifecycle{
				orchestrator: orchestrator, store: st, robots: robotdomain.NewService(st, nil),
			}
			shutdownErr := lifecycle.Shutdown(context.Background())
			if test.confirmed && shutdownErr != nil {
				t.Fatal(shutdownErr)
			}
			if !test.confirmed && shutdownErr == nil {
				t.Fatal("缺少安全证据时 Shutdown 应保留可见错误")
			}
			latest, err := st.GetRobotExecution(execution.ID)
			if err != nil || latest.Status != test.want {
				t.Fatalf("Execution 收敛状态错误: execution=%+v err=%v", latest, err)
			}
			if test.confirmed &&
				(latest.Result["safe"] != true || latest.Result["hold_confirmed"] != true) {
				t.Fatalf("安全停止证据没有进入 Execution: %#v", latest.Result)
			}
		})
	}
}

func TestMissingStateJSONBlocksReclaimUntilGoneIsTreatedClean(t *testing.T) {
	ctx := context.Background()
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
	st := storetest.OpenMigrated(t, logger)

	dataRoot := t.TempDir()
	instanceID := "robot-9fa5b90d-333c-4707-99c6-04b46b7a3042-r1_pro_tote_gripper-1"
	robotID := "r1_pro_tote_gripper-1"
	sceneID := "9fa5b90d-333c-4707-99c6-04b46b7a3042"
	dataDir := filepath.Join(dataRoot, "robots", robotID, instanceID)
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		t.Fatal(err)
	}

	_, readErr := readInstanceSupervisorState(dataDir)
	if readErr == nil || !os.IsNotExist(readErr) {
		t.Fatalf("复现前置条件失败，应读不到 state.json: %v", readErr)
	}
	legacyUI := "managed_simulation_reclaim: " + readErr.Error()
	if !strings.Contains(legacyUI, "state.json") || !strings.Contains(legacyUI, "no such file or directory") {
		t.Fatalf("旧页面报错形态不对: %s", legacyUI)
	}

	now := time.Now().UTC()
	instance := robotruntime.RuntimeInstance{
		InstanceID:      instanceID,
		PilotInstanceID: "pilot-" + robotID,
		RobotID:         robotID,
		SceneInstanceID: sceneID,
		Backend:         "mujoco",
		Status:          robotruntime.StateInterrupted,
		DataDirectory:   dataDir,
		FailureReason:   "启动 Ability Runtime: render Robot Runtime instance: exit status 1",
		Revision:        1,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := st.SaveRuntimeInstance(ctx, instance); err != nil {
		t.Fatal(err)
	}
	port, err := st.AcquireAbilityFrameworkPort(ctx, instanceID, 18100, 18110)
	if err != nil {
		t.Fatal(err)
	}
	instance.AbilityFrameworkPort = port
	if err := st.SaveRuntimeInstance(ctx, instance); err != nil {
		t.Fatal(err)
	}

	catalog, err := robotruntime.NewCatalog(robotruntime.Bundle{
		Name: "r1pro-mujoco", Version: "0.5.0", Path: filepath.Join(t.TempDir(), "empty-bundle"),
		Match: robotruntime.MatchKey{
			RobotModel: "r1_pro_chassis", Backend: "mujoco", BackendProfile: "r1pro-tote-mujoco-v1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	launcher := &managedRobotInstanceLauncher{
		processes: map[string]*managedRobotProcess{instanceID: {}},
	}
	orchestrator, err := robotruntime.NewOrchestrator(robotruntime.OrchestratorConfig{
		Catalog: catalog, Store: st, Ports: st, Launcher: launcher, DataRoot: dataRoot,
		PortFirst: 18100, PortLast: 18110,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("switch_scene_reclaim", func(t *testing.T) {
		reclaimed, reclaimErr := orchestrator.ReclaimInterruptedSimulation(
			ctx, instanceID, "scene-new-layout", false, "旧受管仿真场景已被替换",
		)
		if reclaimErr != nil {
			t.Fatalf("修复后回收空实例不应失败: %v", reclaimErr)
		}
		if reclaimed.Status != robotruntime.StateStopped {
			t.Fatalf("回收后状态应为 stopped: %#v", reclaimed)
		}
		if reclaimed.FailureReason != "" {
			t.Fatalf("回收成功后不应再留下页面红字: %s", reclaimed.FailureReason)
		}
		if strings.Contains(reclaimed.FailureReason, "managed_simulation_reclaim") {
			t.Fatal("页面仍会显示 managed_simulation_reclaim")
		}
		if _, activeErr := st.GetActiveRuntimeByRobot(ctx, robotID); !errors.Is(activeErr, robotruntime.ErrInstanceNotFound) {
			t.Fatalf("回收后 Robot 仍被活动实例占用: %v", activeErr)
		}
		if lease, leaseErr := st.AcquireAbilityFrameworkPort(ctx, "other-instance", 18100, 18110); leaseErr != nil || lease != port {
			t.Fatalf("回收后端口未释放: port=%d err=%v", lease, leaseErr)
		}
	})

	t.Run("failed_start_without_render_does_not_stick", func(t *testing.T) {
		stuckRobot := "r1_pro_tote_gripper-retry"
		started, startErr := orchestrator.Start(ctx, robotruntime.StartRequest{
			InstanceID:      "robot-retry-" + stuckRobot,
			PilotInstanceID: "pilot-" + stuckRobot,
			Descriptor: robotruntime.VirtualRobotDescriptor{
				RobotID: stuckRobot, SceneInstanceID: "scene-retry",
				Model: "r1_pro_chassis", Backend: "mujoco", Kind: "simulation",
				BackendProfile: "r1pro-tote-mujoco-v1",
			},
		})
		if startErr == nil {
			t.Fatal("空 Bundle 启动本应失败")
		}
		if !strings.Contains(startErr.Error(), "不可执行") {
			t.Fatalf("启动失败原因不对: %v", startErr)
		}
		if started.Status != robotruntime.StateFailed {
			t.Fatalf("未 render 的启动失败应落成 failed 而不是 interrupted: %#v", started)
		}
		if _, activeErr := st.GetActiveRuntimeByRobot(ctx, stuckRobot); !errors.Is(activeErr, robotruntime.ErrInstanceNotFound) {
			t.Fatalf("failed 实例不应继续占用 Robot: %v", activeErr)
		}
		_, retryErr := orchestrator.Start(ctx, robotruntime.StartRequest{
			InstanceID:      "robot-retry-2-" + stuckRobot,
			PilotInstanceID: "pilot-" + stuckRobot,
			Descriptor: robotruntime.VirtualRobotDescriptor{
				RobotID: stuckRobot, SceneInstanceID: "scene-retry-2",
				Model: "r1_pro_chassis", Backend: "mujoco", Kind: "simulation",
				BackendProfile: "r1pro-tote-mujoco-v1",
			},
		})
		if errors.Is(retryErr, robotruntime.ErrRobotAlreadyInUse) {
			t.Fatal("修复后重试启动仍被旧实例占用")
		}
	})

	t.Run("prepare_new_scene_unblocks_after_empty_interrupt", func(t *testing.T) {
		blockedID := "robot-blocked-scene"
		blockedDir := filepath.Join(dataRoot, "robots", "blocked", blockedID)
		if err := os.MkdirAll(blockedDir, 0o750); err != nil {
			t.Fatal(err)
		}
		blocked := robotruntime.RuntimeInstance{
			InstanceID:      blockedID,
			RobotID:         "blocked-robot",
			SceneInstanceID: "scene-old",
			Backend:         "mujoco",
			Status:          robotruntime.StateInterrupted,
			DataDirectory:   blockedDir,
			Revision:        1,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if err := st.SaveRuntimeInstance(ctx, blocked); err != nil {
			t.Fatal(err)
		}
		lifecycle := &managedSceneRobotLifecycle{orchestrator: orchestrator, store: st}
		skip, prepareErr := lifecycle.prepareSceneRobotStart(ctx, simulation.SceneInstance{
			InstanceID: "scene-new",
		}, robotruntime.VirtualRobotDescriptor{
			RobotID: "blocked-robot", SceneInstanceID: "scene-new",
			Model: "r1_pro_chassis", Backend: "mujoco", BackendProfile: "r1pro-tote-mujoco-v1",
		})
		if prepareErr != nil {
			t.Fatalf("新场景启动前回收空实例失败: %v", prepareErr)
		}
		if skip {
			t.Fatal("回收后不应跳过新场景启动")
		}
		latest, latestErr := st.GetRuntimeInstance(ctx, blockedID)
		if latestErr != nil || latest.Status != robotruntime.StateStopped {
			t.Fatalf("prepareSceneRobotStart 没有把空实例收成 stopped: %+v err=%v", latest, latestErr)
		}
	})
}

func TestReclaimInterruptedSimulationTreatsMissingStateAsClean(t *testing.T) {
	launcher := &managedRobotInstanceLauncher{
		processes: map[string]*managedRobotProcess{"runtime-1": {}},
	}
	evidence, err := launcher.ReclaimInterruptedSimulation(
		context.Background(),
		robotruntime.RuntimeInstance{
			InstanceID:    "runtime-1",
			Status:        robotruntime.StateInterrupted,
			Backend:       "mujoco",
			DataDirectory: filepath.Join(t.TempDir(), "missing-instance"),
		},
		"旧受管仿真场景已被替换",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !evidence.Confirmed || evidence.Details["managed_instance_absent"] != true {
		t.Fatalf("没有 state.json 时应视为已清理: %#v", evidence)
	}
	if _, stillTracked := launcher.processes["runtime-1"]; stillTracked {
		t.Fatal("空实例回收后仍留在进程表")
	}
}

func TestStopTreatsMissingRenderedInstanceAsConfirmed(t *testing.T) {
	launcher := &managedRobotInstanceLauncher{
		processes: map[string]*managedRobotProcess{"runtime-1": {}},
	}
	evidence, err := launcher.Stop(
		context.Background(),
		robotruntime.RuntimeInstance{
			InstanceID:    "runtime-1",
			DataDirectory: filepath.Join(t.TempDir(), "never-rendered"),
		},
		"runtime startup failed",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !evidence.Confirmed || evidence.Details["managed_instance_absent"] != true {
		t.Fatalf("未 render 的实例停止应直接确认: %#v", evidence)
	}
}

func TestWithInstanceTempDirOverridesExistingTMPDIR(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "robots", "r1-pro-1")
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	want := dataDir + ".tmp"
	env, err := withInstanceTempDir(
		[]string{"PATH=/usr/bin", "TMPDIR=/tmp", "HOME=/home/dev", "TMPDIR=/dev/shm"},
		dataDir,
	)
	if err != nil {
		t.Fatal(err)
	}
	if info, statErr := os.Stat(want); statErr != nil || !info.IsDir() {
		t.Fatalf("应创建实例旁的临时目录 %s: %v", want, statErr)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("TMPDIR 不能建在实例目录内，否则 render 会拒绝覆盖: %#v", entries)
	}
	var tmpdirs []string
	for _, item := range env {
		if strings.HasPrefix(item, "TMPDIR=") {
			tmpdirs = append(tmpdirs, item)
		}
	}
	if len(tmpdirs) != 1 || tmpdirs[0] != "TMPDIR="+want {
		t.Fatalf("TMPDIR 应只保留实例目录旁一条: %#v", tmpdirs)
	}
}

func TestCPUWaitPolicyEnvStopsSpinWaitingForManagedShapes(t *testing.T) {
	// 无独显主机上推理在 CPU 上跑；空闲线程自旋会与同机仿真抢 CPU，同机重载下
	// 交错复测三次约 3 倍差距（10 线程 21.3s 对 7.7s），因此必须在拉起受管
	// 实例时就注入，而不是等 torch 初始化后再设。
	env := cpuWaitPolicyEnv([]string{"PATH=/usr/bin", "HOME=/home/dev"})
	want := map[string]bool{"KMP_BLOCKTIME=0": false, "OMP_WAIT_POLICY=PASSIVE": false}
	for _, item := range env {
		if _, ok := want[item]; ok {
			want[item] = true
		}
	}
	for key, found := range want {
		if !found {
			t.Fatalf("受管进程环境应包含 %s: %#v", key, env)
		}
	}
	if len(env) != 4 {
		t.Fatalf("应保留基础环境并追加两条等待策略: %#v", env)
	}
}

func TestCPUWaitPolicyEnvKeepsOperatorOverrides(t *testing.T) {
	// 现场排障可能显式指定等待策略，注入不能覆盖运维意图，也不能留下两条同名列。
	env := cpuWaitPolicyEnv([]string{"KMP_BLOCKTIME=200", "OMP_WAIT_POLICY=active", "PATH=/usr/bin"})
	counts := map[string]int{}
	for _, item := range env {
		name, value, _ := strings.Cut(item, "=")
		counts[name]++
		if name == "KMP_BLOCKTIME" && value != "200" {
			t.Fatalf("KMP_BLOCKTIME 应保留运维值 200: %#v", env)
		}
		if name == "OMP_WAIT_POLICY" && value != "active" {
			t.Fatalf("OMP_WAIT_POLICY 应保留运维值 active: %#v", env)
		}
	}
	for _, name := range []string{"KMP_BLOCKTIME", "OMP_WAIT_POLICY", "PATH"} {
		if counts[name] != 1 {
			t.Fatalf("%s 应恰好出现一次: %#v", name, env)
		}
	}
}

func TestRemoveRenderBlockingTempDirClearsOnlyLeftoverTmp(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "blocked")
	if err := os.MkdirAll(filepath.Join(blocked, "tmp"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := removeRenderBlockingTempDir(blocked); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(blocked)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("只剩 tmp 时应清掉以便 render: %#v", entries)
	}

	kept := filepath.Join(root, "kept")
	if err := os.MkdirAll(filepath.Join(kept, "tmp"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kept, "instance.yaml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeRenderBlockingTempDir(kept); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(kept, "tmp")); err != nil {
		t.Fatal("有其它文件时不应误删 tmp")
	}
}

func localMujocoBundle() (launcher, bundle string, ok bool) {
	bundle, err := filepath.Abs(filepath.Join("..", "..", ".output", "robot-bundles", "r1pro-mujoco-0.5.0-dev"))
	if err != nil {
		return "", "", false
	}
	launcher = filepath.Join(bundle, "bin", "semantic-robot-instance")
	info, err := os.Stat(launcher)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return "", "", false
	}
	return launcher, bundle, true
}

func renderRobotInstance(t *testing.T, launcher, configPath, outputDir string, env []string) (string, error) {
	t.Helper()
	command := exec.Command(launcher, "render", "--config", configPath, "--output", outputDir)
	if env != nil {
		command.Env = env
	}
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func sameDevice(t *testing.T, left, right string) bool {
	t.Helper()
	var leftInfo, rightInfo syscall.Stat_t
	if err := syscall.Stat(left, &leftInfo); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Stat(right, &rightInfo); err != nil {
		t.Fatal(err)
	}
	return leftInfo.Dev == rightInfo.Dev
}

func TestLocalRenderTmpdirDoesNotBlockOrCrossFilesystem(t *testing.T) {
	launcherPath, bundlePath, ok := localMujocoBundle()
	if !ok {
		t.Skip("本机没有 r1pro-mujoco Bundle，跳过真实 render 复现")
	}
	urdfPath, err := filepath.Abs(filepath.Join(
		"..", "..", "..", "semantic-scene", "mujoco-asset", "robot",
		"r1_pro_tote_gripper", "meshes", "r1_pro_tote_gripper.urdf",
	))
	if err != nil {
		t.Fatal(err)
	}
	packageDir := filepath.Clean(filepath.Join(filepath.Dir(urdfPath), "..", ".."))
	if _, err := os.Stat(urdfPath); err != nil {
		t.Skip("本机没有 R1 Pro URDF，跳过真实 render 复现")
	}

	root := t.TempDir()
	robotsRoot := filepath.Join(root, "robots", "r1_pro_tote_gripper-1")
	launcher := &managedRobotInstanceLauncher{
		serverHTTP: "http://127.0.0.1:8080",
		serverWS:   "ws://127.0.0.1:8081/ws/pilot",
	}

	newRequest := func(instanceID, dataDir string) robotruntime.LaunchRequest {
		return robotruntime.LaunchRequest{
			Bundle: robotruntime.Bundle{Path: bundlePath},
			Instance: robotruntime.RuntimeInstance{
				InstanceID:           instanceID,
				PilotInstanceID:      "pilot-" + instanceID,
				RobotID:              "r1_pro_tote_gripper-1",
				AbilityFrameworkPort: 18123,
				DataDirectory:        dataDir,
			},
			Descriptor: robotruntime.VirtualRobotDescriptor{
				RobotID:            "r1_pro_tote_gripper-1",
				SceneInstanceID:    "scene-local",
				Model:              "r1_pro_chassis",
				Backend:            "mujoco",
				BackendProfile:     "r1pro-tote-mujoco-v1",
				Endpoint:           "http://127.0.0.1:8090",
				URDFPath:           urdfPath,
				PackageDirectories: []string{packageDir},
				Tools: []robotruntime.ToolDescriptor{
					{
						ToolRef: "component://tool/left", Side: "left", Kind: "tote_clamp",
						Frame: "left_tote_load_frame", Joint: "left_tote_clamp_joint",
						TravelM: 0.039, NormalForceN: 60, MaximumForceN: 120,
					},
					{
						ToolRef: "component://tool/right", Side: "right", Kind: "tote_clamp",
						Frame: "right_tote_load_frame", Joint: "right_tote_clamp_joint",
						TravelM: 0.039, NormalForceN: 60, MaximumForceN: 120,
					},
				},
			},
		}
	}

	t.Run("old_inner_tmp_still_rejected_by_real_render", func(t *testing.T) {
		dataDir := filepath.Join(robotsRoot, "old-inner-tmp")
		if err := os.MkdirAll(filepath.Join(dataDir, "tmp"), 0o750); err != nil {
			t.Fatal(err)
		}
		configPath, err := launcher.writeInstanceConfig(newRequest("old-inner-tmp", dataDir), "token")
		if err != nil {
			t.Fatal(err)
		}
		output, err := renderRobotInstance(t, launcherPath, configPath, dataDir, os.Environ())
		if err == nil || !strings.Contains(output, "实例目录必须为空，拒绝覆盖") {
			t.Fatalf("旧路径应被真实 render 拒绝: err=%v output=%s", err, output)
		}
	})

	t.Run("fixed_sibling_tmpdir_allows_real_render", func(t *testing.T) {
		dataDir := filepath.Join(robotsRoot, "fixed-sibling")
		if err := os.MkdirAll(dataDir, 0o750); err != nil {
			t.Fatal(err)
		}
		configPath, err := launcher.writeInstanceConfig(newRequest("fixed-sibling", dataDir), "token")
		if err != nil {
			t.Fatal(err)
		}
		if err := removeRenderBlockingTempDir(dataDir); err != nil {
			t.Fatal(err)
		}
		env, err := withInstanceTempDir(os.Environ(), dataDir)
		if err != nil {
			t.Fatal(err)
		}
		output, err := renderRobotInstance(t, launcherPath, configPath, dataDir, env)
		if err != nil {
			t.Fatalf("修复后真实 render 失败: %v %s", err, output)
		}
		if _, statErr := os.Stat(filepath.Join(dataDir, "run", "state.json")); statErr != nil {
			t.Fatalf("render 成功后应写出 state.json: %v", statErr)
		}
		tmpDir := instanceSiblingTempDir(dataDir)
		if !sameDevice(t, dataDir, tmpDir) {
			t.Fatal("sibling TMPDIR 必须和实例目录同盘")
		}
		if sameDevice(t, dataDir, "/dev/shm") {
			t.Fatal("本机 /dev/shm 不应和仓库同盘，否则跨盘用例失真")
		}
		entries, err := os.ReadDir(dataDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.Name() == "tmp" {
				t.Fatal("修复后实例目录内不应再出现 tmp")
			}
		}
	})

	t.Run("leftover_inner_tmp_is_cleared_then_render", func(t *testing.T) {
		dataDir := filepath.Join(robotsRoot, "leftover-tmp")
		if err := os.MkdirAll(filepath.Join(dataDir, "tmp"), 0o750); err != nil {
			t.Fatal(err)
		}
		configPath, err := launcher.writeInstanceConfig(newRequest("leftover-tmp", dataDir), "token")
		if err != nil {
			t.Fatal(err)
		}
		if err := removeRenderBlockingTempDir(dataDir); err != nil {
			t.Fatal(err)
		}
		env, err := withInstanceTempDir(os.Environ(), dataDir)
		if err != nil {
			t.Fatal(err)
		}
		output, err := renderRobotInstance(t, launcherPath, configPath, dataDir, env)
		if err != nil {
			t.Fatalf("清掉遗留 tmp 后仍 render 失败: %v %s", err, output)
		}
	})

	t.Run("already_rendered_dir_still_refuses_overwrite", func(t *testing.T) {
		dataDir := filepath.Join(robotsRoot, "fixed-sibling")
		configPath, err := launcher.writeInstanceConfig(newRequest("fixed-sibling-retry", dataDir), "token")
		if err != nil {
			t.Fatal(err)
		}
		output, err := renderRobotInstance(t, launcherPath, configPath, dataDir, os.Environ())
		if err == nil || !strings.Contains(output, "实例目录必须为空，拒绝覆盖") {
			t.Fatalf("已有实例被再次 render 时应继续拒绝覆盖: err=%v output=%s", err, output)
		}
	})

	t.Run("cross_filesystem_rename_still_exdev", func(t *testing.T) {
		dataDir := filepath.Join(robotsRoot, "exdev-probe")
		if err := os.MkdirAll(dataDir, 0o750); err != nil {
			t.Fatal(err)
		}
		src, err := os.CreateTemp("/dev/shm", "af-package-")
		if err != nil {
			t.Fatal(err)
		}
		srcName := src.Name()
		_ = src.Close()
		target := filepath.Join(dataDir, "packaged.bin")
		err = os.Rename(srcName, target)
		if err == nil {
			t.Fatal("本机 /dev/shm → 实例目录的 rename 应复现 EXDEV")
		}
		if !errors.Is(err, syscall.EXDEV) && !strings.Contains(err.Error(), "invalid cross-device link") {
			t.Fatalf("跨盘错误类型不对: %v", err)
		}
		tmpDir := instanceSiblingTempDir(dataDir)
		if err := os.MkdirAll(tmpDir, 0o750); err != nil {
			t.Fatal(err)
		}
		sameSrc, err := os.CreateTemp(tmpDir, "af-package-")
		if err != nil {
			t.Fatal(err)
		}
		sameName := sameSrc.Name()
		_ = sameSrc.Close()
		if err := os.Rename(sameName, filepath.Join(dataDir, "packaged-ok.bin")); err != nil {
			t.Fatalf("同盘 sibling TMPDIR rename 应成功: %v", err)
		}
	})
}
