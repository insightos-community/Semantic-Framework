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

package integration

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/bootstrap"
	"insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/log"
)

const v050GateRobotID = "r1pro-test"

type v050GateScope struct {
	project  store.Project
	workflow store.WorkflowView
	session  store.ChatSession
}

// TestV050RealServerPilotGate 只在 make test-v050-real-gate 显式提供真实
// AbilityFramework、Pilot 和 Skill 路径时运行。模型虽使用确定性脚本，但仍经
// Kernel、正式 robot.run/robot.stop 工具和 Server Robot Service 下发命令；测试
// 不直接调用 Pilot Runtime，也不会伪造 Ability 结果。
func TestV050RealServerPilotGate(t *testing.T) {
	endpoint := os.Getenv("SEMANTIC_REAL_ABILITY_FRAMEWORK_ENDPOINT")
	pilotBinary := os.Getenv("SEMANTIC_PILOT_BINARY")
	profilePath := os.Getenv("SEMANTIC_ROBOT_CONFIG")
	skillsRepository := os.Getenv("SEMANTIC_ROBOT_SKILLS_DIR")
	skillPython := os.Getenv("SEMANTIC_ROBOT_SKILL_PYTHON")
	skillSDK := os.Getenv("SEMANTIC_ROBOT_SKILL_SDK")
	if endpoint == "" || pilotBinary == "" || profilePath == "" || skillsRepository == "" || skillPython == "" || skillSDK == "" {
		t.Skip("real v0.5 gate environment is not configured")
	}
	outputDirectory := os.Getenv("SEMANTIC_GATE_OUTPUT_DIR")
	if outputDirectory == "" {
		outputDirectory = t.TempDir()
	}
	if err := os.MkdirAll(filepath.Join(outputDirectory, "executions"), 0o750); err != nil {
		t.Fatal(err)
	}

	httpBase, wsBase, app, stopServer := startChatApp(t, filepath.Join(t.TempDir(), "server.db"))
	defer stopServer()
	token := login(t, httpBase)

	pilotContext, stopPilot := context.WithCancel(context.Background())
	pilotWait := startV050GatePilot(t, pilotContext, pilotBinary, profilePath, skillsRepository,
		skillPython, skillSDK, wsBase+"/ws/pilot", httpBase, token, outputDirectory)
	defer func() {
		stopPilot()
		select {
		case <-pilotWait:
		case <-time.After(15 * time.Second):
			t.Errorf("semantic-pilot did not exit in time")
		}
	}()
	waitV050PilotReady(t, app.Store(), app.Robots(), "pilot-v050-real-gate")

	scope := createV050GateTask(t, app.Store())
	if os.Getenv("SEMANTIC_REAL_GATE_MODE") == "stop" {
		runV050StopGate(t, app, scope, outputDirectory)
		return
	}
	runV050NormalGate(t, app, scope, outputDirectory)
}

func startV050GatePilot(t *testing.T, ctx context.Context, binary, profilePath, skillsRepository,
	python, skillSDK, serverWS, serverHTTP, token, outputDirectory string) <-chan error {
	t.Helper()
	storage := filepath.Join(t.TempDir(), "skills")
	active := filepath.Join(storage, "active")
	if err := os.MkdirAll(active, 0o750); err != nil {
		t.Fatal(err)
	}
	skillRoot := filepath.Join(skillsRepository, "semantic_robot_skills", "skills")
	for directory, name := range map[string]string{
		"grasp_object": "grasp-object", "semantic_navigation": "semantic-navigation", "place_object": "place-object",
	} {
		if err := os.Symlink(filepath.Join(skillRoot, directory), filepath.Join(active, directory)); err != nil {
			t.Fatalf("prepare active skill %s: %v", name, err)
		}
		// Gate 的隔离 Python 环境已装正式 Skill SDK。每个 Skill 环境只放一个
		// 很薄的解释器入口，显式转发到该环境；这样不重复下载依赖，同时 Pilot
		// 仍按 environments/<name>/<version> 选择 Worker。
		environment := filepath.Join(storage, "environments", name, "0.1.0")
		if err := os.MkdirAll(filepath.Join(environment, "bin"), 0o750); err != nil {
			t.Fatal(err)
		}
		wrapper := "#!/bin/sh\nexec \"" + strings.ReplaceAll(python, "\"", "\\\"") + "\" \"$@\"\n"
		if err := os.WriteFile(filepath.Join(environment, "bin", "python"), []byte(wrapper), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(environment, ".semantic-ready"), []byte(name+"@0.1.0\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	pilotLog, err := os.OpenFile(filepath.Join(outputDirectory, "pilot.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, binary,
		"--profile", profilePath,
		"--server-ws", serverWS,
		"--server-http", serverHTTP,
		"--access-token", token,
		"--pilot-id", "pilot-v050-real-gate",
		"--data-dir", filepath.Join(storage, "pilot-data"),
		"--skill-storage", storage,
		"--skill-sdk-source", skillSDK,
		"--python", python,
	)
	command.Stdout, command.Stderr = pilotLog, pilotLog
	command.Env = append(os.Environ(), "SEMANTIC_ROBOT_CONFIG="+profilePath)
	if err := command.Start(); err != nil {
		_ = pilotLog.Close()
		t.Fatalf("start semantic-pilot: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- command.Wait()
		_ = pilotLog.Close()
	}()
	return done
}

func waitV050PilotReady(t *testing.T, st *store.Store, robots *robot.Service, pilotID string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		pilot, err := st.GetRobotPilot(pilotID)
		skills, skillsErr := st.ListRobotPilotSkills(pilotID)
		healthyAbilities := 0
		for _, ability := range pilot.Abilities {
			healthy, _ := ability["healthy"].(bool)
			selected, _ := ability["selected"].(bool)
			if healthy && selected {
				healthyAbilities++
			}
		}
		if err == nil && skillsErr == nil && robots.IsOnline(pilotID) &&
			pilot.AbilityFrameworkStatus == "ready" && healthyAbilities == 7 {
			enabled := 0
			for _, item := range skills {
				if item.Enabled && item.Status == "installed" {
					enabled++
				}
			}
			if enabled == 3 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	pilot, _ := st.GetRobotPilot(pilotID)
	skills, _ := st.ListRobotPilotSkills(pilotID)
	t.Fatalf("Pilot or seven Ability instances did not become ready: pilot=%#v skills=%#v", pilot, skills)
}

func createV050GateTask(t *testing.T, st *store.Store) v050GateScope {
	t.Helper()
	const ownerID = "v050-gate-user"
	project, err := st.CreateProject(ownerID, "v0.5 Robot Gate")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	session := store.ChatSession{ID: store.NewChatSessionID(), UserID: ownerID, ProjectID: project.ID,
		Title: "脚本化 Robot Agent", CreatedAt: now, UpdatedAt: now, Revision: 1}
	if err := st.CreateChatSession(session); err != nil {
		t.Fatal(err)
	}
	draft := store.WorkflowDraft{
		Goal: "依次完成抓取、携物导航和放置",
		Tasks: []store.TaskDraft{{ID: "task-v050-real-gate", RequiredRole: "robot",
			Goal: "执行拆码垛流程", Input: json.RawMessage(`{"robot_id":"r1pro-test"}`),
			SubTasks: []store.SubTaskDraft{
				{ID: "subtask-v050-grasp", Kind: "robot_skill", Goal: "抓取箱体"},
				{ID: "subtask-v050-navigation", Kind: "robot_skill", Goal: "携物导航"},
				{ID: "subtask-v050-place", Kind: "robot_skill", Goal: "放置箱体"},
			}}},
	}
	ready, err := st.SubmitPlanProposal(project.ID, session.ID, draft, "", nil,
		"# Robot Gate", now)
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := st.ApprovePlanProposal(ready.ID, ready.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	return v050GateScope{project: project, workflow: workflow, session: session}
}

func runV050NormalGate(t *testing.T, app *bootstrap.App, scope v050GateScope, outputDirectory string) {
	t.Helper()
	task := scope.workflow.Tasks[0]
	grasp := runV050RobotSkill(t, app, scope, task, "subtask-v050-grasp", "grasp-object", map[string]any{
		"object_ref": "object://pallet-a/box-17", "gripper_ref": "component://gripper/main", "preferred_strategy": "top",
	}, "v050-gate-grasp")
	held, ok := grasp.Result["held_object"].(map[string]any)
	if !ok {
		t.Fatalf("grasp result lacks held_object: %#v", grasp.Result)
	}
	navigation := runV050RobotSkill(t, app, scope, task, "subtask-v050-navigation", "semantic-navigation", map[string]any{
		"target": map[string]any{"target_ref": "semantic://pallet-b/preplace", "pose": v050GatePose(2.2, 1, 0),
			"map_type": "simulation", "map_generation": "generation-1", "map_revision": 1},
		"navigation_purpose": "carry_to_place", "carrying_object": held, "require_visual_confirmation": true,
	}, "v050-gate-navigation")
	carried, ok := navigation.Result["carrying_object"].(map[string]any)
	if !ok {
		t.Fatalf("navigation result lacks carrying_object: %#v", navigation.Result)
	}
	placement := runV050RobotSkill(t, app, scope, task, "subtask-v050-place", "place-object", map[string]any{
		"held_object": carried,
		"target": map[string]any{"target_ref": "slot://pallet-b/cell-07", "region_ref": "region://pallet-b",
			"support_surface_ref": "surface://pallet-b", "stability_duration_ms": 800,
			"expected_scene_revision": "scene-place-42"},
	}, "v050-gate-place")
	for name, execution := range map[string]store.RobotExecution{"grasp": grasp, "navigation": navigation, "place": placement} {
		assertV050AbilityEvidence(t, app.Store(), execution)
		writeV050ExecutionEvidence(t, app.Store(), filepath.Join(outputDirectory, "executions", name+".json"), execution)
	}
}

func runV050StopGate(t *testing.T, app *bootstrap.App, scope v050GateScope, outputDirectory string) {
	t.Helper()
	task := scope.workflow.Tasks[0]
	requestKey := "v050-gate-stop-navigation"
	input := map[string]any{
		"target": map[string]any{"target_ref": "semantic://safe-stop-target", "pose": v050GatePose(4, 0, 0),
			"map_type": "simulation", "map_generation": "generation-1", "map_revision": 1},
		"navigation_purpose": "transit", "require_visual_confirmation": false,
	}
	invokeV050RobotAgent(t, app.ToolRegistry(), v050ExecutionScope(scope, task, "subtask-v050-navigation"),
		"robot.run", map[string]any{"skill_name": "semantic-navigation", "skill_version": "0.1.0", "input": input, "request_key": requestKey})
	execution := waitV050Execution(t, app.Store(), scope.project.ID, requestKey, false)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		events, _ := app.Store().ListRobotExecutionEvents(execution.ID, 0, 500)
		for _, event := range events {
			if event.Type == "action.started" && event.Payload["action_type"] == "navigation.follow_route" {
				goto stop
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("navigation did not enter FollowRoute: %#v", execution)

stop:
	invokeV050RobotAgent(t, app.ToolRegistry(), v050ExecutionScope(scope, task, "subtask-v050-navigation"),
		"robot.stop", map[string]any{"execution_id": execution.ID, "reason": "v0.5 Gate 安全停止"})
	execution = waitV050Execution(t, app.Store(), scope.project.ID, requestKey, true)
	if execution.Status != "stopped" {
		t.Fatalf("robot.stop did not produce stopped: %#v", execution)
	}
	events, err := app.Store().ListRobotExecutionEvents(execution.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	confirmed := false
	for _, event := range events {
		if event.Type != "skill.stop.finalized" {
			continue
		}
		outcome, _ := event.Payload["stop_outcome"].(map[string]any)
		confirmed, _ = outcome["safe"].(bool)
	}
	if !confirmed {
		t.Fatalf("stop execution lacks SDK hold evidence: %#v", events)
	}
	writeV050ExecutionEvidence(t, app.Store(), filepath.Join(outputDirectory, "executions", "safe-stop.json"), execution)
}

func runV050RobotSkill(t *testing.T, app *bootstrap.App, scope v050GateScope, task store.Task,
	subtaskID, skillName string, input map[string]any, requestKey string) store.RobotExecution {
	t.Helper()
	invokeV050RobotAgent(t, app.ToolRegistry(), v050ExecutionScope(scope, task, subtaskID),
		"robot.run", map[string]any{"skill_name": skillName, "skill_version": "0.1.0", "input": input, "request_key": requestKey})
	return waitV050Execution(t, app.Store(), scope.project.ID, requestKey, true)
}

func v050ExecutionScope(scope v050GateScope, task store.Task, subtaskID string) tool.ExecutionScope {
	return tool.ExecutionScope{RunKind: store.RunKindTaskExecution, RunID: "run-" + subtaskID,
		AgentID: "robot-agent", WorkflowID: scope.workflow.Workflow.ID, TaskID: task.ID,
		SubtaskID: subtaskID, RobotID: v050GateRobotID, SessionID: scope.session.ID,
		ProjectID: scope.project.ID, OwnerID: scope.project.OwnerID, WorkspaceRoot: scope.project.WorkspaceRoot,
		ExecutionMode: "full"}
}

func invokeV050RobotAgent(t *testing.T, registry *tool.Registry, scope tool.ExecutionScope,
	toolName string, arguments map[string]any) {
	t.Helper()
	implementation, ok := registry.Get(toolName)
	if !ok {
		t.Fatalf("tool %s is not registered", toolName)
	}
	executor := tool.NewExecutor(registry, tool.ExecutorOptions{DefaultTimeout: 30 * time.Second,
		SerialNamespaces: []string{"robot"}, Logger: log.New(log.Options{Level: log.LevelError, Writer: io.Discard})})
	adapted, err := kernel.AdaptTools([]tool.Definition{implementation.Def()}, executor)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	model := kernel.NewMockChatModel()
	model.SetScript(
		kernel.MockReply{ToolCalls: []schema.ToolCall{{ID: "call-" + kernel.SafeToolName(toolName), Type: "function",
			Function: schema.FunctionCall{Name: kernel.SafeToolName(toolName), Arguments: string(body)}}}},
		kernel.MockReply{Content: "Robot 工具调用已提交。"},
	)
	runner, err := kernel.BuildAgent(context.Background(), kernel.AgentConfig{Name: "robot-gate-agent",
		Role: "robot", Model: model, ModelName: "mock", Tools: adapted, MaxTurns: 4})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := runner.Run(tool.WithExecutionScope(context.Background(), scope), nil, "执行已确认的 Robot Task")
	if err != nil {
		t.Fatal(err)
	}
	var toolResult string
	for {
		event, ok := stream.Next()
		if !ok {
			break
		}
		if event.Kind == kernel.EventToolResult {
			toolResult = event.Text
		}
		if event.Kind == kernel.EventError {
			t.Fatalf("scripted Robot Agent failed: %v", event.Err)
		}
	}
	var result struct {
		OK    bool `json:"ok"`
		Error any  `json:"error"`
	}
	if toolResult == "" || json.Unmarshal([]byte(toolResult), &result) != nil || !result.OK {
		t.Fatalf("%s returned invalid result: %s error=%#v", toolName, toolResult, result.Error)
	}
}

func waitV050Execution(t *testing.T, st *store.Store, projectID, requestKey string, terminal bool) store.RobotExecution {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		execution, err := st.GetRobotExecutionByRequest(projectID, requestKey)
		if err == nil {
			active := execution.Status == "queued" || execution.Status == "starting" || execution.Status == "running" || execution.Status == "waiting_agent" || execution.Status == "stopping"
			if (!terminal && active) || (terminal && !active) {
				if terminal && execution.Status != "completed" && execution.Status != "stopped" {
					events, _ := st.ListRobotExecutionEvents(execution.ID, 0, 500)
					t.Fatalf("Robot Skill ended as %s: error=%#v events=%#v", execution.Status, execution.Error, events)
				}
				return execution
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	execution, _ := st.GetRobotExecutionByRequest(projectID, requestKey)
	events, _ := st.ListRobotExecutionEvents(execution.ID, 0, 500)
	t.Fatalf("Robot Execution did not reach expected state: execution=%#v events=%#v", execution, events)
	return store.RobotExecution{}
}

func assertV050AbilityEvidence(t *testing.T, st *store.Store, execution store.RobotExecution) {
	t.Helper()
	events, err := st.ListRobotExecutionEvents(execution.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	started := 0
	for _, event := range events {
		if event.Type != "action.started" {
			continue
		}
		started++
		instanceID, _ := event.Payload["ability_instance_id"].(string)
		invocationID, _ := event.Payload["invocation_id"].(string)
		if instanceID == "" || invocationID == "" || strings.HasPrefix(instanceID, "fake-") {
			t.Fatalf("Action did not use a real exact Ability instance: %#v", event.Payload)
		}
	}
	if started == 0 {
		t.Fatalf("execution has no Action/Ability evidence: %#v", events)
	}
}

func writeV050ExecutionEvidence(t *testing.T, st *store.Store, path string, execution store.RobotExecution) {
	t.Helper()
	events, err := st.ListRobotExecutionEvents(execution.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	// Gate 证据同时保存 Execution 和真实 Action/Ability 事件，不能只凭最终状态
	// 判断链路已经经过精确 Ability 实例。
	writeV050GateJSON(t, path, map[string]any{"execution": execution, "events": events})
}
func v050GatePose(x, y, z float64) map[string]any {
	return map[string]any{"frame_id": "world", "position_m": []float64{x, y, z},
		"orientation_xyzw": []float64{0, 0, 0, 1}, "observed_at": time.Now().UTC().Format(time.RFC3339Nano),
		"revision": "v050-gate-target"}
}

func writeV050GateJSON(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o640); err != nil {
		t.Fatal(err)
	}
}
