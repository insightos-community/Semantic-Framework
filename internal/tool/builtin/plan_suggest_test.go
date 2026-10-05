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
	"errors"
	"strings"
	"testing"

	"insightos.cn/semantic-framework/internal/interaction"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
)

type fakePlanSubmitter struct {
	request interaction.PlanSuggestionRequest
	err     error
	calls   int
}

func (f *fakePlanSubmitter) SubmitPlanProposal(_ context.Context,
	request interaction.PlanSuggestionRequest) (store.PlanProposal, error) {
	f.calls++
	f.request = request
	if f.err != nil {
		return store.PlanProposal{}, f.err
	}
	return store.PlanProposal{ID: "plan-proposal", Revision: 2,
		Status: store.PlanProposalStatusReady}, nil
}

func planSuggestionScope(kind, mode string) context.Context {
	return tool.WithExecutionScope(context.Background(), tool.ExecutionScope{
		RunKind: kind, InteractionMode: mode, RunID: "run-leader", AgentID: "leader",
		ProjectID: "project-a", SessionID: "conversation-a", OwnerID: "user-a",
		WorkspaceRoot: "/tmp/project-a",
	})
}

const validPlanSuggestion = `{
	"goal":"生成可执行计划",
	"summary":"将 box-17 搬运到目标位置",
	"approved_scope":{"robot_ids":["r1pro-fake-02"],"allowed_skills":["grasp-object"]},
	"tasks":[{
		"id":"task-move","required_role":"robot",
		"required_capabilities":["grasp-object"],
		"resource_requirements":{"robot_ids":["r1pro-fake-02"]},
		"goal":"搬运 box-17"
	}]
}`

func TestPlanSuggestSubmitsCompleteProposalInExplicitPlanMode(t *testing.T) {
	creator := &fakePlanSubmitter{}
	result, err := (&planSuggestTool{creator: creator}).Run(
		planSuggestionScope(store.RunKindConversation, "plan"), validPlanSuggestion)
	if err != nil {
		t.Fatal(err)
	}
	if creator.calls != 1 || creator.request.UserID != "user-a" ||
		creator.request.ProjectID != "project-a" ||
		creator.request.SessionID != "conversation-a" ||
		creator.request.RunID != "run-leader" ||
		creator.request.SourceAgentID != "leader" ||
		len(creator.request.Tasks) != 1 ||
		creator.request.Tasks[0].RequiredRole != "robot" {
		t.Fatalf("plan.suggest 未提交精确 Run scope 与 Task: %+v", creator)
	}
	if !strings.Contains(result, "plan-proposal") || !strings.Contains(result, "review_plan") {
		t.Fatalf("工具结果未返回可审阅 Proposal: %s", result)
	}
}

func TestPlanSuggestAllowsRobotSkillLateBindingWithoutCapabilities(t *testing.T) {
	creator := &fakePlanSubmitter{}
	args := `{"goal":"后绑定 Robot Skill","summary":"由 Robot Agent 读取实际目录",` +
		`"tasks":[{"required_role":"robot","resource_requirements":{"robot_ids":["r1"]},` +
		`"goal":"搬运一个箱子"}]}`
	if _, err := (&planSuggestTool{creator: creator}).Run(
		planSuggestionScope(store.RunKindConversation, "plan"), args); err != nil {
		t.Fatal(err)
	}
	if creator.calls != 1 {
		t.Fatalf("省略 required_capabilities 的 Robot Task 应保持后绑定: %d", creator.calls)
	}
}

func TestPlanSuggestRequiresTasksAndExactPlanScope(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
		args string
	}{
		{name: "missing scope", ctx: context.Background(), args: validPlanSuggestion},
		{name: "collaboration mode", ctx: planSuggestionScope(store.RunKindConversation, "collaboration"), args: validPlanSuggestion},
		{name: "task run", ctx: planSuggestionScope(store.RunKindTaskExecution, "plan"), args: validPlanSuggestion},
		{name: "empty goal", ctx: planSuggestionScope(store.RunKindConversation, "plan"), args: `{"goal":"  ","tasks":[{"kind":"generic","required_role":"developer","goal":"x"}]}`},
		{name: "missing tasks", ctx: planSuggestionScope(store.RunKindConversation, "plan"), args: `{"goal":"计划"}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			creator := &fakePlanSubmitter{}
			if _, err := (&planSuggestTool{creator: creator}).Run(test.ctx, test.args); err == nil {
				t.Fatal("非法 plan.suggest 必须拒绝")
			}
			if creator.calls != 0 {
				t.Fatalf("非法请求不得进入 Proposal Service: %d", creator.calls)
			}
		})
	}
}

func TestPlanSuggestRejectsInvalidStructuredPlaceholders(t *testing.T) {
	task := `"tasks":[{"required_role":"developer","goal":"x"}]`
	tests := []struct {
		name string
		args string
		want string
	}{
		{name: "constraints", args: `{"goal":"计划","summary":"摘要","constraints":true,` + task + `}`, want: "constraints 必须是数组或对象"},
		{name: "criteria", args: `{"goal":"计划","summary":"摘要","completion_criteria":true,` + task + `}`, want: "completion_criteria 必须是数组或对象"},
		{name: "map", args: `{"goal":"计划","summary":"摘要","map_binding":true,` + task + `}`, want: "map_binding 必须是包含"},
		{name: "map fields", args: `{"goal":"计划","summary":"摘要","map_binding":{"map_id":"simulation_map","generation":1,"selections":[]},` + task + `}`, want: "map_binding 必须是有效"},
		{name: "task role", args: `{"goal":"计划","summary":"摘要","tasks":[{"required_role":"Robot Agent","goal":"x"}]}`, want: "required_role 必须是 robot"},
		{name: "task resources", args: `{"goal":"计划","summary":"摘要","tasks":[{"required_role":"robot","resource_requirements":{"robot":"r1"},"goal":"x"}]}`, want: "resource_requirements 只允许"},
		{name: "environment category is not backend", args: `{"goal":"计划","summary":"摘要","tasks":[{"required_role":"robot","resource_requirements":{"backends":["simulation"]},"goal":"x"}]}`, want: "simulation是环境类别"},
		{name: "robot capability without approved skills", args: `{"goal":"计划","summary":"摘要","tasks":[{"required_role":"robot","required_capabilities":["抓取并整理携物姿态"],"goal":"x"}]}`, want: "必须同时声明 approved_scope.allowed_skills"},
		{name: "task capability", args: `{"goal":"计划","summary":"摘要","approved_scope":{"allowed_skills":["grasp-object"]},"tasks":[{"required_role":"robot","required_capabilities":["grasp-object","invented-capability"],"goal":"x"}]}`, want: "不在 approved_scope.allowed_skills 内"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			creator := &fakePlanSubmitter{}
			_, err := (&planSuggestTool{creator: creator}).Run(
				planSuggestionScope(store.RunKindConversation, "plan"), test.args)
			var toolErr *tool.Error
			if !errors.As(err, &toolErr) || toolErr.Code != "BAD_ARGUMENTS" ||
				!strings.Contains(toolErr.Message, test.want) {
				t.Fatalf("应返回可纠正的 BAD_ARGUMENTS: %v", err)
			}
			if creator.calls != 0 {
				t.Fatalf("非法结构化参数不得提交 Proposal: %d", creator.calls)
			}
		})
	}
}

func TestPlanSuggestReturnsServiceError(t *testing.T) {
	creator := &fakePlanSubmitter{err: errors.New("store failed")}
	_, err := (&planSuggestTool{creator: creator}).Run(
		planSuggestionScope(store.RunKindConversation, "plan"), validPlanSuggestion)
	var toolErr *tool.Error
	if !errors.As(err, &toolErr) || toolErr.Code != "PLAN_SUGGEST_FAILED" || creator.calls != 1 {
		t.Fatalf("Service 错误必须原样映射到工具错误: calls=%d err=%v", creator.calls, err)
	}
}
