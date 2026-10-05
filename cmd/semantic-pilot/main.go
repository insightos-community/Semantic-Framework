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

// semantic-pilot 入口：设备端代理，向 semantic-server 上报能力。
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	stopport "insightos.cn/semantic-framework/internal/ports/stop"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"insightos.cn/semantic-framework/internal/pilot"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/log"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "skill" {
		if err := runLocalSkillCommand(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "semantic-pilot 本地 Skill 调试失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	var (
		python       = flag.String("python", "python", "运行 Robot Skill Worker 的 Python")
		profilePath  = flag.String("profile", envFirst("SEMANTIC_ROBOT_CONFIG", "SEMANTIC_ROBOT_DEPLOYMENT"), "统一 RobotDeployment YAML")
		serverWS     = flag.String("server-ws", os.Getenv("SEMANTIC_PILOT_SERVER_WS"), "Semantic Server /ws/pilot 地址")
		serverHTTP   = flag.String("server-http", os.Getenv("SEMANTIC_PILOT_SERVER_HTTP"), "Semantic Server HTTP 地址")
		accessToken  = flag.String("access-token", os.Getenv("SEMANTIC_PILOT_ACCESS_TOKEN"), "Pilot 访问令牌")
		pilotID      = flag.String("pilot-id", os.Getenv("SEMANTIC_PILOT_ID"), "稳定 Pilot 实例 ID")
		dataDir      = flag.String("data-dir", ".output/semantic-pilot", "Pilot SQLite、Skill 和 Artifact 目录")
		skillStorage = flag.String("skill-storage", "", "覆盖 RobotDeployment 中的 Robot Skill 存储目录")
		skillSDK     = flag.String("skill-sdk-source", os.Getenv("SEMANTIC_ROBOT_SKILL_SDK"), "Robot Skill SDK wheel 或源码目录")
		skillWheels  = flag.String("skill-wheelhouse", os.Getenv("SEMANTIC_ROBOT_SKILL_WHEELHOUSE"), "Robot Skill 依赖 Wheel 目录，默认从 --skill-sdk-source 推导")
		pilotVersion = flag.String("pilot-version", "0.5.0-dev", "上报给 Server 的 Pilot 版本")
	)
	flag.Parse()

	if err := runPermanent(*profilePath, *serverWS, *serverHTTP, *accessToken, *pilotID,
		*dataDir, *skillStorage, *skillSDK, *skillWheels, *python, *pilotVersion); err != nil {
		fmt.Fprintf(os.Stderr, "semantic-pilot 启动失败: %v\n", err)
		os.Exit(1)
	}
}

func runPermanent(profilePath, serverWS, serverHTTP, accessToken, pilotID, dataDir,
	skillStorage, skillSDK, skillWheelhouse, python, pilotVersion string) error {
	ctx, stop, err := stopport.NotifyContext(context.Background())
	if err != nil {
		return err
	}
	defer stop()
	return runPermanentContext(ctx, profilePath, serverWS, serverHTTP, accessToken, pilotID,
		dataDir, skillStorage, skillSDK, skillWheelhouse, python, pilotVersion)
}

func runPermanentContext(ctx context.Context, profilePath, serverWS, serverHTTP, accessToken, pilotID, dataDir,
	skillStorage, skillSDK, skillWheelhouse, python, pilotVersion string) error {
	if strings.TrimSpace(profilePath) == "" {
		return errors.New("--profile 必填")
	}
	deployment, err := pilot.LoadRobotDeployment(profilePath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(pilotID) == "" {
		pilotID = strings.TrimSpace(deployment.Pilot.ID)
	}
	for name, value := range map[string]string{
		"--server-ws": serverWS, "--server-http": serverHTTP, "--access-token": accessToken,
		"pilot.id": pilotID, "--skill-sdk-source": skillSDK,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s 必填", name)
		}
	}
	if skillStorage == "" {
		skillStorage = deployment.Pilot.RobotSkillDirectory
	}
	if skillStorage == "" {
		skillStorage = filepath.Join(dataDir, "skills")
	}
	activeDirectory := filepath.Join(skillStorage, "active")
	for _, directory := range []string{dataDir, activeDirectory, filepath.Join(skillStorage, "packages"),
		filepath.Join(skillStorage, "environments"), filepath.Join(skillStorage, "staging")} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return err
		}
	}

	logger := log.New(log.Options{Level: log.LevelInfo})
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "pilot.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return err
	}
	persisted, err := pilot.NewSQLiteStore(db)
	if err != nil {
		return err
	}
	skills, err := pilot.ScanSkillCatalog(activeDirectory)
	if err != nil {
		return err
	}
	actionCatalog := pilot.NewCatalog()
	abilityClient := pilot.NewAbilityFrameworkClient(deployment.AbilityFramework.Endpoint, nil)
	discovery := pilot.NewAbilityFrameworkDiscovery(deployment, abilityClient, actionCatalog, logger)
	if err := discovery.Refresh(context.Background()); err != nil {
		logger.WithError(err).Warn("AbilityFramework 尚未就绪，Pilot 将以 offline 状态注册并继续轮询")
	}
	runner := pilot.NewRunner(actionCatalog, abilityClient, persisted)
	// SDK 以 wheel 形式发布时，bundle 的 wheels 目录就是 Skill 环境的离线
	// wheelhouse；源码目录（开发场景）保持 pip 默认解析行为。
	wheelhouse := strings.TrimSpace(skillWheelhouse)
	if wheelhouse == "" && strings.HasSuffix(strings.ToLower(skillSDK), ".whl") {
		wheelhouse = filepath.Dir(skillSDK)
	}
	installer := pilot.VenvSkillInstaller{BaseDirectory: filepath.Join(skillStorage, "environments"),
		PythonExecutable: python, SDKSource: skillSDK, Wheelhouse: wheelhouse,
		UVExecutable: os.Getenv("SEMANTIC_SKILL_UV"), EnvironmentRoot: os.Getenv("SEMANTIC_SKILL_ENV_ROOT")}
	supervisor := pilot.WorkerSupervisor{Installer: installer, PythonExecutable: python,
		OnStderrLine: func(line string) { logger.Warn("Robot Skill Worker stderr", "line", line) }}
	agent := pilot.NewRemoteAgentGateway(nil)
	runtime := pilot.NewSkillRuntime(skills, actionCatalog, runner, persisted, supervisor, agent, nil, nil)
	manager := &pilot.InstalledSkillManager{BaseDirectory: skillStorage, Catalog: skills,
		Installer: installer, InUse: runtime.SkillInUse}
	artifacts := pilot.NewArtifactStore(filepath.Join(dataDir, "artifacts"), serverHTTP)
	artifacts.AbilityExchangeDirectory = os.Getenv("SEMANTIC_ABILITY_ARTIFACT_ROOT")
	heartbeat := time.Duration(deployment.Pilot.HeartbeatIntervalSecond) * time.Second
	remote := pilot.NewRemoteClient(pilot.RemoteClientConfig{
		ServerWebSocketURL: serverWS, ServerHTTPBaseURL: serverHTTP, AccessToken: accessToken,
		HeartbeatInterval: heartbeat, AbilityStatus: discovery.Snapshot,
		Pilot: store.RobotPilot{PilotInstanceID: pilotID, RobotID: deployment.Robot.ID,
			DisplayName: deployment.Robot.DisplayName, RobotModel: deployment.Robot.Model,
			Backend: deployment.Robot.Backend, PilotVersion: pilotVersion, Status: "connecting",
			RobotStatus: "idle", AbilityFrameworkStatus: discovery.Snapshot().Status,
			Configuration: deployment.DeviceConfiguration(), DesiredSkills: deployment.DesiredSkills()},
	}, runtime, persisted, manager, artifacts, agent, logger)
	runtime.SetEventSink(&pilot.RemoteRuntimeEventSink{Client: remote})
	debug := pilot.NewAbilityDebugService(deployment.Robot.ID, deployment.Pilot.AllowAbilityDebug, abilityClient, persisted)
	debug.RobotBusy = runtime.RobotInUse
	remote.SetAbilityDebugService(debug)

	if err := runtime.Recover(ctx); err != nil {
		return fmt.Errorf("恢复 Robot Skill: %w", err)
	}
	if err := debug.Recover(); err != nil {
		return fmt.Errorf("恢复 Ability debug: %w", err)
	}
	go discovery.Run(ctx)
	logger.Info("semantic-pilot 已启动", "pilot_id", pilotID, "robot_id", deployment.Robot.ID,
		"server_ws", serverWS, "ability_framework", deployment.AbilityFramework.Endpoint)
	runErr := remote.Run(ctx)
	stopTimeout := 15 * time.Second
	if deployment.Pilot.WorkerTimeoutSeconds > 0 {
		stopTimeout = time.Duration(deployment.Pilot.WorkerTimeoutSeconds) * time.Second
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), stopTimeout)
	defer shutdownCancel()
	stopReport, shutdownErr := stopActivePilotWork(shutdownCtx, persisted, runtime, debug)
	if reportErr := writePilotStopReport(os.Getenv("SEMANTIC_PILOT_STOP_REPORT"), stopReport); reportErr != nil {
		shutdownErr = errors.Join(shutdownErr, fmt.Errorf("写入 Pilot 停止证据: %w", reportErr))
	}
	if shutdownErr != nil {
		logger.WithError(shutdownErr).Error("semantic-pilot 未能确认 Robot 安全停止")
	}
	return errors.Join(runErr, shutdownErr)
}

type activePilotWorkStore interface {
	ListRecoverableSkillExecutions() ([]pilot.SkillExecution, error)
	ListActiveAbilityDebug() ([]pilot.AbilityDebugExecution, error)
	ListActiveActions() ([]pilot.ActionExecution, error)
}

type skillWorkStopper interface {
	Stop(context.Context, string, string, string, string) (pilot.SkillExecution, error)
}

type debugWorkStopper interface {
	Stop(context.Context, string, string) (pilot.AbilityDebugExecution, error)
}

type pilotStopReport struct {
	ExecutionID       string    `json:"execution_id,omitempty"`
	Safe              bool      `json:"safe"`
	HoldConfirmed     bool      `json:"hold_confirmed"`
	ActiveInvocations []string  `json:"active_invocations"`
	Reason            string    `json:"reason"`
	FinishedAt        time.Time `json:"finished_at"`
}

// stopActivePilotWork 是 Pilot 退出前的最后一道物理安全边界。即使一个停止失败，
// 也继续尝试其余工作；最终错误使进程非零退出，Server 不会把实例记成 stopped。
func stopActivePilotWork(ctx context.Context, store activePilotWorkStore, skills skillWorkStopper,
	debug debugWorkStopper) (pilotStopReport, error) {
	report := pilotStopReport{ActiveInvocations: []string{}}
	var failures []error
	actions, err := store.ListActiveActions()
	if err != nil {
		failures = append(failures, fmt.Errorf("读取活动 Ability invocation: %w", err))
	} else {
		for _, action := range actions {
			if action.InvocationID != "" {
				report.ActiveInvocations = append(report.ActiveInvocations, action.InvocationID)
			}
		}
	}
	executions, err := store.ListRecoverableSkillExecutions()
	if err != nil {
		failures = append(failures, fmt.Errorf("读取活动 Robot Skill: %w", err))
	} else {
		for _, execution := range executions {
			if report.ExecutionID == "" {
				report.ExecutionID = execution.ID
			}
			result, stopErr := skills.Stop(ctx, execution.ID, "runtime", "semantic-pilot shutdown", "safe")
			if stopErr != nil {
				failures = append(failures, fmt.Errorf("停止 Skill %s: %w", execution.ID, stopErr))
				continue
			}
			if result.Status != pilot.SkillStopped && result.Status != pilot.SkillCompleted {
				failures = append(failures, fmt.Errorf("停止 Skill %s 未确认安全状态: %s", execution.ID, result.Status))
			} else if result.Status == pilot.SkillStopped {
				safe, _ := result.StopOutcome["safe"].(bool)
				if !safe {
					failures = append(failures, fmt.Errorf("停止 Skill %s 缺少 Robot hold 或无活动动作证据", execution.ID))
				}
			}
		}
	}
	debugExecutions, err := store.ListActiveAbilityDebug()
	if err != nil {
		failures = append(failures, fmt.Errorf("读取活动 Ability debug: %w", err))
	} else {
		for _, execution := range debugExecutions {
			result, stopErr := debug.Stop(ctx, execution.ID, "semantic-pilot shutdown")
			if stopErr != nil {
				failures = append(failures, fmt.Errorf("停止 Ability debug %s: %w", execution.ID, stopErr))
				continue
			}
			if result.Status != "stopped" && result.Status != "succeeded" {
				failures = append(failures, fmt.Errorf("停止 Ability debug %s 未确认安全状态: %s", execution.ID, result.Status))
			}
		}
	}
	stopErr := errors.Join(failures...)
	report.Safe = stopErr == nil
	report.HoldConfirmed = report.Safe
	report.FinishedAt = time.Now().UTC()
	if stopErr == nil {
		report.Reason = "Robot 已进入 hold，或已确认没有活动物理动作"
	} else {
		report.Reason = stopErr.Error()
	}
	return report, stopErr
}

func writePilotStopReport(path string, report pilotStopReport) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(encoded, '\n'), 0o640); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	return nil
}

func envFirst(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}
