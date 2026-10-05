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
	"strings"

	"insightos.cn/semantic-framework/internal/interaction"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
)

type interactionAskTool struct{ creator AgentQuestionCreator }

type interactionAskArgs struct {
	Prompt       string                           `json:"prompt"`
	Presentation string                           `json:"presentation"`
	Fields       []interaction.AgentQuestionField `json:"fields"`
	AllowOther   *bool                            `json:"allow_other,omitempty"`
}

func (t *interactionAskTool) Def() tool.Definition {
	return tool.Definition{
		Name: nameInteractionAsk, Namespace: "interaction",
		Description: "仅在缺少会改变业务结果、授权或安全选择的用户决定时提出结构化问题，并指出具体缺失项。已批准的任务步骤、资源等待、可由当前上下文或只读查询确定的事实不需要再次确认。该工具会结束当前 Run；用户回答后系统为同一 Agent 创建新 Run 并注入原上下文。",
		ParametersJSON: `{"type":"object","required":["prompt","presentation"],"properties":{
			"prompt":{"type":"string","minLength":1},
			"presentation":{"type":"string","enum":["confirm","form","single_select","multi_select","parameter","image_select","map_select","file_select"]},
			"fields":{"type":"array","items":{"type":"object","required":["name","label","value_type"],"properties":{
				"name":{"type":"string","minLength":1},"label":{"type":"string","minLength":1},
				"description":{"type":"string"},"value_type":{"type":"string","enum":["string","number","integer","boolean"]},
				"required":{"type":"boolean"},"options":{"type":"array","items":{"type":"object","required":["value","label"],"properties":{"value":{"type":"string","minLength":1},"label":{"type":"string","minLength":1},"description":{"type":"string"}},"additionalProperties":false}}
			},"additionalProperties":false}},
			"allow_other":{"type":"boolean"}
		},"additionalProperties":false}`,
		Annotations: tool.Annotations{Risk: tool.RiskLow, Idempotent: false},
	}
}

func (t *interactionAskTool) Run(ctx context.Context, raw string) (string, error) {
	if t.creator == nil {
		return "", &tool.Error{Code: "INTERACTION_UNAVAILABLE", Message: "Interaction 服务未装配"}
	}
	var args interactionAskArgs
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return "", &tool.Error{Code: "BAD_ARGUMENTS", Message: "参数不是合法 JSON: " + err.Error()}
	}
	args.Prompt = strings.TrimSpace(args.Prompt)
	if args.Prompt == "" || strings.TrimSpace(args.Presentation) == "" {
		return "", &tool.Error{Code: "BAD_ARGUMENTS", Message: "prompt 和 presentation 不能为空"}
	}
	scope, ok := tool.ExecutionScopeFromContext(ctx)
	if !ok || scope.RunID == "" || scope.AgentID == "" ||
		(scope.RunKind != store.RunKindConversation && scope.RunKind != store.RunKindTaskPlanning &&
			scope.RunKind != store.RunKindTaskExecution) {
		return "", &tool.Error{Code: "INTERACTION_SCOPE_INVALID",
			Message: "interaction.ask 只能由 Conversation、Task Planning 或 Task Execution Run 调用"}
	}
	created, err := t.creator.CreateAgentQuestion(ctx, interaction.AgentQuestionRequest{
		UserID:    scope.OwnerID,
		ProjectID: scope.ProjectID, SessionID: scope.SessionID, WorkflowID: scope.WorkflowID,
		TaskID: scope.TaskID, RunID: scope.RunID, SourceAgentID: scope.AgentID,
		InteractionMode: scope.InteractionMode,
		UIKind:          strings.TrimSpace(args.Presentation), Prompt: args.Prompt,
		Fields: args.Fields, AllowOther: args.AllowOther,
	})
	if err != nil {
		return "", &tool.Error{Code: "INTERACTION_CREATE_FAILED", Message: err.Error()}
	}
	return tool.OKResult(map[string]any{
		"interaction_id": created.ID,
		"status":         created.Status,
		"action":         "wait_for_user_answer",
	})
}
