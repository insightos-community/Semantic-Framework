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
	"fmt"
	"strings"

	"insightos.cn/semantic-framework/internal/interaction"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
)

type planSuggestTool struct{ creator PlanProposalSubmitter }

type planSuggestArgs struct {
	Goal               string                 `json:"goal"`
	Summary            string                 `json:"summary"`
	ApprovedScope      json.RawMessage        `json:"approved_scope"`
	Constraints        json.RawMessage        `json:"constraints"`
	CompletionCriteria json.RawMessage        `json:"completion_criteria"`
	MapBinding         json.RawMessage        `json:"map_binding"`
	Tasks              []store.TaskDraft      `json:"tasks"`
	Dependencies       []store.TaskDependency `json:"dependencies"`
}

func (t *planSuggestTool) Def() tool.Definition {
	return tool.Definition{Name: namePlanSuggest, Namespace: "plan",
		Description: "在显式 Plan Mode 下提交完整方案，创建或修订可审阅 Plan Proposal。用户批准精确 revision 后才创建 Workflow。constraints 和 completion_criteria 只能传数组或对象；map_binding 只有在已有精确地图选择时才传对象，未知时必须省略。该工具不确认或执行计划。",
		ParametersJSON: `{"type":"object","properties":{
			"goal":{"type":"string","minLength":1},
			"summary":{"type":"string","minLength":1},
			"approved_scope":{"type":"object","properties":{"robot_ids":{"type":"array","items":{"type":"string"}},"robot_models":{"type":"array","items":{"type":"string"}},"allowed_skills":{"type":"array","description":"用户批准的精确 Robot Skill 名称范围；若 Robot Task 填写 required_capabilities，必须同时在这里声明同名 Skill。","items":{"type":"string"}},"objects":{"type":"array","items":{"type":"string"}},"regions":{"type":"array","items":{"type":"string"}},"physical_operations":{"type":"array","items":{"type":"string"}},"safety_constraints":{"type":"array","items":{"type":"string"}}},"additionalProperties":false},
			"constraints":{"oneOf":[{"type":"array"},{"type":"object"}]},
			"completion_criteria":{"oneOf":[{"type":"array"},{"type":"object"}]},
			"tasks":{"type":"array","minItems":1,"items":{"type":"object","required":["required_role","goal"],"properties":{"id":{"type":"string"},"required_role":{"type":"string","enum":["robot","developer","map","monitor"]},"required_capabilities":{"type":"array","description":"机器可匹配的精确能力标识。Robot Task只能填写approved_scope.allowed_skills中的Robot Skill名称；若不锁定具体Skill则省略，不能填写业务步骤说明。","items":{"type":"string"}},"resource_requirements":{"type":"object","properties":{"robot_ids":{"type":"array","items":{"type":"string"}},"robot_models":{"type":"array","items":{"type":"string"}},"backends":{"type":"array","items":{"type":"string"}},"workspace_write":{"type":"boolean"}},"additionalProperties":false},"goal":{"type":"string","minLength":1},"input":{"type":"object"},"completion_criteria":{"oneOf":[{"type":"array"},{"type":"object"}]}},"additionalProperties":false}},
			"dependencies":{"type":"array","items":{"type":"object","required":["task_id","depends_on_task_id"],"properties":{"task_id":{"type":"string","minLength":1},"depends_on_task_id":{"type":"string","minLength":1}},"additionalProperties":false}},
			"map_binding":{"type":"object","required":["map_id","generation","selections"],"properties":{
				"map_id":{"type":"string","enum":["simulation_map","real_map"]},
				"generation":{"type":"integer","minimum":1},
				"selections":{"type":"array","minItems":1,"items":{"oneOf":[
					{"type":"object","required":["kind","entity_id"],"properties":{"kind":{"enum":["entity","region"]},"entity_id":{"type":"string","minLength":1}},"additionalProperties":false},
					{"type":"object","required":["kind","frame_id","position"],"properties":{"kind":{"const":"point"},"frame_id":{"type":"string","minLength":1},"position":{"type":"array","minItems":3,"maxItems":3,"items":{"type":"number"}}},"additionalProperties":false}
				]}}
			},"additionalProperties":false}
		},"required":["goal","summary","tasks"],"additionalProperties":false}`,
		Annotations: tool.Annotations{Risk: tool.RiskLow, Idempotent: false}}
}

func (t *planSuggestTool) Run(ctx context.Context, raw string) (string, error) {
	if t.creator == nil {
		return "", &tool.Error{Code: "PLAN_SUGGEST_UNAVAILABLE", Message: "Plan suggestion 服务未装配"}
	}
	var args planSuggestArgs
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return "", &tool.Error{Code: "BAD_ARGUMENTS", Message: "参数不是合法 JSON: " + err.Error()}
	}
	args.Goal = strings.TrimSpace(args.Goal)
	if args.Goal == "" {
		return "", &tool.Error{Code: "BAD_ARGUMENTS", Message: "goal 不能为空"}
	}
	if err := validatePlanSuggestionArgs(args); err != nil {
		return "", &tool.Error{Code: "BAD_ARGUMENTS", Message: err.Error()}
	}
	scope, ok := tool.ExecutionScopeFromContext(ctx)
	if !ok || scope.RunKind != store.RunKindConversation || scope.InteractionMode != "plan" ||
		scope.RunID == "" || scope.AgentID == "" {
		return "", &tool.Error{Code: "PLAN_SUGGEST_SCOPE_INVALID", Message: "plan.suggest 只能由显式 Plan Mode 的 Leader Run 调用"}
	}
	if len(args.Tasks) == 0 {
		return "", &tool.Error{Code: "BAD_ARGUMENTS",
			Message: "显式 Plan Mode 必须提交 tasks；每个 Task 只描述业务目标、角色、能力和资源范围"}
	}
	request := interaction.PlanSuggestionRequest{
		UserID: scope.OwnerID, ProjectID: scope.ProjectID, SessionID: scope.SessionID,
		RunID: scope.RunID, SourceAgentID: scope.AgentID, Goal: args.Goal, Summary: strings.TrimSpace(args.Summary),
		ApprovedScope: args.ApprovedScope,
		Constraints:   args.Constraints, CompletionCriteria: args.CompletionCriteria,
		MapBinding: args.MapBinding, Tasks: args.Tasks, Dependencies: args.Dependencies,
	}
	proposal, err := t.creator.SubmitPlanProposal(ctx, request)
	if err != nil {
		return "", &tool.Error{Code: "PLAN_SUGGEST_FAILED", Message: err.Error()}
	}
	return tool.OKResult(map[string]any{"plan_proposal_id": proposal.ID,
		"revision": proposal.Revision, "status": proposal.Status, "action": "review_plan"})
}

func validatePlanSuggestionArgs(args planSuggestArgs) error {
	if strings.TrimSpace(args.Summary) == "" {
		return fmt.Errorf("summary 不能为空")
	}
	allowedSkills, err := planAllowedSkills(args.ApprovedScope)
	if err != nil {
		return err
	}
	for index, task := range args.Tasks {
		if err := validatePlanTask(task, allowedSkills); err != nil {
			return fmt.Errorf("tasks[%d]: %w", index, err)
		}
	}
	for _, field := range []struct {
		name string
		raw  json.RawMessage
	}{
		{name: "constraints", raw: args.Constraints},
		{name: "completion_criteria", raw: args.CompletionCriteria},
	} {
		if emptyJSON(field.raw) {
			continue
		}
		trimmed := bytesTrimSpace(field.raw)
		if len(trimmed) == 0 || (trimmed[0] != '[' && trimmed[0] != '{') {
			return fmt.Errorf("%s 必须是数组或对象；未知时请省略，不能传 true/false 占位", field.name)
		}
	}
	if emptyJSON(args.MapBinding) || string(bytesTrimSpace(args.MapBinding)) == "{}" {
		return nil
	}
	trimmed := bytesTrimSpace(args.MapBinding)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("map_binding 必须是包含 map_id、generation 和 selections 的对象；没有精确地图选择时请省略，不能传 true/false 占位")
	}
	var binding struct {
		MapID      string `json:"map_id"`
		Generation int64  `json:"generation"`
		Selections []struct {
			Kind     string    `json:"kind"`
			EntityID string    `json:"entity_id"`
			FrameID  string    `json:"frame_id"`
			Position []float64 `json:"position"`
		} `json:"selections"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(trimmed)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&binding); err != nil ||
		(binding.MapID != "simulation_map" && binding.MapID != "real_map") ||
		binding.Generation <= 0 || len(binding.Selections) == 0 {
		return fmt.Errorf("map_binding 必须是有效的地图选择对象；没有精确地图选择时请省略")
	}
	for _, selection := range binding.Selections {
		switch selection.Kind {
		case "entity", "region":
			if strings.TrimSpace(selection.EntityID) == "" {
				return fmt.Errorf("map_binding 的 entity/region 选择必须包含 entity_id")
			}
		case "point":
			if strings.TrimSpace(selection.FrameID) == "" || len(selection.Position) != 3 {
				return fmt.Errorf("map_binding 的 point 选择必须包含 frame_id 和三个 position 数值")
			}
		default:
			return fmt.Errorf("map_binding 的 selection.kind 只能是 entity、region 或 point")
		}
	}
	return nil
}

func validatePlanTask(task store.TaskDraft, allowedSkills map[string]struct{}) error {
	switch task.RequiredRole {
	case "robot", "developer", "map", "monitor":
	default:
		return fmt.Errorf("required_role 必须是 robot、developer、map 或 monitor；不要填写 Agent 显示名称")
	}
	var capabilities []string
	if !emptyJSON(task.RequiredCapabilities) {
		if err := json.Unmarshal(task.RequiredCapabilities, &capabilities); err != nil {
			return fmt.Errorf("required_capabilities 必须是字符串数组: %w", err)
		}
	}
	if task.RequiredRole == "robot" && len(capabilities) > 0 {
		if len(allowedSkills) == 0 {
			// 调度器只按精确 Skill/Ability 标识匹配资源，不能把“抓取并整理姿态”等
			// 业务步骤猜成某个 Robot Skill。没有批准具体 Skill 范围时应保持后绑定，
			// 直接省略 required_capabilities，由 Robot Agent 根据实际目录规划 SubTask。
			return fmt.Errorf("Robot Task 填写 required_capabilities 时必须同时声明 approved_scope.allowed_skills；若保持 Skill 后绑定请省略 required_capabilities，不能填写业务步骤")
		}
		for _, capability := range capabilities {
			if _, ok := allowedSkills[capability]; !ok {
				return fmt.Errorf("required_capabilities 中的 %q 不在 approved_scope.allowed_skills 内；不要发明未批准的能力", capability)
			}
		}
	}
	if emptyJSON(task.ResourceRequirements) || string(bytesTrimSpace(task.ResourceRequirements)) == "{}" {
		return nil
	}
	var requirements struct {
		RobotIDs       []string `json:"robot_ids"`
		RobotModels    []string `json:"robot_models"`
		Backends       []string `json:"backends"`
		WorkspaceWrite bool     `json:"workspace_write"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(task.ResourceRequirements)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&requirements); err != nil {
		// Task 分配器只理解这四个资源条件。这里拒绝诸如 robot/object/target_column
		// 等业务字段，是为了让模型在当前 Run 内修正，而不是创建一个永远无法分配的 Workflow。
		return fmt.Errorf("resource_requirements 只允许 robot_ids、robot_models、backends、workspace_write: %w", err)
	}
	for _, backend := range requirements.Backends {
		if strings.EqualFold(strings.TrimSpace(backend), "simulation") {
			return fmt.Errorf("backends 只能填写Robot实际后端标识（例如mujoco或fake）；simulation是环境类别，不是可匹配backend，不需要限制时请省略backends")
		}
	}
	return nil
}

func emptyJSON(raw json.RawMessage) bool {
	trimmed := string(bytesTrimSpace(raw))
	return trimmed == "" || trimmed == "null"
}

func bytesTrimSpace(raw []byte) []byte {
	return []byte(strings.TrimSpace(string(raw)))
}

func planAllowedSkills(raw json.RawMessage) (map[string]struct{}, error) {
	allowed := make(map[string]struct{})
	if emptyJSON(raw) || string(bytesTrimSpace(raw)) == "{}" {
		return allowed, nil
	}
	var scope struct {
		AllowedSkills []string `json:"allowed_skills"`
	}
	if err := json.Unmarshal(raw, &scope); err != nil {
		return nil, fmt.Errorf("approved_scope 必须是对象: %w", err)
	}
	for _, skill := range scope.AllowedSkills {
		skill = strings.TrimSpace(skill)
		if skill != "" {
			allowed[skill] = struct{}{}
		}
	}
	return allowed, nil
}
