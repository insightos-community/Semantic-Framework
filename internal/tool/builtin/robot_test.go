package builtin

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	robotdomain "insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/robotruntime"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
)

func TestRobotRunRequestKeyComesFromExecutionScope(t *testing.T) {
	base := tool.ExecutionScope{
		WorkflowID: "wf-a", TaskID: "task-a", SubtaskID: "sub-a", RunID: "run-a",
	}
	key := robotRunRequestKey(base)

	retry := base
	retry.RunID = "run-retry"
	if got := robotRunRequestKey(retry); got != key {
		t.Fatalf("同一SubTask的Agent重试必须复用幂等键: first=%q retry=%q", key, got)
	}

	otherWorkflow := base
	otherWorkflow.WorkflowID = "wf-b"
	if got := robotRunRequestKey(otherWorkflow); got == key {
		t.Fatalf("不同Workflow不能因业务目标相同而复用幂等键: %q", got)
	}

	otherSubTask := base
	otherSubTask.SubtaskID = "sub-b"
	if got := robotRunRequestKey(otherSubTask); got == key {
		t.Fatalf("替代SubTask必须获得新的幂等键: %q", got)
	}
}

func TestRobotRunSchemaDoesNotExposeRequestKeyToModel(t *testing.T) {
	definition := (&robotRunTool{}).Def()
	if strings.Contains(definition.ParametersJSON, "request_key") {
		t.Fatalf("request_key属于Framework执行边界，不能继续让模型生成: %s", definition.ParametersJSON)
	}
	for _, required := range []string{"skill_name", "skill_version", "input"} {
		if !strings.Contains(definition.ParametersJSON, required) {
			t.Fatalf("robot.run契约缺少执行参数%q: %s", required, definition.ParametersJSON)
		}
	}
}

func TestDirectRobotRequestKeyUsesRunAndToolCallIdentity(t *testing.T) {
	first := directRobotRunRequestKey("run-1", "call-1")
	if first != directRobotRunRequestKey("run-1", "call-1") ||
		first == directRobotRunRequestKey("run-2", "call-1") ||
		first == directRobotRunRequestKey("run-1", "call-2") {
		t.Fatal("直接调用幂等必须精确绑定Run和tool call")
	}
	if directRobotRunRequestKey("run:a", "b") == directRobotRunRequestKey("run", "a:b") {
		t.Fatal("工具身份分隔符不得产生键碰撞")
	}
}

func TestRobotGetLoadsInstalledContractOnDemand(t *testing.T) {
	_, st := newTestRegistry(t)
	project, err := st.CreateProject("contract-user", "Robot contract")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.CreateChatSession(store.ChatSession{ID: "contract-session", ProjectID: project.ID,
		UserID: project.OwnerID, Title: "Robot contract", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRunSession(store.RunSession{ID: "contract-run", ProjectID: project.ID,
		ChatSessionID: "contract-session", AgentID: "robot:contract-robot", Status: store.RunStatusRunning}); err != nil {
		t.Fatal(err)
	}
	huge := strings.Repeat("SDK_PROFILE_AND_ABILITY_SCHEMA", 3000)
	if err := st.SaveRobotPilot(store.RobotPilot{PilotInstanceID: "contract-pilot",
		RobotID: "contract-robot", RobotStatus: "busy", LastSeenAt: now,
		CurrentExecutionID: "contract-execution", Configuration: map[string]any{"sdk_profile": huge},
		Abilities: []map[string]any{{"task_model": huge}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRobotExecution(store.RobotExecution{ID: "contract-execution", ProjectID: project.ID,
		RobotID: "contract-robot", PilotInstanceID: "contract-pilot", SkillName: "exact-skill", SkillVersion: "1.0.0",
		Status: "running", Stage: "carry_to_target", Input: map[string]any{"large_input": huge},
		Result: map[string]any{"large_result": huge}, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRobotPilotSkill(store.RobotPilotSkill{PilotInstanceID: "contract-pilot",
		Name: "exact-skill", Version: "1.0.0", Enabled: true, Status: "installed"}); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "exact-skill.zip")
	output, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(output)
	entry, err := writer.Create("exact-skill/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(`---
name: exact-skill
description: 精确已安装契约
version: 1.0.0
runtime:
  input_model: scripts.models:ExactInput
  result_model: scripts.models:ExactResult
---
# Exact input

Use target.object_ref from the current request.
`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRobotSkillPackage(store.RobotSkillPackage{Name: "exact-skill", Version: "1.0.0",
		Description: "精确已安装契约", PackagePath: archive, PublishedAt: now}); err != nil {
		t.Fatal(err)
	}
	service := robotdomain.NewService(st, nil)
	contractPilot, err := st.GetRobotPilot("contract-pilot")
	if err != nil {
		t.Fatal(err)
	}
	commands, disconnect, err := service.Connect(contractPilot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(disconnect)
	contractCtx, cancelContracts := context.WithCancel(context.Background())
	t.Cleanup(cancelContracts)
	go func() {
		for {
			select {
			case command, ok := <-commands:
				if !ok {
					return
				}
				if command.Type != "skill.describe_input" {
					continue
				}
				_ = service.HandleCommandAckResult("contract-pilot", command.CommandID, true, "",
					map[string]any{"name": "exact-skill", "version": "1.0.0", "input_schema": map[string]any{"type": "object", "required": []any{"target"}}})
			case <-contractCtx.Done():
				return
			}
		}
	}()
	if err := st.SaveRuntimeInstance(context.Background(), robotruntime.RuntimeInstance{
		InstanceID: "contract-runtime", ProjectID: project.ID, RobotID: "contract-robot",
		SceneInstanceID: "contract-scene", Status: robotruntime.StateReady,
	}); err != nil {
		t.Fatal(err)
	}
	heldObject := "object://tote-1"
	service.SetRobotStateReader("simulation.robot_state", func(context.Context, string, string, string) (robotdomain.StateObservation, error) {
		return robotdomain.StateObservation{RobotID: "contract-robot", ObservedAt: now,
			HoldingObject: &heldObject, InHold: true}, nil
	})
	get := &robotGetTool{service: service}
	ctx := tool.WithExecutionScope(context.Background(), tool.ExecutionScope{
		ProjectID: project.ID, RunID: "contract-run", AgentID: "robot:contract-robot", RobotID: "contract-robot",
		SessionID: "contract-session", WorkspaceRoot: t.TempDir(), RunKind: store.RunKindConversation,
	})
	read := func(raw string) map[string]any {
		t.Helper()
		out, err := get.Run(ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		if raw == `{}` && len(out) >= 8000 {
			t.Fatalf("默认摘要不应随完整 SDK/能力/输入结果膨胀: bytes=%d", len(out))
		}
		var result struct{ Data map[string]any }
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatal(err)
		}
		return result.Data
	}
	summary := read(`{}`)
	if _, exists := summary["skill_contract"]; exists || len(summary["skills"].([]any)) != 1 {
		t.Fatalf("默认查询只应加载已安装摘要: %+v", summary)
	}
	if strings.Contains(stringMustJSON(t, summary), huge) {
		t.Fatal("默认返回不得带完整配置/Ability契约/Execution输入结果")
	}
	execution := summary["current_execution"].(map[string]any)
	work := summary["current_work"].(map[string]any)
	if execution["id"] != "contract-execution" || execution["status"] != "running" ||
		execution["stage"] != "carry_to_target" || work["current_stage"] != "carry_to_target" {
		t.Fatalf("精简不能丢失正在做什么的关键字段: execution=%+v work=%+v", execution, work)
	}
	physical := summary["physical_state"].(map[string]any)
	if physical["status"] != "observed" || physical["source"] != "simulation.robot_state" ||
		physical["holding_object"] != heldObject || physical["in_hold"] != true || physical["observed_at"] == nil {
		t.Fatalf("精简查询须保留真实携物来源与时间: %+v", physical)
	}
	full := read(`{"include_details":true}`)["robot_details"].(map[string]any)
	if full["configuration"].(map[string]any)["sdk_profile"] != huge || full["abilities"] == nil {
		t.Fatal("完整配置和能力必须仍可显式按需查询")
	}
	if pilot, _, err := service.GetRobot("contract-robot"); err != nil || pilot.Configuration["sdk_profile"] != huge {
		t.Fatal("tool 摘要不得改变 Service.GetRobot 外部完整契约")
	}
	service.SetRobotStateReader("simulation.robot_state", func(context.Context, string, string, string) (robotdomain.StateObservation, error) {
		return robotdomain.StateObservation{}, errors.New("read failed")
	})
	physical = read(`{}`)["physical_state"].(map[string]any)
	if physical["status"] != "unavailable" || physical["source"] != "simulation.robot_state" || physical["in_hold"] != nil {
		t.Fatalf("读取失败不能被解释为空工具: %+v", physical)
	}
	result := read(`{"skill_name":"exact-skill"}`)
	contract := result["skill_contract"].(map[string]any)
	if contract["name"] != "exact-skill" || contract["version"] != "1.0.0" ||
		contract["input_model"] != "scripts.models:ExactInput" || contract["result_model"] != "scripts.models:ExactResult" ||
		!strings.Contains(contract["documentation"].(string), "target.object_ref") {
		t.Fatalf("直接对话必须拿到真实已装契约: %+v", contract)
	}
	if _, exists := contract["package_path"]; exists {
		t.Fatal("模型契约不应包含 Server 发布包文件路径")
	}
	if inputSchema, ok := contract["input_schema"].(map[string]any); !ok || inputSchema["type"] != "object" {
		t.Fatal("必须返回已安装 Worker 的真实 Schema，不得由模型标识或示例拼接")
	}
	selected := read(`{"skill_name":"exact-skill","include_details":true}`)
	if selected["robot_details"] != nil || contract["extensions"] != nil || contract["resources"] != nil {
		t.Fatal("选定 Skill 不得携带整个 Robot/Ability 目录或重复扩展文档")
	}
	taskCtx := tool.WithExecutionScope(context.Background(), tool.ExecutionScope{
		ProjectID: project.ID, TaskID: "current-task", RobotID: "contract-robot", SessionID: "contract-session",
		WorkspaceRoot: t.TempDir(),
	})
	taskOut, taskErr := get.Run(taskCtx, `{"include_details":true}`)
	if taskErr != nil || strings.Contains(taskOut, `"robot_details"`) || strings.Contains(taskOut, huge) {
		t.Fatalf("Task 查询不能扩展到全量配置: %v", taskErr)
	}
	for _, test := range []struct{ raw, code string }{
		{`{"skill_version":"1.0.0"}`, "BAD_ARGUMENTS"},
		{`{"skill_name":"missing"}`, "ROBOT_SKILL_UNAVAILABLE"},
		{`{"skill_name":"exact-skill","skill_version":"9.0.0"}`, "ROBOT_SKILL_UNAVAILABLE"},
	} {
		_, err := get.Run(ctx, test.raw)
		var toolErr *tool.Error
		if !errors.As(err, &toolErr) || toolErr.Code != test.code {
			t.Fatalf("%s: want %s, got %v", test.raw, test.code, err)
		}
	}
	if err := st.SaveRobotPilotSkill(store.RobotPilotSkill{PilotInstanceID: "contract-pilot",
		Name: "exact-skill", Version: "2.0.0", Enabled: true, Status: "installed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := get.Run(ctx, `{"skill_name":"exact-skill"}`); err == nil {
		t.Fatal("多版本目录必须要求显式版本")
	} else {
		var toolErr *tool.Error
		if !errors.As(err, &toolErr) || toolErr.Code != "ROBOT_SKILL_VERSION_REQUIRED" {
			t.Fatalf("版本歧义应返回明确工具错误: %v", err)
		}
	}
	read(`{"skill_name":"exact-skill","skill_version":"1.0.0"}`)
	if run, err := st.GetRunSession("contract-run"); err != nil || run.RobotID != "" {
		t.Fatalf("读取契约不得申请动作占用: %+v %v", run, err)
	}
	definition := get.Def()
	for _, field := range []string{"skill_name", "skill_version"} {
		if !strings.Contains(definition.ParametersJSON, field) {
			t.Fatalf("robot.get Schema 未暴露 %s", field)
		}
	}
}

func stringMustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestRobotWorkSummariesBoundLargeTaskAndRunFields(t *testing.T) {
	huge := strings.Repeat("任务输入与执行证据", 10000)
	task := robotTaskSummary(store.Task{ID: "task-summary", WorkflowID: "workflow-summary",
		Goal: huge, WaitingReason: huge, Input: json.RawMessage(`{"large":true}`),
		ResultSummary: huge, Status: store.TaskStatusRunning, AssignedRobotID: "robot-summary"})
	run := robotRunSummary(store.RunSession{ID: "run-summary", Status: store.RunStatusRunning, Error: huge,
		AgentID: "robot:robot-summary", RobotID: "robot-summary", ChatSessionID: "conversation-summary"})
	if size := len(stringMustJSON(t, map[string]any{"current_task": task, "current_run": run})); size >= 4000 {
		t.Fatalf("Task/Run摘要不能无限附带正文: bytes=%d", size)
	}
	if task["id"] != "task-summary" || task["status"] != store.TaskStatusRunning ||
		run["id"] != "run-summary" || run["conversation_id"] != "conversation-summary" {
		t.Fatal("摘要不得丢失当前工作身份")
	}
}
