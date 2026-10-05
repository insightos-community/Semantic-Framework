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

package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/compose"

	robotdomain "insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
)

func RegisterRobotTools(reg *tool.Registry, service *robotdomain.Service) error {
	for _, item := range []tool.Tool{&robotGetTool{service: service}, &robotRunTool{service: service}, &robotStopTool{service: service}} {
		if err := reg.Register(item); err != nil {
			return err
		}
	}
	return nil
}

type robotGetTool struct{ service *robotdomain.Service }

func (t *robotGetTool) Def() tool.Definition {
	return tool.Definition{Name: "robot.get", Namespace: "robot", Description: "默认返回精简 Robot 在线事实、已装 Skill 摘要、当前 Task/Run/Execution/步骤与只读 physical_state。physical_state 为 unavailable/stale 时不能据此判断当前携物；holding_object=null 不能证明双工具空载。include_details仅在未指定skill_name的非Task诊断中返回完整Robot配置；指定skill_name只返回该Skill契约，不附带全量Ability。执行前缺少准确契约时按需传 skill_name 获取 skill_contract，不得猜字段；skill_version 省略仅能解析当前 Robot 唯一已安装启用版本，不使用 Registry latest。按 execution_id 可查询本 Project 此 Robot 的完整执行结果、图像引用与分页事件。", ParametersJSON: `{"type":"object","properties":{"include_details":{"type":"boolean","default":false},"skill_name":{"type":"string","minLength":1},"skill_version":{"type":"string","minLength":1},"execution_id":{"type":"string"},"after_sequence":{"type":"integer","minimum":0}},"additionalProperties":false}`, Annotations: tool.Annotations{Risk: tool.RiskLow, Idempotent: true}}
}
func (t *robotGetTool) Run(ctx context.Context, raw string) (string, error) {
	var args struct {
		IncludeDetails bool   `json:"include_details"`
		SkillName      string `json:"skill_name"`
		SkillVersion   string `json:"skill_version"`
		ExecutionID    string `json:"execution_id"`
		AfterSequence  int64  `json:"after_sequence"`
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil || args.AfterSequence < 0 {
			return "", &tool.Error{Code: "BAD_ARGUMENTS", Message: "robot.get 参数无效"}
		}
	}
	args.SkillName, args.SkillVersion = strings.TrimSpace(args.SkillName), strings.TrimSpace(args.SkillVersion)
	if args.SkillVersion != "" && args.SkillName == "" {
		return "", &tool.Error{Code: "BAD_ARGUMENTS", Message: "skill_version 必须与 skill_name 一起提供"}
	}
	scope, ok := tool.ExecutionScopeFromContext(ctx)
	if !ok || scope.RobotID == "" {
		return "", &tool.Error{Code: "ROBOT_BINDING_MISSING", Message: "当前 Agent 或 Task 没有绑定 Robot"}
	}
	if scope.TaskID == "" {
		if err := t.service.ValidateDirectRunScope(scope.ProjectID, scope.RunID, scope.AgentID, scope.RobotID); err != nil {
			return "", robotToolError(err)
		}
	}
	pilot, skills, err := t.service.GetRobot(scope.RobotID)
	if err != nil {
		return "", robotToolError(err)
	}
	device, err := t.service.Device(scope.RobotID)
	if err != nil {
		return "", robotToolError(err)
	}
	skillSummaries := make([]map[string]any, 0, len(skills))
	for _, installed := range skills {
		skillSummaries = append(skillSummaries, map[string]any{"name": installed.Name,
			"version": installed.Version, "enabled": installed.Enabled, "status": installed.Status})
	}
	// Device/GetRobot retain their full UI/domain contract. Only this model-facing
	// projection is small: do not duplicate the SDK Profile and Ability schemas.
	result := map[string]any{
		"robot": robotFields(device, "robot_id", "display_name", "model", "backend", "status", "revision",
			"pilot", "ability_framework", "skill_catalog_revision", "ability_catalog_revision"),
		"skills":         skillSummaries,
		"current_work":   robotFields(device, "status", "current_execution_id", "current_stage", "progress"),
		"physical_state": t.service.ReadRobotState(ctx, scope.ProjectID, scope.RobotID),
	}
	// A selected Skill scopes details to that contract. Whole-device diagnostics
	// remain available without a Skill selection, outside task execution.
	if args.IncludeDetails && args.SkillName == "" && scope.TaskID == "" {
		result["robot_details"] = pilot
	}
	if args.SkillName != "" {
		detail, err := t.service.GetInstalledSkillPackageDetail(scope.RobotID, args.SkillName, args.SkillVersion)
		if err != nil {
			return "", robotToolError(err)
		}
		// Match TaskExecution's semantic contract, not the Server filesystem
		// package path. Keep the parsed extensions and resources available for
		// complete documentation without synthesizing an input JSON Schema.
		contract := map[string]any{
			"name": detail.Name, "version": detail.Version, "description": detail.Description,
			"when_to_use": detail.WhenToUse, "documentation": detail.Body,
			"required_actions": detail.RequiredActions, "stop_actions": detail.StopActions,
		}
		if runtimeSpec, ok := detail.Extensions["runtime"].(map[string]any); ok {
			contract["input_model"], _ = runtimeSpec["input_model"].(string)
			contract["result_model"], _ = runtimeSpec["result_model"].(string)
		}
		inputSchema, err := t.service.DescribeSkillInput(ctx, scope.RobotID, detail.Name, detail.Version)
		if err != nil {
			return "", robotToolError(err)
		}
		contract["input_schema"] = inputSchema
		result["skill_contract"] = contract
	}
	if run, err := t.service.Store().GetRobotConversationRun(scope.RobotID); err == nil && run.ProjectID == scope.ProjectID {
		result["current_run"] = robotRunSummary(run)
	}
	if task, err := t.service.Store().GetRobotTask(scope.RobotID); err == nil {
		if workflow, err := t.service.Store().GetWorkflow(task.WorkflowID); err == nil && workflow.ProjectID == scope.ProjectID {
			result["current_task"] = robotTaskSummary(task)
		}
	}
	if pilot.CurrentExecutionID != "" {
		if execution, err := t.service.Store().GetRobotExecution(pilot.CurrentExecutionID); err == nil && execution.ProjectID == scope.ProjectID {
			result["current_execution"] = robotExecutionSummary(execution)
			if execution.SubtaskID != "" {
				if subtask, err := t.service.Store().GetSubTask(execution.SubtaskID); err == nil && subtask.TaskID == execution.TaskID {
					result["current_subtask"] = map[string]any{"id": subtask.ID, "task_id": subtask.TaskID,
						"position": subtask.Position, "kind": subtask.Kind, "status": subtask.Status,
						"goal": robotSummaryText(subtask.Goal, 512), "updated_at": subtask.UpdatedAt}
				}
			}
		}
	}
	if args.ExecutionID != "" {
		execution, err := t.service.Store().GetRobotExecution(args.ExecutionID)
		if err != nil || execution.ProjectID != scope.ProjectID || execution.RobotID != scope.RobotID {
			return "", &tool.Error{Code: "ROBOT_EXECUTION_OUTSIDE_SCOPE", Message: "Execution 不属于当前 Project 与 Robot"}
		}
		events, err := t.service.Store().ListRobotExecutionEvents(execution.ID, args.AfterSequence, 101)
		if err != nil {
			return "", robotToolError(err)
		}
		result["has_more"] = len(events) > 100
		if len(events) > 100 {
			events = events[:100]
		}
		if len(events) > 0 {
			result["next_sequence"] = events[len(events)-1].Sequence
		}
		result["execution"], result["events"] = execution, events
	}
	return tool.OKResult(result)
}

func robotFields(source map[string]any, names ...string) map[string]any {
	result := make(map[string]any, len(names))
	for _, name := range names {
		if value, exists := source[name]; exists {
			result[name] = value
		}
	}
	return result
}

func robotRunSummary(run store.RunSession) map[string]any {
	return map[string]any{"id": run.ID, "conversation_id": run.ChatSessionID,
		"agent_id": run.AgentID, "robot_id": run.RobotID, "status": run.Status,
		"started_at": run.StartedAt, "updated_at": run.UpdatedAt}
}

func robotTaskSummary(task store.Task) map[string]any {
	return map[string]any{"id": task.ID, "workflow_id": task.WorkflowID, "status": task.Status,
		"goal": robotSummaryText(task.Goal, 512), "reason": robotSummaryText(task.WaitingReason, 256),
		"assigned_agent_id": task.AssignedAgentID, "assigned_robot_id": task.AssignedRobotID,
		"updated_at": task.UpdatedAt}
}

func robotExecutionSummary(execution store.RobotExecution) map[string]any {
	return map[string]any{"id": execution.ID, "workflow_id": execution.WorkflowID,
		"task_id": execution.TaskID, "subtask_id": execution.SubtaskID, "run_id": execution.RunID,
		"robot_id": execution.RobotID, "skill_name": execution.SkillName, "skill_version": execution.SkillVersion,
		"status": execution.Status, "stage": execution.Stage, "progress": execution.Progress,
		"has_result": len(execution.Result) > 0, "has_error": len(execution.Error) > 0,
		"artifact_count": len(execution.ArtifactRefs), "updated_at": execution.UpdatedAt}
}

func robotSummaryText(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return value
}

type robotRunArgs struct {
	SkillName    string         `json:"skill_name"`
	SkillVersion string         `json:"skill_version"`
	Input        map[string]any `json:"input"`
}
type robotRunTool struct{ service *robotdomain.Service }

func (t *robotRunTool) Def() tool.Definition {
	return tool.Definition{Name: "robot.run", Namespace: "robot", Description: "在当前 Robot 上启动一个已安装 Skill；使用准确 skill_version 与契约字段，缺少契约时先 robot.get(skill_name)。受理后立即返回 Execution ID 并结束本轮请求，实际进度和结果由执行面板展示；accepted 不代表任务成功。不得轮询或再次调用以重放参数。", ParametersJSON: `{"type":"object","properties":{"skill_name":{"type":"string"},"skill_version":{"type":"string"},"input":{"type":"object"}},"required":["skill_name","skill_version","input"],"additionalProperties":false}`, Annotations: tool.Annotations{Risk: tool.RiskHigh, Timeout: 15 * time.Minute}}
}
func (t *robotRunTool) Run(ctx context.Context, raw string) (string, error) {
	var args robotRunArgs
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return "", &tool.Error{Code: "BAD_ARGUMENTS", Message: err.Error()}
	}
	scope, ok := tool.ExecutionScopeFromContext(ctx)
	if !ok || scope.RobotID == "" {
		return "", &tool.Error{Code: "ROBOT_BINDING_MISSING", Message: "robot.run 需要当前 Robot Agent 或已绑定 Robot 的 Task"}
	}
	direct := scope.RunKind == store.RunKindConversation && scope.WorkflowID == "" && scope.TaskID == "" && scope.SubtaskID == ""
	request := robotdomain.RunRequest{ProjectID: scope.ProjectID, WorkflowID: scope.WorkflowID,
		TaskID: scope.TaskID, SubtaskID: scope.SubtaskID, RunID: scope.RunID, AgentID: scope.AgentID,
		RobotID: scope.RobotID, SkillName: args.SkillName, SkillVersion: args.SkillVersion,
		Input: args.Input, RequestKey: robotRunRequestKey(scope), ArtifactRefs: collectArtifactRefs(args.Input)}
	if direct {
		if scope.InteractionMode == "plan" {
			return "", &tool.Error{Code: "ROBOT_SKILL_OUTSIDE_APPROVED_SCOPE", Message: "规划模式不能执行 Robot 动作"}
		}
		callID := compose.GetToolCallID(ctx)
		if callID == "" {
			return "", &tool.Error{Code: "ROBOT_REQUEST_IDENTITY_MISSING", Message: "直接 Robot 请求缺少工具调用身份"}
		}
		request.RequestKey = directRobotRunRequestKey(scope.RunID, callID)
	} else {
		if err := t.service.ValidateTaskSkillScope(request); err != nil {
			return "", robotToolError(err)
		}
	}
	execution, err := t.service.Run(ctx, request)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(err, robotdomain.ErrRobotBusy) {
			// 拒绝的是新请求，原执行继续持有机器人。返回原执行身份，避免
			// Agent 把冲突理解为旧任务已失败，再次调用 robot.run 或停止错对象。
			if pilot, loadErr := t.service.Store().GetActiveRobotPilot(scope.RobotID); loadErr == nil && pilot.CurrentExecutionID != "" {
				return "", &tool.Error{Code: "ROBOT_BUSY", Message: fmt.Sprintf(
					"Robot 正由执行 %s 占用，本次请求未启动。请查看原执行；需要更换任务时，先停止该执行并等待停止确认，不要自动重试 robot.run。", pilot.CurrentExecutionID)}
			}
		}
		return "", robotToolError(err)
	}
	return tool.OKResult(map[string]any{"accepted": true, "execution_id": execution.ID, "status": execution.Status})
}

func directRobotRunRequestKey(runID, callID string) string {
	// JSON tuple avoids ambiguous delimiter combinations in provider call IDs.
	identity, _ := json.Marshal([]string{runID, callID})
	return "conversation:" + string(identity)
}

func robotRunRequestKey(scope tool.ExecutionScope) string {
	// request_key 是物理执行的幂等边界，不是业务参数。它必须来自已经由
	// Framework 校验的 Workflow/Task/SubTask 身份；交给模型生成会让不同
	// Workflow 中相同的“箱体→槽位”描述复用旧键，导致新动作在执行前被拒绝。
	return "workflow:" + scope.WorkflowID + ":task:" + scope.TaskID + ":subtask:" + scope.SubtaskID
}

type robotStopArgs struct {
	ExecutionID string `json:"execution_id"`
	Reason      string `json:"reason"`
}
type robotStopTool struct{ service *robotdomain.Service }

func (t *robotStopTool) Def() tool.Definition {
	return tool.Definition{Name: "robot.stop", Namespace: "robot", Description: "请求当前 Project 中明确 Robot Execution 安全停止。", ParametersJSON: `{"type":"object","properties":{"execution_id":{"type":"string"},"reason":{"type":"string"}},"required":["execution_id","reason"],"additionalProperties":false}`, Annotations: tool.Annotations{Risk: tool.RiskHigh, Idempotent: true}}
}
func (t *robotStopTool) Run(ctx context.Context, raw string) (string, error) {
	var args robotStopArgs
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return "", &tool.Error{Code: "BAD_ARGUMENTS", Message: err.Error()}
	}
	scope, ok := tool.ExecutionScopeFromContext(ctx)
	if !ok {
		return "", &tool.Error{Code: "EXECUTION_SCOPE_MISSING", Message: "robot.stop 只能在 Project Run 中使用"}
	}
	if scope.RobotID != "" {
		execution, err := t.service.Store().GetRobotExecution(args.ExecutionID)
		if err != nil || execution.ProjectID != scope.ProjectID || execution.RobotID != scope.RobotID {
			return "", &tool.Error{Code: "ROBOT_EXECUTION_OUTSIDE_SCOPE", Message: "Execution 不属于当前 Project 与 Robot"}
		}
	}
	execution, err := t.service.Stop(ctx, scope.ProjectID, args.ExecutionID, args.Reason)
	if err != nil {
		if execution.ID == "" {
			return "", robotToolError(err)
		}
		return tool.OKResult(map[string]any{"accepted": false, "execution_id": execution.ID,
			"status": execution.Status, "error": err.Error()})
	}
	return tool.OKResult(map[string]any{"accepted": true, "execution_id": execution.ID, "status": execution.Status})
}

func robotToolError(err error) *tool.Error {
	code := "ROBOT_OPERATION_FAILED"
	switch {
	case errors.Is(err, robotdomain.ErrPilotOffline):
		code = "PILOT_OFFLINE"
	case errors.Is(err, robotdomain.ErrRobotBusy):
		code = "ROBOT_BUSY"
	case errors.Is(err, robotdomain.ErrSkillUnavailable):
		code = "ROBOT_SKILL_UNAVAILABLE"
	case errors.Is(err, robotdomain.ErrSkillVersionRequired):
		code = "ROBOT_SKILL_VERSION_REQUIRED"
	case errors.Is(err, robotdomain.ErrRequestConflict):
		code = "ROBOT_REQUEST_CONFLICT"
	case errors.Is(err, robotdomain.ErrSkillInputInvalid):
		// A model may correct these arguments. This must not be confused with
		// an Action failure, nor marked retryable for blind execution replay.
		code = "ROBOT_SKILL_INPUT_INVALID"
	case errors.Is(err, robotdomain.ErrSkillOutsideTask):
		code = "ROBOT_SKILL_OUTSIDE_APPROVED_SCOPE"
	case errors.Is(err, robotdomain.ErrDirectRunScope):
		code = "ROBOT_RUN_SCOPE_INVALID"
	}
	return &tool.Error{Code: code, Message: err.Error()}
}

func collectArtifactRefs(value any) []string {
	seen := map[string]struct{}{}
	var walk func(any)
	walk = func(current any) {
		switch item := current.(type) {
		case string:
			if strings.HasPrefix(item, "artifact://") {
				seen[item] = struct{}{}
			}
		case []any:
			for _, child := range item {
				walk(child)
			}
		case map[string]any:
			for _, child := range item {
				walk(child)
			}
		}
	}
	walk(value)
	result := make([]string, 0, len(seen))
	for ref := range seen {
		result = append(result, ref)
	}
	return result
}
