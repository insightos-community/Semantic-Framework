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

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/agent/team"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/llm"
)

// writeRoleFile 覆写角色目录下的单个文件（夹具定制用）。
func writeRoleFile(t *testing.T, root, role, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, role, name), []byte(content), 0o644); err != nil {
		t.Fatalf("写入 %s/%s 失败: %v", role, name, err)
	}
}

// TestLeaderDelegation 验证委派链路的 runtime 接线：leader 装配后工具集含
// ask_query（模型发起调用即执行——门禁清单含委派工具契约，L2 校验真实生效）、
// 冒泡事件转换为 subagent.* 下行（归因 query-1），leader 总结经
// message.done 收尾。
func TestLeaderDelegation(t *testing.T) {
	fx := newTestFixture(t)

	// 角色夹具：leader + query（启用 subagent，tool_name=ask_query）。
	root := writeTeamProfiles(t)
	queryYAML := "name: query\nmode: service\ndescription: 系统与产物查询助手\nmodel: mock\n" +
		"subagent: {enabled: true, tool_name: ask_query, tool_description: 系统状态与产物查询助手}\n"
	writeRoleFile(t, root, "query", "role.yaml", queryYAML)
	fx.loader = profile.NewLoader(root)

	// 模型按构建序注入：首次是 leader（runtimeFor），其后是 SubAgent。
	leaderMock := kernel.NewMockChatModel()
	leaderMock.SetScript(
		kernel.MockReply{ToolCalls: []schema.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: schema.FunctionCall{
				Name:      "ask_query",
				Arguments: `{"task":"查 artifact 列表"}`,
			},
		}}},
		kernel.MockReply{Content: "当前共有 3 个产物：report.pdf、log.txt、img.png。"},
	)
	queryMock := kernel.NewMockChatModel()
	queryMock.SetScript(
		kernel.MockReply{ToolCalls: []schema.ToolCall{{
			ID: "query-call-1", Type: "function",
			Function: schema.FunctionCall{Name: "get_weather", Arguments: `{"city":"北京"}`},
		}}},
		kernel.MockReply{Content: "当前产物：report.pdf、log.txt、img.png"},
	)
	var builds int32
	buildModel := func(context.Context, llm.Provider, string) (kernel.Model, error) {
		if atomic.AddInt32(&builds, 1) == 1 {
			return leaderMock, nil
		}
		return queryMock, nil
	}

	// 空但非 nil 的工具注册表 + 执行器：safety 管线真实挂接（门禁清单
	// 只有委派工具契约——它不经执行器执行，但必须过 L2 校验）。
	registry := tool.NewRegistry()
	executor := tool.NewExecutor(registry, tool.ExecutorOptions{})

	var toolHit int32
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
		BuildModel: buildModel, Registry: registry, Executor: executor,
		Tools: []kernel.Tool{newWeatherTool(t, &toolHit)},
	})

	def := &team.Def{
		Name:    "default",
		Leader:  team.MemberDef{ID: "leader", Role: "leader"},
		Members: []team.MemberDef{{ID: "query-1", Role: "query"}},
	}
	if err := svc.AssembleTeam(context.Background(), def); err != nil {
		t.Fatalf("AssembleTeam 失败: %v", err)
	}

	runID, err := svc.HandleMessage(context.Background(), "u1", "", "查一下有哪些产物")
	if err != nil {
		t.Fatalf("HandleMessage 失败: %v", err)
	}

	// ① ask_query 被执行（query 模型收到委派任务，输入为干净的任务文本）。
	if got := queryMock.GenerateCallCount() + queryMock.StreamCallCount(); got != 2 {
		t.Fatalf("query 模型应被调用 2 次（工具调用 + 总结），实际: %d", got)
	}
	var taskText string
	for _, msg := range queryMock.CallInputs()[0] {
		if msg.Role == schema.User {
			taskText = msg.Content
		}
	}
	if taskText != "查 artifact 列表" {
		t.Errorf("query 收到的用户消息应为任务文本，实际: %q", taskText)
	}

	// ② 下行事件：subagent.delta / subagent.result 归因 query-1；
	// message.done 归因 leader 且携带总结文本。
	var subDelta, subResult, leaderDone, subToolCall, subToolResult bool
	for _, ev := range drainEvents(fx.events) {
		env, ok := ev.Payload.(ws.Envelope)
		if !ok {
			continue
		}
		switch env.Type {
		case EventTypeToolCall:
			if env.Agent.ID == "query-1" {
				subToolCall = true
				if p, ok := env.Payload.(ToolCallPayload); !ok || p.CallID != "query-call-1" || p.Name != "get_weather" {
					t.Errorf("SubAgent tool.call 负载不符: %+v", env.Payload)
				}
			} else {
				t.Errorf("SubAgent 工具调用不得归因 Leader: %+v", env.Agent)
			}
		case EventTypeToolResult:
			if env.Agent.ID == "query-1" {
				subToolResult = true
			} else {
				t.Errorf("SubAgent 工具结果不得归因 Leader: %+v", env.Agent)
			}
		case EventTypeSubAgentDelta:
			subDelta = true
			if env.Agent.ID != "query-1" || env.Agent.Role != "service" {
				t.Errorf("subagent.delta 归因不符: %+v", env.Agent)
			}
		case EventTypeSubAgentResult:
			subResult = true
			p, ok := env.Payload.(SubAgentResultPayload)
			if !ok {
				t.Errorf("subagent.result 负载类型不符: %T", env.Payload)
				continue
			}
			if p.RunID != runID || p.Task != "查 artifact 列表" ||
				p.Text != "当前产物：report.pdf、log.txt、img.png" {
				t.Errorf("subagent.result 负载不符: %+v", p)
			}
		case EventTypeMessageDone:
			leaderDone = true
			if env.Agent.ID != "leader" {
				t.Errorf("message.done 应归因 leader，实际: %+v", env.Agent)
			}
			if p, ok := env.Payload.(MessageDonePayload); !ok ||
				p.Text != "当前共有 3 个产物：report.pdf、log.txt、img.png。" {
				t.Errorf("message.done 负载不符: %+v", env.Payload)
			}
		}
	}
	if !subDelta || !subResult || !leaderDone || !subToolCall || !subToolResult {
		t.Errorf("下行事件不全: delta=%v result=%v tool_call=%v tool_result=%v done=%v",
			subDelta, subResult, subToolCall, subToolResult, leaderDone)
	}

}

// TestTextLeaderDelegatesImageToVisionQuery 覆盖真实组合边界：文本 Leader 只
// 读取 ArtifactRef 并调用 ask_query，视觉 Query 使用自己的会话模型接收图片
// 本体。两者的模型能力和输入不能相互污染。
func TestTextLeaderDelegatesImageToVisionQuery(t *testing.T) {
	fx := newTestFixture(t)
	root := writeTeamProfiles(t)
	writeRoleFile(t, root, "leader", "role.yaml",
		"name: leader\nmode: coordinator\ndescription: 文本协调者\nmodel: leader-text\n")
	writeRoleFile(t, root, "query", "role.yaml",
		"name: query\nmode: service\ndescription: 视觉查询助手\nmodel: query-vision\n"+
			"subagent: {enabled: true, tool_name: ask_query, tool_description: 视觉查询助手}\n")
	fx.loader = profile.NewLoader(root)
	if err := fx.llmReg.Reload(config.LLMConfig{Default: "leader-text",
		Providers: map[string]config.LLMProviderConfig{
			"leader-text":  {Component: "mock", Model: "leader-text-model", Capabilities: []string{"text"}},
			"query-vision": {Component: "mock", Model: "query-vision-model", Capabilities: []string{"text", "image"}},
		}}); err != nil {
		t.Fatalf("重载多 Agent 模型注册表失败: %v", err)
	}
	image, err := fx.st.PutUserArtifact("usr-vision", "image/png", "模型截图", "{}", []byte("png"))
	if err != nil {
		t.Fatalf("写入图片 Artifact 失败: %v", err)
	}

	leaderMock := kernel.NewMockChatModel()
	leaderMock.SetScript(
		kernel.MockReply{ToolCalls: []schema.ToolCall{{
			ID: "call-image", Type: "function",
			Function: schema.FunctionCall{Name: "ask_query", Arguments: `{"task":"识别当前图片中的模型名称",` +
				`"artifact_refs":["` + image.ID + `"]}`},
		}}},
		kernel.MockReply{Content: "视觉助手确认图片中的模型是 MiniMax-M3。"},
	)
	queryMock := kernel.NewMockChatModel()
	queryMock.SetResponse("图片中的模型是 MiniMax-M3")
	var builtModels []string
	buildModel := func(_ context.Context, entry llm.Provider, _ string) (kernel.Model, error) {
		builtModels = append(builtModels, entry.Model)
		if entry.Model == "query-vision-model" {
			return queryMock, nil
		}
		return leaderMock, nil
	}

	registry := tool.NewRegistry()
	executor := tool.NewExecutor(registry, tool.ExecutorOptions{})
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
		BuildModel: buildModel, Registry: registry, Executor: executor,
	})
	if err := svc.AssembleTeam(context.Background(), &team.Def{Name: "default",
		Leader:  team.MemberDef{ID: "leader", Role: "leader"},
		Members: []team.MemberDef{{ID: "query-1", Role: "query"}},
	}); err != nil {
		t.Fatalf("AssembleTeam 失败: %v", err)
	}
	if _, err := svc.HandleMessageWithAttachments(context.Background(), "usr-vision", "",
		"让 Query Agent 看看这是什么模型", []string{image.ID}); err != nil {
		t.Fatalf("图片委派全链路失败: %v", err)
	}

	if len(builtModels) != 2 || builtModels[0] != "leader-text-model" ||
		builtModels[1] != "query-vision-model" {
		t.Fatalf("Leader 与 Query 应使用独立会话模型，实际: %v", builtModels)
	}
	leaderInput := leaderMock.CallInputs()[0]
	var leaderUser *schema.Message
	for _, message := range leaderInput {
		if message.Role == schema.User {
			leaderUser = message
		}
	}
	if leaderUser == nil || len(leaderUser.UserInputMultiContent) != 2 ||
		leaderUser.UserInputMultiContent[1].Image != nil ||
		!strings.Contains(leaderUser.UserInputMultiContent[1].Text, image.URI) {
		t.Fatalf("文本 Leader 应只收到图片引用: %+v", leaderUser)
	}
	queryInputs := queryMock.CallInputs()
	if len(queryInputs) != 1 {
		t.Fatalf("Query 模型应调用一次，实际: %d", len(queryInputs))
	}
	var queryUser *schema.Message
	for _, message := range queryInputs[0] {
		if message.Role == schema.User {
			queryUser = message
		}
	}
	if queryUser == nil || len(queryUser.UserInputMultiContent) != 2 ||
		queryUser.UserInputMultiContent[1].Image == nil ||
		queryUser.UserInputMultiContent[1].Image.MIMEType != "image/png" {
		t.Fatalf("视觉 Query 应收到任务文本和图片本体: %+v", queryUser)
	}
	querySnapshot, err := fx.st.GetSessionAgentModel(
		fxSessionID(t, fx.st, "usr-vision"), "query-1")
	if err != nil || querySnapshot.EndpointID != "query-vision" {
		t.Fatalf("Query 会话模型快照不符: snapshot=%+v err=%v", querySnapshot, err)
	}
}

// fxSessionID 返回测试用户唯一会话 ID，减少组合验收中的样板查询。
func fxSessionID(t *testing.T, st *store.Store, userID string) string {
	t.Helper()
	sessions, err := st.ListChatSessionsByUser(userID)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("读取测试会话失败: sessions=%d err=%v", len(sessions), err)
	}
	return sessions[0].ID
}

// TestAssembleTeamSubAgentRoster 验证启用 subagent 的 service 成员的
// roster 活动描述（可委派的待命形态）。
func TestAssembleTeamSubAgentRoster(t *testing.T) {
	fx := newTestFixture(t)
	root := writeTeamProfiles(t)
	writeRoleFile(t, root, "query", "role.yaml",
		"name: query\nmode: service\ndescription: 查询助手\nmodel: mock\n"+
			"subagent: {enabled: true, tool_name: ask_query, tool_description: 查询}\n")
	fx.loader = profile.NewLoader(root)

	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
	})
	if err := svc.AssembleTeam(context.Background(), testDef()); err != nil {
		t.Fatalf("AssembleTeam 失败: %v", err)
	}
	defer svc.Shutdown()

	query := rosterEntry(t, svc, "query-1")
	if query.Status != AgentStatusIdle || query.Activity != "待命：可经 ask_query 委派" {
		t.Errorf("query-1 条目不符: %+v", query)
	}
}
