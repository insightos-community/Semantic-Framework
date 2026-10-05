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
	"testing"

	"insightos.cn/semantic-framework/internal/interaction"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
)

type fakeQuestionCreator struct {
	request interaction.AgentQuestionRequest
	calls   int
}

func (f *fakeQuestionCreator) CreateAgentQuestion(_ context.Context,
	request interaction.AgentQuestionRequest) (store.Interaction, error) {
	f.calls++
	f.request = request
	return store.Interaction{ID: "interaction-a", Status: store.InteractionStatusPending}, nil
}

func questionScope(kind, mode string) context.Context {
	return tool.WithExecutionScope(context.Background(), tool.ExecutionScope{
		RunKind: kind, InteractionMode: mode, RunID: "run-a", AgentID: "leader",
		ProjectID: "project-a", SessionID: "conversation-a", WorkflowID: "workflow-a",
		TaskID: "task-a", OwnerID: "user-a", WorkspaceRoot: "/tmp/project-a",
	})
}

func TestInteractionAskUsesExecutionScopeAndTypedFields(t *testing.T) {
	creator := &fakeQuestionCreator{}
	result, err := (&interactionAskTool{creator: creator}).Run(
		questionScope(store.RunKindTaskExecution, "collaboration"), `{
			"prompt":"请确认目标和速度", "presentation":"form",
			"fields":[
				{"name":"target","label":"目标","value_type":"string","required":true},
				{"name":"speed","label":"速度","value_type":"number","required":true}
			]
		}`)
	if err != nil {
		t.Fatal(err)
	}
	request := creator.request
	if creator.calls != 1 || request.ProjectID != "project-a" ||
		request.UserID != "user-a" ||
		request.SessionID != "conversation-a" || request.WorkflowID != "workflow-a" ||
		request.TaskID != "task-a" || request.RunID != "run-a" ||
		request.SourceAgentID != "leader" || request.InteractionMode != "collaboration" ||
		request.UIKind != store.InteractionUIForm || len(request.Fields) != 2 {
		t.Fatalf("interaction.ask 没有使用精确 Run scope 或字段: %+v", request)
	}
	if result == "" {
		t.Fatal("工具应返回待回答 Interaction")
	}
}

func TestInteractionAskRejectsInvalidScopeAndQuestion(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		args string
	}{
		{name: "missing scope", ctx: context.Background(), args: `{"prompt":"确认？","presentation":"confirm"}`},
		{name: "missing prompt", ctx: questionScope(store.RunKindConversation, "plan"), args: `{"presentation":"confirm"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			creator := &fakeQuestionCreator{}
			if _, err := (&interactionAskTool{creator: creator}).Run(test.ctx, test.args); err == nil {
				t.Fatal("非法 interaction.ask 必须拒绝")
			}
			if creator.calls != 0 {
				t.Fatalf("非法请求不应进入 Interaction Service: %d", creator.calls)
			}
		})
	}
	creator := &fakeQuestionCreator{}
	if _, err := (&interactionAskTool{creator: creator}).Run(
		questionScope(store.RunKindTaskPlanning, ""),
		`{"prompt":"请选择需要搬运的箱体","presentation":"confirm"}`); err != nil || creator.calls != 1 {
		t.Fatalf("Task Planning在确有业务问题时应允许interaction.ask: calls=%d err=%v", creator.calls, err)
	}
}
