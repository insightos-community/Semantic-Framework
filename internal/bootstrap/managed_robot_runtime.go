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

package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"insightos.cn/semantic-framework/internal/ports/platform"
	processport "insightos.cn/semantic-framework/internal/ports/process"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	robotdomain "insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/robotruntime"
	"insightos.cn/semantic-framework/internal/simulation"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/log"
)

const (
	managedRobotShutdownTimeout = 45 * time.Second
)

type robotInstanceConfig struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Bundle string `yaml:"bundle"`
		Robot  struct {
			ID                 string         `yaml:"id"`
			DisplayName        string         `yaml:"displayName"`
			Model              string         `yaml:"model"`
			Backend            string         `yaml:"backend"`
			BackendProfile     string         `yaml:"backendProfile"`
			SDKEndpoint        string         `yaml:"sdkEndpoint"`
			FirmwareProfile    string         `yaml:"firmwareProfile"`
			URDFPath           string         `yaml:"urdfPath,omitempty"`
			PackageDirectories []string       `yaml:"packageDirectories,omitempty"`
			Options            map[string]any `yaml:"options,omitempty"`
			SceneInstanceID    string         `yaml:"sceneInstanceId,omitempty"`
			Tools              []struct {
				ToolRef       string  `yaml:"toolRef"`
				Side          string  `yaml:"side"`
				Kind          string  `yaml:"kind"`
				Frame         string  `yaml:"frame"`
				Joint         string  `yaml:"joint"`
				TravelM       float64 `yaml:"travelM"`
				NormalForceN  float64 `yaml:"normalForceN"`
				MaximumForceN float64 `yaml:"maximumForceN"`
			} `yaml:"tools,omitempty"`
		} `yaml:"robot"`
		AbilityFramework struct {
			Endpoint      string `yaml:"endpoint"`
			Managed       bool   `yaml:"managed"`
			ListenAddress string `yaml:"listenAddress"`
		} `yaml:"abilityFramework"`
		SemanticServer struct {
			WebSocketURL string `yaml:"websocketURL"`
			HTTPURL      string `yaml:"httpURL"`
			AccessToken  string `yaml:"accessToken"`
		} `yaml:"semanticServer"`
		Pilot struct {
			ID string `yaml:"id"`
		} `yaml:"pilot"`
	} `yaml:"spec"`
}

type instanceSupervisorState struct {
	Status              string `json:"status"`
	Error               string `json:"error,omitempty"`
	SupervisorPID       int    `json:"supervisor_pid,omitempty"`
	AbilityFrameworkPID int    `json:"ability_framework_pid,omitempty"`
	PilotPID            int    `json:"pilot_pid,omitempty"`
	StopEvidence        *struct {
		Safe                 bool `json:"safe"`
		HoldConfirmed        bool `json:"hold_confirmed"`
		PilotExitedCleanly   bool `json:"pilot_exited_cleanly"`
		AbilityStopRequested int  `json:"ability_stop_requested"`
		AbilityStopConfirmed int  `json:"ability_stop_confirmed"`
	} `json:"stop_evidence,omitempty"`
}

type managedRobotProcess struct {
	command *exec.Cmd
	done    chan error
}

// managedRobotInstanceLauncher 只把 Framework 的 Runtime Instance 转成已有
// semantic-robot-instance 的前台 supervisor 进程。Robot 的安全停止、Ability
// 与 AbilityFramework 的关闭顺序仍由 supervisor 负责；这里不直接操作
// MuJoCo Runtime，也不复制一套进程生命周期。
type managedRobotInstanceLauncher struct {
	robots *robotdomain.Service
	store  *store.Store
	logger *log.Logger

	serverHTTP string
	serverWS   string
	readiness  time.Duration
	shutdown   time.Duration

	mu        sync.Mutex
	processes map[string]*managedRobotProcess
}

func newManagedRobotInstanceLauncher(
	robots *robotdomain.Service,
	st *store.Store,
	logger *log.Logger,
	cfg config.RobotRuntimeConfig,
) *managedRobotInstanceLauncher {
	return &managedRobotInstanceLauncher{
		robots: robots, store: st, logger: logger,
		serverHTTP: strings.TrimRight(cfg.ServerHTTP, "/"),
		serverWS:   strings.TrimRight(cfg.ServerWS, "/"),
		readiness:  managedRobotReadinessTimeout,
		shutdown:   managedRobotShutdownTimeout,
		processes:  make(map[string]*managedRobotProcess),
	}
}

func (l *managedRobotInstanceLauncher) Start(
	ctx context.Context,
	request robotruntime.LaunchRequest,
) (robotruntime.LaunchResult, error) {
	launcher := filepath.Join(request.Bundle.Path, "bin", platform.Executable("semantic-robot-instance"))
	if info, err := os.Stat(launcher); err != nil || !platform.Runnable(info) {
		return robotruntime.LaunchResult{}, fmt.Errorf("Robot Runtime launcher 不可执行: %s", launcher)
	}
	if err := l.ensureRendered(ctx, launcher, request); err != nil {
		return robotruntime.LaunchResult{}, err
	}
	logPath := filepath.Join(request.Instance.DataDirectory, "run", "supervisor.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return robotruntime.LaunchResult{}, err
	}
	command := exec.Command(launcher, "run", "--instance", request.Instance.DataDirectory)
	// Server 和实例 supervisor 不能共享终端进程组。Ctrl-C 只应唤醒 Server，
	// 再由 App.shutdown 调用 Orchestrator.Stop；若两者同时收到信号，Pilot 与
	// AbilityFramework 会和 supervisor 竞争退出，留下 stopping 状态与孤儿进程。
	command.Dir = request.Instance.DataDirectory
	command.Stdout = logFile
	command.Stderr = logFile
	env, envErr := withInstanceTempDir(os.Environ(), request.Instance.DataDirectory)
	if envErr != nil {
		_ = logFile.Close()
		return robotruntime.LaunchResult{}, envErr
	}
	command.Env = env
	tree, err := processport.Start(command)
	if err != nil {
		_ = logFile.Close()
		return robotruntime.LaunchResult{}, fmt.Errorf("启动 Robot instance supervisor: %w", err)
	}
	process := &managedRobotProcess{command: command, done: make(chan error, 1)}
	l.mu.Lock()
	l.processes[request.Instance.InstanceID] = process
	l.mu.Unlock()
	go func() {
		waitErr := command.Wait()
		_ = tree.Close()
		_ = logFile.Close()
		process.done <- waitErr
		close(process.done)
	}()

	waitCtx, cancel := context.WithTimeout(ctx, l.readiness)
	defer cancel()
	if err := l.waitReady(waitCtx, request.Instance, process); err != nil {
		return robotruntime.LaunchResult{}, err
	}
	return robotruntime.LaunchResult{
		AbilityFrameworkEndpoint: fmt.Sprintf(
			"http://127.0.0.1:%d", request.Instance.AbilityFrameworkPort,
		),
	}, nil
}

func (l *managedRobotInstanceLauncher) ensureRendered(
	ctx context.Context,
	launcher string,
	request robotruntime.LaunchRequest,
) error {
	statePath := filepath.Join(request.Instance.DataDirectory, "run", "state.json")
	if _, err := os.Stat(statePath); err == nil {
		// reset 或场景再次启动时复用同一 Robot 的可写目录和专用 Pilot
		// credential。supervisor 会重新启动 AF/Ability/Pilot，不需要再次配对。
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	enrollment, err := l.robots.CreatePilotEnrollment("system:" + request.Instance.InstanceID)
	if err != nil {
		return fmt.Errorf("签发受管 Pilot enrollment: %w", err)
	}
	_, credential, err := l.robots.ClaimPilotEnrollment(
		enrollment.Code, request.Instance.PilotInstanceID,
	)
	if err != nil {
		return fmt.Errorf("领取受管 Pilot credential: %w", err)
	}
	configPath, err := l.writeInstanceConfig(request, credential)
	if err != nil {
		return err
	}
	if err := removeRenderBlockingTempDir(request.Instance.DataDirectory); err != nil {
		return err
	}
	command := exec.CommandContext(
		ctx, launcher, "render",
		"--config", configPath,
		"--output", request.Instance.DataDirectory,
	)
	env, envErr := withInstanceTempDir(os.Environ(), request.Instance.DataDirectory)
	if envErr != nil {
		return envErr
	}
	command.Env = env
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("render Robot Runtime instance: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (l *managedRobotInstanceLauncher) writeInstanceConfig(
	request robotruntime.LaunchRequest,
	credential string,
) (string, error) {
	var document robotInstanceConfig
	document.APIVersion = "semantic.insightos.cn/v1alpha1"
	document.Kind = "RobotInstance"
	document.Metadata.Name = request.Instance.InstanceID
	document.Spec.Bundle = request.Bundle.Path
	robot := &document.Spec.Robot
	robot.ID = request.Descriptor.RobotID
	robot.DisplayName = request.Descriptor.RobotID
	robot.Model = request.Descriptor.Model
	robot.Backend = request.Descriptor.Backend
	robot.BackendProfile = request.Descriptor.BackendProfile
	robot.SDKEndpoint = request.Descriptor.Endpoint
	robot.FirmwareProfile = request.Descriptor.BackendProfile
	robot.URDFPath = request.Descriptor.URDFPath
	robot.PackageDirectories = append([]string(nil), request.Descriptor.PackageDirectories...)
	robot.Options = cloneAnyMap(request.Descriptor.Configuration)
	if robot.Options == nil {
		robot.Options = make(map[string]any)
	}
	robot.SceneInstanceID = request.Descriptor.SceneInstanceID
	for _, descriptor := range request.Descriptor.Tools {
		tool := struct {
			ToolRef       string  `yaml:"toolRef"`
			Side          string  `yaml:"side"`
			Kind          string  `yaml:"kind"`
			Frame         string  `yaml:"frame"`
			Joint         string  `yaml:"joint"`
			TravelM       float64 `yaml:"travelM"`
			NormalForceN  float64 `yaml:"normalForceN"`
			MaximumForceN float64 `yaml:"maximumForceN"`
		}{descriptor.ToolRef, descriptor.Side, descriptor.Kind, descriptor.Frame, descriptor.Joint,
			descriptor.TravelM, descriptor.NormalForceN, descriptor.MaximumForceN}
		robot.Tools = append(robot.Tools, tool)
	}

	document.Spec.AbilityFramework.Endpoint = fmt.Sprintf(
		"http://127.0.0.1:%d", request.Instance.AbilityFrameworkPort,
	)
	document.Spec.AbilityFramework.Managed = true
	document.Spec.AbilityFramework.ListenAddress = "127.0.0.1"
	document.Spec.SemanticServer.HTTPURL = l.serverHTTP
	document.Spec.SemanticServer.WebSocketURL = l.serverWS
	document.Spec.SemanticServer.AccessToken = credential
	document.Spec.Pilot.ID = request.Instance.PilotInstanceID

	encoded, err := yaml.Marshal(document)
	if err != nil {
		return "", err
	}
	configDirectory := filepath.Join(
		filepath.Dir(request.Instance.DataDirectory), ".instance-configs",
	)
	if err := os.MkdirAll(configDirectory, 0o750); err != nil {
		return "", err
	}
	configPath := filepath.Join(configDirectory, request.Instance.InstanceID+".yaml")
	if err := os.WriteFile(configPath, encoded, 0o600); err != nil {
		return "", err
	}
	return configPath, nil
}

func (l *managedRobotInstanceLauncher) waitReady(
	ctx context.Context,
	instance robotruntime.RuntimeInstance,
	process *managedRobotProcess,
) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var lastReason string
	for {
		if ready, reason := l.pilotReady(instance); ready {
			l.logger.Info("受管 Robot 已就绪",
				"instance_id", instance.InstanceID,
				"pilot_id", instance.PilotInstanceID,
			)
			return nil
		} else {
			if reason != lastReason {
				// 这里只记录状态变化，不在 500ms 轮询中重复刷日志。启动超时
				// 往往发生在 Pilot、Ability 与 Skill 分阶段收敛时；保留精确
				// 阻塞原因才能区分真实离线和清理阶段造成的最终离线快照。
				l.logger.Info("等待受管 Robot 就绪",
					"instance_id", instance.InstanceID,
					"pilot_id", instance.PilotInstanceID,
					"reason", reason,
				)
			}
			lastReason = reason
		}
		select {
		case <-ctx.Done():
			if lastReason == "" {
				lastReason = ctx.Err().Error()
			}
			return fmt.Errorf("等待受管 Robot 就绪超时: %s", lastReason)
		case err := <-process.done:
			if err == nil {
				err = errors.New("supervisor 在 Robot ready 前退出")
			}
			return fmt.Errorf("Robot instance supervisor 提前退出: %w", err)
		case <-ticker.C:
		}
	}
}

func (l *managedRobotInstanceLauncher) pilotReady(
	instance robotruntime.RuntimeInstance,
) (bool, string) {
	if !l.robots.IsOnline(instance.PilotInstanceID) {
		return false, "Pilot 尚未上线"
	}
	pilot, err := l.store.GetRobotPilot(instance.PilotInstanceID)
	if err != nil {
		return false, "Pilot 状态尚未持久化"
	}
	if pilot.RobotID != instance.RobotID {
		return false, "Pilot 注册了不同 Robot"
	}
	if pilot.AbilityFrameworkStatus != "ready" {
		return false, "AbilityFramework 状态为 " + pilot.AbilityFrameworkStatus
	}
	// 七类 Ability 是当前 R1 Pro 类型包的部署内容，不是 Framework 平台契约。
	// supervisor 已按 bundle.yaml 启动并等待全部声明实例；Server 这里只确认
	// Pilot 实际发现了健康目录，避免再维护一份会阻碍其他 Robot 类型的数量常量。
	if len(pilot.Abilities) == 0 {
		return false, "Pilot 尚未发现 Ability 实例"
	}
	for _, ability := range pilot.Abilities {
		if fmt.Sprint(ability["health"]) != "healthy" {
			return false, "存在不健康 Ability"
		}
	}
	desired, err := l.store.ListRobotDesiredSkills(instance.RobotID)
	if err != nil {
		return false, "读取 desired Robot Skill 失败"
	}
	actual, err := l.store.ListRobotPilotSkills(instance.PilotInstanceID)
	if err != nil {
		return false, "读取 actual Robot Skill 失败"
	}
	actualByVersion := make(map[string]store.RobotPilotSkill, len(actual))
	for _, item := range actual {
		actualByVersion[item.Name+"\x00"+item.Version] = item
	}
	for _, target := range desired {
		item, exists := actualByVersion[target.Name+"\x00"+target.Version]
		label := target.Name + "@" + target.Version
		if !exists {
			return false, "Robot Skill 尚未安装: " + label
		}
		if item.Status != "installed" {
			return false, "Robot Skill 状态为 " + item.Status + ": " + label
		}
		if item.Enabled != target.Enabled {
			return false, "Robot Skill 启用状态尚未收敛: " + label
		}
	}
	return true, ""
}

func (l *managedRobotInstanceLauncher) Stop(
	ctx context.Context,
	instance robotruntime.RuntimeInstance,
	reason string,
) (robotruntime.StopEvidence, error) {
	if gone, goneEvidence := managedInstanceAlreadyGone(instance.DataDirectory, reason); gone {
		l.forgetManagedProcess(instance.InstanceID)
		return goneEvidence, nil
	}
	launcher, err := instanceLauncherFromData(instance.DataDirectory)
	if err != nil {
		return robotruntime.StopEvidence{}, err
	}
	timeout := l.shutdown
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}
	command := exec.CommandContext(
		ctx, launcher, "stop",
		"--instance", instance.DataDirectory,
		"--timeout", timeout.String(),
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return robotruntime.StopEvidence{}, fmt.Errorf(
			"停止受管 Robot instance: %w: %s", err, strings.TrimSpace(string(output)),
		)
	}
	state, err := readInstanceSupervisorState(instance.DataDirectory)
	if err != nil {
		return robotruntime.StopEvidence{}, err
	}
	evidence := state.StopEvidence
	if evidence == nil {
		return robotruntime.StopEvidence{Confirmed: false}, nil
	}
	confirmed := evidence.Safe && evidence.HoldConfirmed && evidence.PilotExitedCleanly &&
		evidence.AbilityStopConfirmed == evidence.AbilityStopRequested
	details := map[string]any{
		"safe":                   evidence.Safe,
		"hold_confirmed":         evidence.HoldConfirmed,
		"pilot_exited_cleanly":   evidence.PilotExitedCleanly,
		"ability_stop_requested": evidence.AbilityStopRequested,
		"ability_stop_confirmed": evidence.AbilityStopConfirmed,
		"reason":                 reason,
	}
	l.mu.Lock()
	delete(l.processes, instance.InstanceID)
	l.mu.Unlock()
	return robotruntime.StopEvidence{Confirmed: confirmed, Details: details}, nil
}

func instanceSiblingTempDir(dataDirectory string) string {
	return dataDirectory + ".tmp"
}

func removeRenderBlockingTempDir(dataDirectory string) error {
	entries, err := os.ReadDir(dataDirectory)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(entries) != 1 || entries[0].Name() != "tmp" {
		return nil
	}
	return os.RemoveAll(filepath.Join(dataDirectory, "tmp"))
}

func withInstanceTempDir(baseEnv []string, dataDirectory string) ([]string, error) {
	if strings.TrimSpace(dataDirectory) == "" {
		return nil, errors.New("实例数据目录为空，无法设置同盘 TMPDIR")
	}
	// TMPDIR 必须和实例目录同盘，但不能建在实例目录里面：render 要求输出目录为空。
	tmpDir := instanceSiblingTempDir(dataDirectory)
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return nil, fmt.Errorf("创建实例临时目录失败: %w", err)
	}
	env := make([]string, 0, len(baseEnv)+1)
	for _, item := range baseEnv {
		if strings.HasPrefix(item, "TMPDIR=") {
			continue
		}
		env = append(env, item)
	}
	return append(env, "TMPDIR="+tmpDir), nil
}

func (l *managedRobotInstanceLauncher) ReclaimInterruptedSimulation(
	ctx context.Context,
	instance robotruntime.RuntimeInstance,
	reason string,
) (robotruntime.StopEvidence, error) {
	if instance.Status != robotruntime.StateInterrupted || instance.Backend != "mujoco" {
		return robotruntime.StopEvidence{}, errors.New("仅允许回收 interrupted 的本机 MuJoCo 实例")
	}
	if gone, goneEvidence := managedInstanceAlreadyGone(instance.DataDirectory, reason); gone {
		l.forgetManagedProcess(instance.InstanceID)
		goneEvidence.Details["managed_simulation"] = true
		goneEvidence.Details["managed_reclaim"] = true
		return goneEvidence, nil
	}
	state, err := readInstanceSupervisorState(instance.DataDirectory)
	if err != nil {
		return robotruntime.StopEvidence{}, err
	}
	bundleLauncher, err := instanceLauncherFromData(instance.DataDirectory)
	if err != nil {
		return robotruntime.StopEvidence{}, err
	}
	bundleRoot := filepath.Dir(filepath.Dir(bundleLauncher))
	supervisorRunning, err := managedExecutableRunning(state.SupervisorPID, bundleLauncher)
	if err != nil {
		return robotruntime.StopEvidence{}, err
	}
	if supervisorRunning {
		return robotruntime.StopEvidence{}, errors.New("instance supervisor 仍在线，应使用正常安全停止")
	}

	// 旧 Scene 已由 SimulationService 销毁，仿真中的物理动作不可能继续存在。
	// 这里仍然只发送 SIGTERM，并按 Pilot→AbilityFramework 的顺序等待精确进程组
	// 退出；不使用 SIGKILL，也绝不能把这条恢复路径用于真机或远程 Runtime。
	if err := terminateManagedProcessGroup(
		ctx, state.PilotPID, filepath.Join(bundleRoot, "bin", platform.Executable("semantic-pilot")),
	); err != nil {
		return robotruntime.StopEvidence{}, fmt.Errorf("停止遗留 Pilot: %w", err)
	}
	if err := terminateManagedProcessGroup(
		ctx, state.AbilityFrameworkPID, filepath.Join(bundleRoot, "bin", platform.Executable("AbilityFramework")),
	); err != nil {
		return robotruntime.StopEvidence{}, fmt.Errorf("停止遗留 AbilityFramework: %w", err)
	}
	l.mu.Lock()
	delete(l.processes, instance.InstanceID)
	l.mu.Unlock()
	return robotruntime.StopEvidence{Confirmed: true, Details: map[string]any{
		"managed_simulation":       true,
		"managed_reclaim":          true,
		"supervisor_offline":       true,
		"orphan_processes_stopped": true,
		"reason":                   reason,
	}}, nil
}

func managedExecutableRunning(pid int, expectedExecutable string) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	actual, err := managedProcessExecutable(pid)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	expected, err := filepath.EvalSymlinks(expectedExecutable)
	if err != nil {
		return false, err
	}
	if !platform.SamePath(actual, expected) {
		return false, fmt.Errorf("PID %d executable不属于目标实例: %s", pid, actual)
	}
	return true, nil
}

func instanceLauncherFromData(dataDirectory string) (string, error) {
	content, err := os.ReadFile(filepath.Join(dataDirectory, "run", "bundle.json"))
	if err != nil {
		return "", err
	}
	var metadata struct {
		BundleRoot string `json:"bundle_root"`
	}
	if err := json.Unmarshal(content, &metadata); err != nil {
		return "", err
	}
	if strings.TrimSpace(metadata.BundleRoot) == "" {
		return "", errors.New("Robot instance bundle metadata 缺少 bundle_root")
	}
	return filepath.Join(metadata.BundleRoot, "bin", platform.Executable("semantic-robot-instance")), nil
}

func managedInstanceAlreadyGone(dataDirectory, reason string) (bool, robotruntime.StopEvidence) {
	_, err := os.Stat(filepath.Join(dataDirectory, "run", "state.json"))
	if err == nil || !os.IsNotExist(err) {
		return false, robotruntime.StopEvidence{}
	}
	return true, robotruntime.StopEvidence{
		Confirmed: true,
		Details: map[string]any{
			"managed_instance_absent": true,
			"reason":                  reason,
		},
	}
}

func (l *managedRobotInstanceLauncher) forgetManagedProcess(instanceID string) {
	l.mu.Lock()
	delete(l.processes, instanceID)
	l.mu.Unlock()
}

func readInstanceSupervisorState(dataDirectory string) (instanceSupervisorState, error) {
	content, err := os.ReadFile(filepath.Join(dataDirectory, "run", "state.json"))
	if err != nil {
		return instanceSupervisorState{}, err
	}
	var state instanceSupervisorState
	if err := json.Unmarshal(content, &state); err != nil {
		return state, err
	}
	return state, nil
}

type managedSceneRobotLifecycle struct {
	orchestrator *robotruntime.Orchestrator
	store        *store.Store
	robots       *robotdomain.Service
}

// Shutdown 只停止 Server 自己创建的 Robot 类型包实例，不停止外部共享的
// MuJoCo Runtime，也不改变 Scene 的生命周期。这里必须在 Pilot WS 关闭前执行，
// 否则 Robot 即使已经 hold，supervisor 也无法完成 Ability/AF 的确定性收尾。
func (l *managedSceneRobotLifecycle) Shutdown(ctx context.Context) error {
	instances, err := l.store.ListRuntimeInstances(ctx)
	if err != nil {
		return err
	}
	var result []error
	for _, instance := range instances {
		if !instance.Status.Active() || instance.Status == robotruntime.StateInterrupted {
			continue
		}
		currentExecutionID := ""
		if pilot, pilotErr := l.store.GetActiveRobotPilot(instance.RobotID); pilotErr == nil {
			currentExecutionID = pilot.CurrentExecutionID
		}
		stopped, stopErr := l.orchestrator.Stop(
			ctx, instance.InstanceID, "semantic-server shutdown",
		)
		if stopErr != nil {
			result = append(result, fmt.Errorf("%s: %w", instance.RobotID, stopErr))
			continue
		}
		if stopped.Status == robotruntime.StateStopped {
			// Orchestrator 只有在 supervisor 同步返回 safe/hold、Pilot 正常退出且
			// Ability 全部停止后才会进入 stopped。复用这份证据终结精确的当前
			// Execution；若没有证据，实例会保持 interrupted，绝不进入本分支。
			if convergeErr := l.robots.ConfirmManagedSimulationHold(
				ctx, instance.ProjectID, instance.RobotID, currentExecutionID,
				"semantic-server shutdown",
			); convergeErr != nil {
				result = append(result, fmt.Errorf(
					"%s: 收敛 Robot Execution: %w", instance.RobotID, convergeErr,
				))
			}
		}
	}
	return errors.Join(result...)
}

func (l *managedSceneRobotLifecycle) StartSceneRobots(
	ctx context.Context,
	projectID string,
	scene simulation.SceneInstance,
	robots []simulation.VirtualRobotDescriptor,
) error {
	errs := make(chan error, len(robots))
	var wait sync.WaitGroup
	for _, descriptor := range robots {
		descriptor := descriptor
		wait.Add(1)
		go func() {
			defer wait.Done()
			skip, prepareErr := l.prepareSceneRobotStart(ctx, scene, descriptor)
			if prepareErr != nil {
				errs <- fmt.Errorf("%s: %w", descriptor.RobotID, prepareErr)
				return
			}
			if skip {
				return
			}
			_, err := l.orchestrator.Start(ctx, robotruntime.StartRequest{
				InstanceID:      "robot-" + safeRuntimeID(scene.InstanceID+"-"+descriptor.RobotID),
				PilotInstanceID: "pilot-" + descriptor.RobotID,
				Descriptor:      descriptor,
				ProjectID:       projectID,
			})
			if err != nil {
				errs <- fmt.Errorf("%s: %w", descriptor.RobotID, err)
			}
		}()
	}
	wait.Wait()
	close(errs)
	var result []error
	for err := range errs {
		result = append(result, err)
	}
	return errors.Join(result...)
}

// prepareSceneRobotStart 收敛同一 Robot ID 的旧受管实例。场景 Robot 的 source_id
// 可以跨 Layout 保持稳定，但旧 Pilot/Ability 绝不能因此继续绑定旧 endpoint。
// 先尝试普通安全停止；只有普通停止已把实例明确置为 interrupted，且新的 running
// Scene 已经证明旧物理世界被替换后，才进入受管 MuJoCo 孤儿回收。这个分支不会
// 用于真机，也不会把停止错误伪装成 ready。
func (l *managedSceneRobotLifecycle) prepareSceneRobotStart(
	ctx context.Context,
	scene simulation.SceneInstance,
	descriptor simulation.VirtualRobotDescriptor,
) (bool, error) {
	existing, err := l.store.GetActiveRuntimeByRobot(ctx, descriptor.RobotID)
	if errors.Is(err, robotruntime.ErrInstanceNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("查询活动 Runtime: %w", err)
	}
	if existing.SceneInstanceID == scene.InstanceID {
		// 同一个 Scene 的 ready/starting 实例说明重复收到了启动通知，可以安全
		// 幂等返回；interrupted/stopping 却不是“已经启动”。把它们静默跳过会让
		// Web 永远看到旧 Pilot 离线，而启动请求表面上没有错误。这里保留明确的
		// Runtime 失败状态，等待用户先停止 Scene（Runtime hold 后可回收）再重试。
		if existing.Status == robotruntime.StateStarting ||
			existing.Status == robotruntime.StateReady ||
			existing.Status == robotruntime.StateDegraded {
			return true, nil
		}
		return false, fmt.Errorf(
			"同一场景的 Robot Runtime %s 仍处于 %s，请先停止场景后重试",
			existing.InstanceID, existing.Status)
	}
	if existing.Backend != "mujoco" {
		return false, fmt.Errorf("Robot 已被非受管仿真实例 %s 占用", existing.InstanceID)
	}

	current := existing
	if current.Status != robotruntime.StateInterrupted {
		stopped, stopErr := l.orchestrator.Stop(ctx, current.InstanceID, "启动新场景前停止旧受管实例")
		current = stopped
		if stopErr != nil && current.Status != robotruntime.StateInterrupted {
			return false, fmt.Errorf("停止旧受管实例: %w", stopErr)
		}
	}
	if current.Status == robotruntime.StateInterrupted {
		if _, reclaimErr := l.orchestrator.ReclaimInterruptedSimulation(
			ctx, current.InstanceID, scene.InstanceID, false, "旧受管仿真场景已被替换",
		); reclaimErr != nil {
			return false, fmt.Errorf("回收旧受管仿真实例: %w", reclaimErr)
		}
	}
	return false, nil
}

func (l *managedSceneRobotLifecycle) HoldSceneRobots(
	ctx context.Context,
	projectID string,
	sceneInstanceID string,
	reason string,
) error {
	instances, err := l.store.ListRuntimeInstances(ctx)
	if err != nil {
		return err
	}
	var result []error
	for _, instance := range instances {
		if instance.SceneInstanceID != sceneInstanceID || !instance.Status.Active() {
			continue
		}
		pilot, pilotErr := l.store.GetActiveRobotPilot(instance.RobotID)
		closingScene := reason == "scene_stop"
		if pilotErr != nil || !l.robots.IsOnline(pilot.PilotInstanceID) {
			// Runtime hold 已在进入本方法前同步确认。stop 随后还会退出整个
			// 受管实例，因此 Pilot 断线不应再次阻塞 AF 清理；reset 会保留
			// 进程，仍要求 Pilot 在线并完成 Worker/Execution 收敛。
			if !closingScene {
				result = append(result, fmt.Errorf("%s: Pilot 不在线，无法在 reset 后保留实例", instance.RobotID))
			}
			continue
		}
		if pilot.CurrentExecutionID != "" {
			if closingScene {
				// SimulationService 已经通过同一 Scene Runtime 同步确认底盘、
				// 双臂和工具全部进入 hold。场景 stop 接下来会退出 Pilot、Ability
				// 和 AF，并用这份 Runtime 物理证据收敛 Execution，因此这里只发送
				// 一次正式 execution.stop，不能无界等待一个 waiting_agent Worker
				// 自己恢复；否则前端停止会永久占住 SimulationService 状态锁。
				_, _ = l.robots.Stop(ctx, projectID, pilot.CurrentExecutionID, reason)
				continue
			}
			if _, stopErr := l.robots.StopAndWait(ctx, projectID, pilot.CurrentExecutionID, reason); stopErr != nil {
				result = append(result, fmt.Errorf("%s: %w", instance.RobotID, stopErr))
			}
			continue
		}
		if pilot.RobotStatus != "idle" && !closingScene {
			result = append(result, fmt.Errorf("%s: Robot 状态 %s，无法确认没有活动物理动作",
				instance.RobotID, pilot.RobotStatus))
		}
	}
	return errors.Join(result...)
}

func (l *managedSceneRobotLifecycle) StopSceneRobots(
	ctx context.Context,
	projectID string,
	sceneInstanceID string,
	reason string,
) error {
	instances, err := l.store.ListRuntimeInstances(ctx)
	if err != nil {
		return err
	}
	var result []error
	for _, instance := range instances {
		if instance.SceneInstanceID != sceneInstanceID || !instance.Status.Active() {
			continue
		}
		currentExecutionID := ""
		if pilot, pilotErr := l.store.GetActiveRobotPilot(instance.RobotID); pilotErr == nil {
			currentExecutionID = pilot.CurrentExecutionID
		}
		stopped, stopErr := l.orchestrator.Stop(ctx, instance.InstanceID, reason)
		if stopErr != nil {
			if stopped.Status != robotruntime.StateInterrupted || stopped.Backend != "mujoco" {
				result = append(result, fmt.Errorf("%s: %w", instance.RobotID, stopErr))
				continue
			}
			// SimulationService 在进入这里前已经从同一个 Scene Runtime 同步
			// 取得 hold 成功。若 Pilot 已失联，supervisor 会按通用安全规则
			// 保留 Ability/AF 并返回 interrupted；对受管 MuJoCo 场景可以用
			// 这份物理证据回收孤儿进程。真机不会进入该分支。
			stopped, stopErr = l.orchestrator.ReclaimInterruptedSimulation(
				ctx, stopped.InstanceID, stopped.SceneInstanceID, true,
				"Runtime hold 已确认后清理受管仿真进程",
			)
			if stopErr != nil {
				result = append(result, fmt.Errorf("%s: 回收受管仿真实例: %w", instance.RobotID, stopErr))
				continue
			}
		}
		if stopped.Status == robotruntime.StateStopped {
			if convergeErr := l.robots.ConfirmManagedSimulationHold(
				ctx, projectID, instance.RobotID, currentExecutionID, reason,
			); convergeErr != nil {
				result = append(result, fmt.Errorf("%s: 收敛 Robot Execution: %w", instance.RobotID, convergeErr))
			}
		}
	}
	return errors.Join(result...)
}

func newManagedSceneRobotLifecycle(
	cfg *config.Config,
	st *store.Store,
	robots *robotdomain.Service,
	logger *log.Logger,
) (*managedSceneRobotLifecycle, error) {
	if !cfg.RobotRuntime.Enabled {
		return nil, nil
	}
	catalog, err := robotruntime.LoadCatalog(cfg.RobotRuntime.BundlesDir)
	if err != nil {
		return nil, err
	}
	launcher := newManagedRobotInstanceLauncher(robots, st, logger, cfg.RobotRuntime)
	orchestrator, err := robotruntime.NewOrchestrator(robotruntime.OrchestratorConfig{
		Catalog:   catalog,
		Store:     st,
		Ports:     st,
		Launcher:  launcher,
		Events:    robots,
		DataRoot:  cfg.RobotRuntime.DataRoot,
		PortFirst: cfg.RobotRuntime.PortFirst,
		PortLast:  cfg.RobotRuntime.PortLast,
	})
	if err != nil {
		return nil, err
	}
	return &managedSceneRobotLifecycle{orchestrator: orchestrator, store: st, robots: robots}, nil
}

func safeRuntimeID(value string) string {
	value = strings.Map(func(character rune) rune {
		switch {
		case character >= 'a' && character <= 'z':
			return character
		case character >= 'A' && character <= 'Z':
			return character
		case character >= '0' && character <= '9':
			return character
		case character == '-', character == '_':
			return character
		default:
			return '-'
		}
	}, value)
	return strings.Trim(strings.TrimSpace(value), "-")
}

func cloneAnyMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

var _ robotruntime.InstanceLauncher = (*managedRobotInstanceLauncher)(nil)
var _ robotruntime.InterruptedSimulationCleaner = (*managedRobotInstanceLauncher)(nil)
var _ simulation.RobotLifecycleCoordinator = (*managedSceneRobotLifecycle)(nil)
