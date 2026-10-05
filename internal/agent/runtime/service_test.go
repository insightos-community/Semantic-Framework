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
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/agent/team"
	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/skill"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/llm"
	"insightos.cn/semantic-framework/pkg/log"
)

// cancelBlockingModel 只阻塞第一次流式调用，直到 run context 被取消；第二次
// 调用走标准 mock。它覆盖真实的“停止后马上续聊”竞态，而不是手工伪造历史。
type cancelBlockingModel struct {
	*kernel.MockChatModel
	started chan struct{}
	calls   int32
}

func (m *cancelBlockingModel) Stream(ctx context.Context, input []*schema.Message,
	opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	if atomic.AddInt32(&m.calls, 1) == 1 {
		close(m.started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return m.MockChatModel.Stream(ctx, input, opts...)
}

// weatherInput 是天气查询工具的入参。
type weatherInput struct {
	// City 城市名。
	City string `json:"city" jsonschema:"description=城市名"`
}

// testFixture 是 runtime 单测的公共装配：临时 profile、mock 注册表、临时库、事件总线。
type testFixture struct {
	st          *store.Store
	loader      *profile.Loader
	profileRoot string
	llmReg      *llm.Registry
	bus         *event.Bus
	logger      *log.Logger
	events      <-chan event.Event
}

// newTestFixture 创建公共装配：leader profile（含 AGENT.md 与 SAFETY.md）、
// mock 模型注册表、已迁移的临时库、订阅好 agent.events 的总线。
func newTestFixture(t *testing.T) *testFixture {
	t.Helper()
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})

	// 角色 profile 夹具。
	root := t.TempDir()
	dir := filepath.Join(root, "leader")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("创建角色目录失败: %v", err)
	}
	files := map[string]string{
		"role.yaml": "name: leader\nmode: coordinator\ndescription: 团队指挥官\nmodel: mock\n",
		"AGENT.md":  "# Role\n你是 Leader。",
		"SAFETY.md": "# 安全总则\n危险操作必须审批。",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("写入 %s 失败: %v", name, err)
		}
	}

	// mock 注册表：profile 的 model 字段指向 "mock" 端点。
	llmReg, err := llm.Load(config.LLMConfig{
		Default: "mock",
		Providers: map[string]config.LLMProviderConfig{
			"mock": {Component: "mock", Model: "mock"},
		},
	})
	if err != nil {
		t.Fatalf("构建 LLM 注册表失败: %v", err)
	}

	// 已迁移库取共享模板的私有副本：Migrate 在 -race + coverage 下单次约 1.1 秒，
	// 本包有 60 多个测试要建库，逐个重建会让固定成本高过被测逻辑。
	st := storetest.OpenMigrated(t, logger)

	bus := event.NewBus(logger)
	return &testFixture{
		st:          st,
		loader:      profile.NewLoader(root),
		profileRoot: root,
		llmReg:      llmReg,
		bus:         bus,
		logger:      logger,
		events:      bus.Subscribe(event.TopicAgentEvents),
	}
}

// TestProjectSkillReader 验证运行时只向 Agent 注入 Profile 白名单与 Project
// 绑定的有效交集；Project 空绑定只表示不额外收窄，不能扩大 Agent 白名单。
func TestProjectSkillReader(t *testing.T) {
	fx := newTestFixture(t)
	skillsRoot := t.TempDir()
	for _, name := range []string{"alpha", "beta", "gamma"} {
		dir := filepath.Join(skillsRoot, "general", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		content := "---\nname: " + name + "\ndescription: 运行时测试\ncategory: general\n---\n# 正文\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	skillStore, err := skill.NewStore(skillsRoot, fx.logger)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus,
		Logger: fx.logger, SkillStore: skillStore,
	})
	project, err := fx.st.EnsureDefaultProject("usr-skill")
	if err != nil {
		t.Fatal(err)
	}
	prof := &profile.Profile{Skills: profile.SkillsConfig{Allowlist: []string{"alpha", "beta"}}}
	unrestricted, err := svc.projectSkillReader(project.ID, prof)
	if err != nil || unrestricted == nil || len(unrestricted.List()) != 2 {
		t.Fatalf("Project 空绑定应保留两个 Agent Skill: skills=%v err=%v", unrestricted, err)
	}
	if err := fx.st.ReplaceProjectBindings(context.Background(), project.ID,
		store.ProjectBindings{SkillNames: []string{"beta", "gamma"}}); err != nil {
		t.Fatal(err)
	}
	restricted, err := svc.projectSkillReader(project.ID, prof)
	if err != nil || restricted == nil {
		t.Fatalf("读取 Project Skill 交集失败: skills=%v err=%v", restricted, err)
	}
	got := restricted.List()
	if len(got) != 1 || got[0].Name != "beta" {
		t.Fatalf("有效 Skill 应只有 beta，实际: %+v", got)
	}
	if empty, err := svc.projectSkillReader(project.ID, &profile.Profile{}); err != nil || empty != nil {
		t.Fatalf("Agent 空白名单应关闭 Skill middleware: skills=%v err=%v", empty, err)
	}
}

// TestSubAgentUsesOwnSessionModel 验证 Leader 装配委派工具时按
// session_id + agent_id 解析 Query 快照，而不是继承 Leader 或回退 Mock。
func TestSubAgentUsesOwnSessionModel(t *testing.T) {
	fx := newTestFixture(t)
	queryDir := filepath.Join(fx.profileRoot, "query")
	if err := os.MkdirAll(queryDir, 0o755); err != nil {
		t.Fatalf("创建 Query Profile 目录失败: %v", err)
	}
	queryRole := `name: query
mode: service
model: mock-query
limits:
  max_turns: 3
subagent:
  enabled: true
  tool_name: ask_query
  tool_description: 查询助手
`
	if err := os.WriteFile(filepath.Join(queryDir, "role.yaml"), []byte(queryRole), 0o644); err != nil {
		t.Fatalf("写入 Query Profile 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(queryDir, "AGENT.md"),
		[]byte("# Query\n独立处理查询。"), 0o644); err != nil {
		t.Fatalf("写入 Query 指令失败: %v", err)
	}
	if err := fx.llmReg.Reload(config.LLMConfig{Default: "mock",
		Providers: map[string]config.LLMProviderConfig{
			"mock":       {Component: "mock", Model: "leader-model"},
			"mock-query": {Component: "mock", Model: "query-model"},
		}}); err != nil {
		t.Fatalf("重载模型注册表失败: %v", err)
	}
	var builtModels []string
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
		BuildModel: func(_ context.Context, entry llm.Provider, _ string) (kernel.Model, error) {
			builtModels = append(builtModels, entry.Model)
			return kernel.NewMockChatModel(), nil
		},
	})
	if err := svc.AssembleTeam(context.Background(), &team.Def{Name: "default",
		Leader:  team.MemberDef{ID: "leader", Role: "leader"},
		Members: []team.MemberDef{{ID: "query-1", Role: "query"}},
	}); err != nil {
		t.Fatalf("组建测试 Team 失败: %v", err)
	}
	now := time.Now().UTC()
	if err := fx.st.CreateChatSession(store.ChatSession{ID: "cs-sub-model", UserID: "usr-1",
		Title: "委派模型", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if _, err := svc.runtimeFor(context.Background(), "cs-sub-model"); err != nil {
		t.Fatalf("构建会话运行器失败: %v", err)
	}
	if len(builtModels) != 2 || builtModels[0] != "leader-model" || builtModels[1] != "query-model" {
		t.Fatalf("SubAgent 与 Leader 应分别使用自身快照，实际构建顺序: %v", builtModels)
	}
	querySnapshot, err := fx.st.GetSessionAgentModel("cs-sub-model", "query-1")
	if err != nil || querySnapshot.EndpointID != "mock-query" {
		t.Fatalf("Query 会话快照不符: snapshot=%+v err=%v", querySnapshot, err)
	}
}

// TestSubAgentInheritsSystemDefault 验证 Query Profile 未指定模型时，会话首次
// 装配把系统 Default 固化为 Query 自己的快照，而不是退回名为 mock 的端点，
// 也不是继承 Leader Profile 的固定模型。
func TestSubAgentInheritsSystemDefault(t *testing.T) {
	fx := newTestFixture(t)
	queryDir := filepath.Join(fx.profileRoot, "query")
	if err := os.MkdirAll(queryDir, 0o755); err != nil {
		t.Fatalf("创建 Query Profile 目录失败: %v", err)
	}
	queryRole := `name: query
mode: service
subagent:
  enabled: true
  tool_name: ask_query
  tool_description: 查询助手
`
	if err := os.WriteFile(filepath.Join(queryDir, "role.yaml"), []byte(queryRole), 0o644); err != nil {
		t.Fatalf("写入 Query Profile 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(queryDir, "AGENT.md"),
		[]byte("# Query\n使用系统默认模型处理查询。"), 0o644); err != nil {
		t.Fatalf("写入 Query 指令失败: %v", err)
	}
	if err := fx.llmReg.Reload(config.LLMConfig{Default: "default-endpoint",
		Providers: map[string]config.LLMProviderConfig{
			"mock":             {Component: "mock", Model: "leader-model"},
			"default-endpoint": {Component: "mock", Model: "default-model"},
		}}); err != nil {
		t.Fatalf("重载模型注册表失败: %v", err)
	}
	var builtModels []string
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
		BuildModel: func(_ context.Context, entry llm.Provider, _ string) (kernel.Model, error) {
			builtModels = append(builtModels, entry.Model)
			return kernel.NewMockChatModel(), nil
		},
	})
	if err := svc.AssembleTeam(context.Background(), &team.Def{Name: "default",
		Leader:  team.MemberDef{ID: "leader", Role: "leader"},
		Members: []team.MemberDef{{ID: "query-1", Role: "query"}},
	}); err != nil {
		t.Fatalf("组建测试 Team 失败: %v", err)
	}
	now := time.Now().UTC()
	if err := fx.st.CreateChatSession(store.ChatSession{ID: "cs-sub-default", UserID: "usr-1",
		Title: "默认模型委派", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if _, err := svc.runtimeFor(context.Background(), "cs-sub-default"); err != nil {
		t.Fatalf("构建会话运行器失败: %v", err)
	}
	if len(builtModels) != 2 || builtModels[0] != "leader-model" || builtModels[1] != "default-model" {
		t.Fatalf("Leader 应用 Profile、Query 应用系统 Default，实际构建顺序: %v", builtModels)
	}
	querySnapshot, err := fx.st.GetSessionAgentModel("cs-sub-default", "query-1")
	if err != nil || querySnapshot.EndpointID != "default-endpoint" ||
		querySnapshot.Source != store.ModelSourceSystemDefault {
		t.Fatalf("Query Default 会话快照不符: snapshot=%+v err=%v", querySnapshot, err)
	}
}

// newWeatherTool 构建计数的天气工具（验证 agent loop 的工具调用路径）。
func newWeatherTool(t *testing.T, hit *int32) kernel.Tool {
	t.Helper()
	weatherTool, err := utils.InferTool("get_weather", "查询城市天气",
		func(_ context.Context, in weatherInput) (string, error) {
			atomic.AddInt32(hit, 1)
			return "晴，25°C", nil
		})
	if err != nil {
		t.Fatalf("构建工具失败: %v", err)
	}
	return weatherTool
}

// drainEvents 排空总线事件缓冲（HandleMessage 同步执行，返回时事件已全部入缓冲）。
func drainEvents(ch <-chan event.Event) []event.Event {
	var events []event.Event
	for {
		select {
		case ev := <-ch:
			events = append(events, ev)
		default:
			return events
		}
	}
}

// messageRoles 提取模型输入消息的 (role, content) 序列，便于断言上下文组装。
func messageRoles(msgs []*schema.Message) [][2]string {
	out := make([][2]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, [2]string{string(m.Role), m.Content})
	}
	return out
}

// containsSubsequence 断言 want 是 seq 的保序子序列。
func containsSubsequence(seq [][2]string, want ...[2]string) bool {
	i := 0
	for _, item := range seq {
		if i < len(want) && item == want[i] {
			i++
		}
	}
	return i == len(want)
}

// TestHandleMessageFlow 全流程：mock 脚本"先工具调用、后总结"，
// 断言消息落库顺序、envelope delta 序列、run_sessions 状态迁移、工具执行。
func TestHandleMessageFlow(t *testing.T) {
	fx := newTestFixture(t)

	m := kernel.NewMockChatModel()
	m.SetScript(
		kernel.MockReply{ToolCalls: []schema.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: schema.FunctionCall{
				Name:      "get_weather",
				Arguments: `{"city":"北京"}`,
			},
		}}},
		kernel.MockReply{Content: "北京今天晴，25°C。"},
	)
	var toolHit int32
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
		BuildModel: func(context.Context, llm.Provider, string) (kernel.Model, error) { return m, nil },
		Tools:      []kernel.Tool{newWeatherTool(t, &toolHit)},
	})

	runID, err := svc.HandleMessage(context.Background(), "usr-1", "", "北京天气怎么样？")
	if err != nil {
		t.Fatalf("HandleMessage 失败: %v", err)
	}
	if runID == "" {
		t.Fatal("runID 不应为空")
	}

	// ① 会话创建：标题取首条消息（不足 20 字取全文）。
	sessions, err := fx.st.ListChatSessionsByUser("usr-1")
	if err != nil || len(sessions) != 1 {
		t.Fatalf("应创建 1 个会话，实际: %v, %d", err, len(sessions))
	}
	sess := sessions[0]
	if sess.Title != "北京天气怎么样？" {
		t.Errorf("标题应取首条消息，实际: %q", sess.Title)
	}

	// ② 消息落库顺序：[user, assistant]。
	msgs, err := fx.st.ListChatMessages(sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("ListChatMessages 失败: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("应有 2 条消息，实际: %d", len(msgs))
	}
	if msgs[0].Message == nil || msgs[0].Message.Role != schema.User ||
		msgs[0].Message.Content != "北京天气怎么样？" {
		t.Errorf("首条应为用户消息，实际: %+v", msgs[0])
	}
	if msgs[1].Message == nil || msgs[1].Message.Role != schema.Assistant ||
		msgs[1].Message.Content != "北京今天晴，25°C。" {
		t.Errorf("次条应为助手总结，实际: %+v", msgs[1])
	}

	// ③ run session 状态迁移到 completed。
	run, err := fx.st.GetRunSession(runID)
	if err != nil {
		t.Fatalf("GetRunSession 失败: %v", err)
	}
	if run.Status != store.RunStatusCompleted || run.AgentName != "leader" ||
		run.ChatSessionID != sess.ID || run.EndedAt == nil {
		t.Errorf("run session 终态不符: %+v", run)
	}

	// ④ 工具被执行一次。
	if got := atomic.LoadInt32(&toolHit); got != 1 {
		t.Errorf("工具应执行 1 次，实际: %d", got)
	}

	// ⑤ envelope 序列：run.started → tool.call → tool.result → delta →
	// run.completed → message.done。所有运行活动都精确关联 Run/Trace。
	events := drainEvents(fx.events)
	if len(events) != 6 {
		t.Fatalf("应下行 6 个运行事件，实际: %d", len(events))
	}
	envelopes := make([]ws.Envelope, len(events))
	for i, item := range events {
		var ok bool
		envelopes[i], ok = item.Payload.(ws.Envelope)
		if !ok {
			t.Fatalf("事件 %d 负载应为 ws.Envelope，实际: %T", i, item.Payload)
		}
		if envelopes[i].ProjectID != sess.ProjectID ||
			envelopes[i].Parent.RunID != runID || envelopes[i].Parent.TraceID != run.TraceID {
			t.Errorf("事件 %d 的 Project/Run/Trace 关联不符: %+v", i, envelopes[i])
		}
	}
	started := envelopes[0]
	if started.Type != EventTypeRunStarted || started.ResourceType != "agent_run" ||
		started.ResourceID != runID || started.Revision != 1 {
		t.Fatalf("首个事件应为 revision=1 的 run.started，实际: %+v", started)
	}
	startedPayload, ok := started.Payload.(map[string]any)
	startedRun, runOK := startedPayload["run"].(store.RunSession)
	if !ok || !runOK || startedRun.ID != runID || startedRun.Status != store.RunStatusRunning {
		t.Errorf("run.started 负载不符: %+v", started.Payload)
	}

	callEnv := envelopes[1]
	if callEnv.Type != EventTypeToolCall {
		t.Fatalf("第二个事件应为 tool.call，实际: %+v", callEnv)
	}
	callPayload, ok := callEnv.Payload.(ToolCallPayload)
	if !ok || callPayload.CallID != "call-1" || callPayload.Name != "get_weather" ||
		callPayload.Arguments != `{"city":"北京"}` {
		t.Errorf("tool.call 负载不符: %+v", callEnv.Payload)
	}
	toolEnv := envelopes[2]
	if toolEnv.Type != EventTypeToolResult {
		t.Fatalf("第三个事件应为 tool.result，实际: %+v", toolEnv)
	}
	toolPayload, ok := toolEnv.Payload.(ToolResultPayload)
	if !ok || toolPayload.RunID != runID || toolPayload.Name != "get_weather" ||
		!strings.Contains(toolPayload.Result, "25°C") {
		t.Errorf("tool.result 负载不符: %+v", toolEnv.Payload)
	}

	delta := envelopes[3]
	if delta.Channel != ws.ChannelDialogue || delta.Type != EventTypeMessageDelta ||
		delta.SessionID != sess.ID {
		t.Errorf("delta envelope 不符: %+v", delta)
	}
	if delta.Agent.ID != "leader" || delta.Agent.Role != "coordinator" {
		t.Errorf("delta agent 归因不符: %+v", delta.Agent)
	}
	deltaPayload, ok := delta.Payload.(MessageDeltaPayload)
	if !ok || deltaPayload.RunID != runID || deltaPayload.Text != "北京今天晴，25°C。" {
		t.Errorf("delta 负载不符: %+v", delta.Payload)
	}

	completed := envelopes[4]
	if completed.Type != EventTypeRunCompleted || completed.ResourceType != "agent_run" ||
		completed.ResourceID != runID || completed.Revision != run.Revision {
		t.Errorf("run.completed 事件不符: %+v", completed)
	}
	doneEnv := envelopes[5]
	if doneEnv.Type != EventTypeMessageDone {
		t.Fatalf("最后事件应为 message.done，实际: %+v", doneEnv)
	}
	donePayload, ok := doneEnv.Payload.(MessageDonePayload)
	if !ok {
		t.Fatalf("done 负载类型不符: %T", doneEnv.Payload)
	}
	if donePayload.RunID != runID || donePayload.Text != "北京今天晴，25°C。" ||
		donePayload.TraceID == "" || donePayload.Turns != 2 || donePayload.Error != "" {
		t.Errorf("done 负载不符: %+v", donePayload)
	}
	if donePayload.Usage == nil || donePayload.Usage.TotalTokens != 60 {
		t.Errorf("done 用量应为两轮累计 60，实际: %+v", donePayload.Usage)
	}
	if donePayload.Model == nil || donePayload.Model.RequestedEndpoint != "mock" ||
		donePayload.Model.ResolvedEndpoint != "mock" ||
		donePayload.Model.ResolvedModel != "mock" || donePayload.Model.Fallback {
		t.Errorf("done 应携带本轮实际模型解析结果，实际: %+v", donePayload.Model)
	}

	// ⑥ 系统提示 = AGENT.md + SAFETY 段（首轮模型输入首条）。
	inputs := m.CallInputs()
	if len(inputs) != 2 {
		t.Fatalf("模型应被调用 2 次，实际: %d", len(inputs))
	}
	first := messageRoles(inputs[0])
	if len(first) == 0 || first[0][0] != "system" {
		t.Fatalf("模型输入首条应为系统提示，实际: %+v", first)
	}
}

// TestEmbeddedThinkContentUsesReasoningChannel 验证服务商把思考内容内嵌在
// Content 的 <think> 标签中时，auto 模式会把它下发并归档到 reasoning，
// 助手正文与 message.done 只保留最终回答。
func TestEmbeddedThinkContentUsesReasoningChannel(t *testing.T) {
	fx := newTestFixture(t)
	model := kernel.NewMockChatModel()
	model.SetResponse("<think>先识别图片中的界面</think>这是对话界面。")
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
		BuildModel: func(context.Context, llm.Provider, string) (kernel.Model, error) {
			return model, nil
		},
	})
	if _, err := svc.HandleMessage(context.Background(), "usr-1", "", "分析图片"); err != nil {
		t.Fatalf("执行内嵌思考测试失败: %v", err)
	}

	sessions, _ := fx.st.ListChatSessionsByUser("usr-1")
	messages, _ := fx.st.ListChatMessages(sessions[0].ID, 0, 0)
	assistant := messages[len(messages)-1]
	var metadata RunMetadata
	if err := json.Unmarshal([]byte(assistant.Metadata), &metadata); err != nil {
		t.Fatalf("解析运行元数据失败: %v", err)
	}
	if assistant.Message == nil || assistant.Message.Content != "这是对话界面。" ||
		metadata.Reasoning != "先识别图片中的界面" {
		t.Fatalf("正文或思考归档错误: content=%q reasoning=%q",
			assistant.Message.Content, metadata.Reasoning)
	}

	var reasoningText, doneText string
	for _, event := range drainEvents(fx.events) {
		envelope, ok := event.Payload.(ws.Envelope)
		if !ok {
			continue
		}
		switch payload := envelope.Payload.(type) {
		case ReasoningDeltaPayload:
			reasoningText += payload.Text
		case MessageDonePayload:
			doneText = payload.Text
		}
	}
	if reasoningText != "先识别图片中的界面" || doneText != "这是对话界面。" {
		t.Fatalf("reasoning/message.done 下发错误: reasoning=%q done=%q",
			reasoningText, doneText)
	}
}

// TestHandleMessageHistoryReplay 验证第二条消息时历史完整重放给模型，
// 以及会话恢复（新建 Service 模拟进程重启）后历史延续。
func TestBoundedToolResult(t *testing.T) {
	short, truncated := boundedToolResult("短结果")
	if short != "短结果" || truncated {
		t.Fatalf("短结果不应截断: text=%q truncated=%v", short, truncated)
	}
	long := strings.Repeat("界", toolResultRuneLimit+7)
	got, truncated := boundedToolResult(long)
	if !truncated || len([]rune(got)) != toolResultRuneLimit {
		t.Fatalf("长结果应按 Unicode 字符截断到 %d，实际=%d truncated=%v",
			toolResultRuneLimit, len([]rune(got)), truncated)
	}
}

func TestAutomaticReasoningEffortDoesNotForceLevel(t *testing.T) {
	if got := effectiveReasoningEffort("auto"); got != "" {
		t.Errorf("auto 不应向模型发送固定 effort，实际: %q", got)
	}
	if got := effectiveReasoningEffort("medium"); got != "medium" {
		t.Errorf("显式 effort 应固定覆盖，实际: %q", got)
	}
	if got := robotExecutionReasoningEffort("auto"); got != "low" {
		t.Errorf("Robot执行参数组装默认应使用low，实际: %q", got)
	}
	if got := robotExecutionReasoningEffort("high"); got != "high" {
		t.Errorf("用户显式档位不应被执行期默认值覆盖，实际: %q", got)
	}
}

// TestModelResolutionDoesNotFallback 验证端点缺少密钥时直接失败，不能静默
// 改用全局默认模型，也不能调用任何模型构造逻辑。
func TestModelResolutionDoesNotFallback(t *testing.T) {
	fx := newTestFixture(t)
	t.Setenv(llm.APIKeyEnv("requested"), "")
	if err := fx.loader.UpdateModels("leader", "requested", "auto", "auto"); err != nil {
		t.Fatalf("设置测试 Agent 模型失败: %v", err)
	}
	if err := fx.llmReg.Reload(config.LLMConfig{
		Default: "fallback",
		Providers: map[string]config.LLMProviderConfig{
			"requested": {
				Component: "openai", BaseURL: "https://example.invalid/v1",
				Model: "requested-model",
			},
			"fallback": {Component: "mock", Model: "fallback-model"},
		},
	}); err != nil {
		t.Fatalf("重载测试模型注册表失败: %v", err)
	}
	built := false
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
		BuildModel: func(_ context.Context, _ llm.Provider, _ string) (kernel.Model, error) {
			built = true
			return kernel.NewMockChatModel(), nil
		},
	})
	if _, err := svc.HandleMessage(context.Background(), "usr-1", "", "检查模型错误"); err == nil ||
		!strings.Contains(err.Error(), "未配置 API Token") {
		t.Fatalf("缺少 Token 应返回明确错误，实际: %v", err)
	}
	if built {
		t.Fatal("缺少 Token 时不应构建默认模型")
	}
}

// TestUpdateAgentProfileOnlyAffectsNewSessions 验证 Agent Profile 变更只作为
// 后续新会话的默认值，已有会话的模型快照和 Runner 均保持不变。
func TestUpdateAgentProfileOnlyAffectsNewSessions(t *testing.T) {
	fx := newTestFixture(t)
	if err := fx.llmReg.Reload(config.LLMConfig{
		Default: "mock",
		Providers: map[string]config.LLMProviderConfig{
			"mock":     {Component: "mock", Model: "mock"},
			"mock-new": {Component: "mock", Model: "mock-new"},
		},
	}); err != nil {
		t.Fatalf("重载测试模型注册表失败: %v", err)
	}
	m := &cancelBlockingModel{MockChatModel: kernel.NewMockChatModel(), started: make(chan struct{})}
	m.SetResponse("模型响应")
	var builtModels []string
	var builtMu sync.Mutex
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
		BuildModel: func(_ context.Context, entry llm.Provider, _ string) (kernel.Model, error) {
			builtMu.Lock()
			builtModels = append(builtModels, entry.Model)
			builtMu.Unlock()
			return m, nil
		},
	})
	svc.roster.register(AgentInfo{ID: "leader", Role: "leader", Model: "mock"})

	firstDone := make(chan error, 1)
	go func() {
		_, err := svc.HandleMessage(context.Background(), "usr-1", "", "旧模型运行中")
		firstDone <- err
	}()
	select {
	case <-m.started:
	case <-time.After(runtimeEventWait):
		t.Fatalf("首轮没有在 %s 内进入模型", runtimeEventWait)
	}
	sessions, _ := fx.st.ListChatSessionsByUser("usr-1")
	sessionID := sessions[0].ID
	if err := svc.UpdateAgentModels("leader", "mock-new", "auto", "auto"); err != nil {
		t.Fatalf("运行中更新 Agent 模型失败: %v", err)
	}
	if err := svc.CancelRun(context.Background(), "usr-1", sessionID); err != nil {
		t.Fatalf("取消首轮失败: %v", err)
	}
	if err := <-firstDone; err != nil {
		t.Fatalf("取消首轮应正常收尾: %v", err)
	}

	svc.mu.Lock()
	_, cached := svc.sessions[sessionID]
	svc.mu.Unlock()
	if !cached {
		t.Fatal("Profile 变更不应淘汰已有会话 Runner")
	}
	if _, err := svc.HandleMessage(context.Background(), "usr-1", sessionID, "下一轮"); err != nil {
		t.Fatalf("已有会话下一轮执行失败: %v", err)
	}
	if _, err := svc.HandleMessage(context.Background(), "usr-1", "", "新会话"); err != nil {
		t.Fatalf("新会话执行失败: %v", err)
	}
	builtMu.Lock()
	defer builtMu.Unlock()
	if len(builtModels) != 2 || builtModels[0] != "mock" || builtModels[1] != "mock-new" {
		t.Fatalf("已有会话应保留旧模型且新会话采用新 Profile，实际: %v", builtModels)
	}
}

// TestSetSessionAgentModelRejectsBusyRun 验证活动 Run 期间拒绝切换；取消后
// 显式覆盖会淘汰旧 Runner，并在同一历史的下一轮使用新端点。
func TestSetSessionAgentModelRejectsBusyRun(t *testing.T) {
	fx := newTestFixture(t)
	if err := fx.llmReg.Reload(config.LLMConfig{
		Default: "mock",
		Providers: map[string]config.LLMProviderConfig{
			"mock":     {Component: "mock", Model: "mock"},
			"mock-new": {Component: "mock", Model: "mock-new"},
		},
	}); err != nil {
		t.Fatalf("重载模型注册表失败: %v", err)
	}
	m := &cancelBlockingModel{MockChatModel: kernel.NewMockChatModel(), started: make(chan struct{})}
	m.SetResponse("切换完成")
	var builtModels []string
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
		BuildModel: func(_ context.Context, entry llm.Provider, _ string) (kernel.Model, error) {
			builtModels = append(builtModels, entry.Model)
			return m, nil
		},
	})
	svc.roster.register(AgentInfo{ID: "leader", Role: "leader", Model: "mock"})

	firstDone := make(chan error, 1)
	go func() {
		_, err := svc.HandleMessage(context.Background(), "usr-1", "", "开始")
		firstDone <- err
	}()
	select {
	case <-m.started:
	case <-time.After(runtimeEventWait):
		t.Fatalf("首轮没有在 %s 内进入模型", runtimeEventWait)
	}
	sessions, _ := fx.st.ListChatSessionsByUser("usr-1")
	sessionID := sessions[0].ID
	if _, err := svc.SetSessionAgentModel("usr-1", sessionID, "leader", "mock-new", "auto"); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("活动 Run 应返回 ErrSessionBusy，实际: %v", err)
	}
	if err := svc.CancelRun(context.Background(), "usr-1", sessionID); err != nil {
		t.Fatalf("取消首轮失败: %v", err)
	}
	if err := <-firstDone; err != nil {
		t.Fatalf("取消首轮应正常收尾: %v", err)
	}
	view, err := svc.SetSessionAgentModel("usr-1", sessionID, "leader", "mock-new", "auto")
	if err != nil || view.Model != "mock-new" || view.Source != store.ModelSourceSessionOverride {
		t.Fatalf("会话模型覆盖失败: view=%+v err=%v", view, err)
	}
	if _, err := svc.HandleMessage(context.Background(), "usr-1", sessionID, "继续"); err != nil {
		t.Fatalf("切换后的下一轮失败: %v", err)
	}
	if len(builtModels) != 2 || builtModels[0] != "mock" || builtModels[1] != "mock-new" {
		t.Fatalf("运行器模型构建顺序不符: %v", builtModels)
	}
}

func TestPerRunReasoningOptionsAreTemporaryAndAuditable(t *testing.T) {
	fx := newTestFixture(t)
	m := kernel.NewMockChatModel()
	m.SetResponse("<think>不应展示的内部分析</think>完成")
	var efforts []string
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
		BuildModel: func(_ context.Context, provider llm.Provider, _ string) (kernel.Model, error) {
			effort, _ := provider.Options["reasoning_effort"].(string)
			efforts = append(efforts, effort)
			return m, nil
		},
	})
	if _, err := svc.HandleMessageWithOptions(context.Background(), "usr-1", "", "深度分析",
		nil, "high", "hide"); err != nil {
		t.Fatalf("单轮推理覆盖执行失败: %v", err)
	}
	if len(efforts) < 2 || efforts[0] != "" || efforts[len(efforts)-1] != "high" {
		t.Fatalf("应先构建不强制档位的 auto 基线，再构建本轮 high，实际: %v", efforts)
	}
	sessions, _ := fx.st.ListChatSessionsByUser("usr-1")
	messages, _ := fx.st.ListChatMessages(sessions[0].ID, 0, 0)
	var metadata RunMetadata
	if err := json.Unmarshal([]byte(messages[len(messages)-1].Metadata), &metadata); err != nil {
		t.Fatalf("解析运行元数据失败: %v", err)
	}
	if metadata.ReasoningEffort != "high" || metadata.ReasoningVisibility != "hide" {
		t.Fatalf("单轮推理策略应写入运行归档，实际: %+v", metadata)
	}
	if messages[len(messages)-1].Message == nil ||
		messages[len(messages)-1].Message.Content != "完成" || metadata.Reasoning != "" {
		t.Fatalf("hide 模式应从正文移除 think 且不归档思考，正文=%q reasoning=%q",
			messages[len(messages)-1].Message.Content, metadata.Reasoning)
	}
	base, err := svc.runtimeFor(context.Background(), sessions[0].ID)
	if err != nil {
		t.Fatalf("读取缓存运行态失败: %v", err)
	}
	if base.profile.ReasoningEffort != "auto" || base.profile.ReasoningVisibility != "auto" {
		t.Fatalf("单轮覆盖不应污染 Agent 默认配置: effort=%q visibility=%q",
			base.profile.ReasoningEffort, base.profile.ReasoningVisibility)
	}
	if _, err := svc.HandleMessageWithOptions(context.Background(), "usr-1", sessions[0].ID,
		"非法配置", nil, "max", "auto"); err == nil {
		t.Fatal("非法单轮推理强度应被拒绝")
	}
}

func TestHandleMessageWithImageAttachment(t *testing.T) {
	fx := newTestFixture(t)
	if err := fx.llmReg.Reload(config.LLMConfig{
		Default: "mock",
		Providers: map[string]config.LLMProviderConfig{
			"mock": {Component: "mock", Model: "mock", Capabilities: []string{"text", "image"}},
		},
	}); err != nil {
		t.Fatalf("Reload 视觉端点失败: %v", err)
	}
	image, err := fx.st.PutUserArtifact("usr-1", "image/png", "camera.png", "{}", []byte("png-data"))
	if err != nil {
		t.Fatalf("PutUserArtifact 失败: %v", err)
	}
	m := kernel.NewMockChatModel()
	m.SetResponse("看到了")
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
		BuildModel: func(context.Context, llm.Provider, string) (kernel.Model, error) { return m, nil },
	})
	if _, err := svc.HandleMessageWithAttachments(context.Background(), "usr-1", "", "分析", []string{image.ID}); err != nil {
		t.Fatalf("视觉消息失败: %v", err)
	}
	inputs := m.CallInputs()
	if len(inputs) != 1 {
		t.Fatalf("模型应调用一次，实际: %d", len(inputs))
	}
	found := false
	for _, msg := range inputs[0] {
		if msg.Role == schema.User && len(msg.UserInputMultiContent) == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("模型输入缺少图片 MultiContent: %+v", messageRoles(inputs[0]))
	}
	if _, err := svc.HandleMessageWithAttachments(context.Background(), "usr-2", "", "越权", []string{image.ID}); err == nil {
		t.Fatal("其他用户不得引用该图片")
	}
}

// TestTextModelReceivesImageReference 验证非视觉 Leader 不会把 Base64 图片发送
// 给文本端点，但仍能看到已校验 ArtifactRef，从而决定委派视觉 Agent。
func TestTextModelReceivesImageReference(t *testing.T) {
	fx := newTestFixture(t)
	image, err := fx.st.PutUserArtifact("usr-1", "image/png", "camera.png", "{}", []byte("png-data"))
	if err != nil {
		t.Fatalf("PutUserArtifact 失败: %v", err)
	}
	m := kernel.NewMockChatModel()
	m.SetResponse("我会委派视觉助手处理")
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
		BuildModel: func(context.Context, llm.Provider, string) (kernel.Model, error) { return m, nil },
	})
	if _, err := svc.HandleMessageWithAttachments(context.Background(), "usr-1", "", "让视觉助手分析",
		[]string{image.ID}); err != nil {
		t.Fatalf("文本模型接收图片引用失败: %v", err)
	}
	inputs := m.CallInputs()
	if len(inputs) != 1 {
		t.Fatalf("文本模型应调用一次，实际: %d", len(inputs))
	}
	var user *schema.Message
	for _, message := range inputs[0] {
		if message.Role == schema.User {
			user = message
		}
	}
	if user == nil || len(user.UserInputMultiContent) != 2 ||
		user.UserInputMultiContent[1].Image != nil ||
		!strings.Contains(user.UserInputMultiContent[1].Text, image.URI) ||
		!strings.Contains(user.UserInputMultiContent[1].Text, "可委派给视觉 Agent") {
		t.Fatalf("文本模型应收到图片引用而非本体: %+v", user)
	}
}

func TestHandleMessageHistoryReplay(t *testing.T) {
	fx := newTestFixture(t)

	m := kernel.NewMockChatModel()
	m.SetResponse("第一轮回答")
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
		BuildModel: func(context.Context, llm.Provider, string) (kernel.Model, error) { return m, nil },
	})

	if _, err := svc.HandleMessage(context.Background(), "usr-1", "", "第一轮问题"); err != nil {
		t.Fatalf("第一条消息失败: %v", err)
	}
	sessions, _ := fx.st.ListChatSessionsByUser("usr-1")
	sessID := sessions[0].ID

	if _, err := svc.HandleMessage(context.Background(), "usr-1", sessID, "第二轮问题"); err != nil {
		t.Fatalf("第二条消息失败: %v", err)
	}

	// 第二轮的模型输入应包含完整历史（第一轮问答）+ 新消息，保序。
	inputs := m.CallInputs()
	if len(inputs) != 2 {
		t.Fatalf("模型应被调用 2 次，实际: %d", len(inputs))
	}
	seq := messageRoles(inputs[1])
	if !containsSubsequence(seq,
		[2]string{"user", "第一轮问题"},
		[2]string{"assistant", "第一轮回答"},
		[2]string{"user", "第二轮问题"},
	) {
		t.Errorf("第二轮模型输入应保序包含历史与新消息，实际: %+v", seq)
	}

	// 模拟进程重启：新建 Service（缓存为空），同库同会话再发一条。
	m2 := kernel.NewMockChatModel()
	m2.SetResponse("恢复后回答")
	svc2 := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
		BuildModel: func(context.Context, llm.Provider, string) (kernel.Model, error) { return m2, nil },
	})
	if _, err := svc2.HandleMessage(context.Background(), "usr-1", sessID, "第三轮问题"); err != nil {
		t.Fatalf("重启后消息失败: %v", err)
	}

	inputs2 := m2.CallInputs()
	if len(inputs2) != 1 {
		t.Fatalf("重启后模型应被调用 1 次，实际: %d", len(inputs2))
	}
	seq2 := messageRoles(inputs2[0])
	if !containsSubsequence(seq2,
		[2]string{"user", "第一轮问题"},
		[2]string{"assistant", "第一轮回答"},
		[2]string{"user", "第二轮问题"},
		[2]string{"assistant", "第一轮回答"},
		[2]string{"user", "第三轮问题"},
	) {
		t.Errorf("恢复后模型输入应包含全部历史（两轮）与新消息，实际: %+v", seq2)
	}

	// 落库消息共 6 条（三轮问答），顺序即时间序。
	msgs, err := fx.st.ListChatMessages(sessID, 0, 0)
	if err != nil || len(msgs) != 6 {
		t.Fatalf("应有 6 条消息，实际: %v, %d", err, len(msgs))
	}
	if msgs[5].Message == nil || msgs[5].Message.Content != "恢复后回答" {
		t.Errorf("末条应为恢复后的助手回复，实际: %+v", msgs[5])
	}
}

// TestHistoryRejectsEmptyCancelledAssistant 覆盖用户在正文首帧前中断：运行
// 活动可以记录取消状态，但不得写入违反模型协议的空 Assistant 消息。
func TestHistoryRejectsEmptyCancelledAssistant(t *testing.T) {
	fx := newTestFixture(t)
	m := kernel.NewMockChatModel()
	m.SetResponse("第一轮回答")
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
		BuildModel: func(context.Context, llm.Provider, string) (kernel.Model, error) { return m, nil },
	})
	if _, err := svc.HandleMessage(context.Background(), "usr-1", "", "第一轮"); err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	sessions, _ := fx.st.ListChatSessionsByUser("usr-1")
	sessionID := sessions[0].ID
	if err := fx.st.AppendChatMessage(store.ChatMessage{
		ID: store.NewChatMessageID(), SessionID: sessionID,
		Message: schema.AssistantMessage("", nil), RunID: "run-cancelled",
		Metadata:  `{"status":"cancelled"}`,
		CreatedAt: time.Now().UTC(),
	}); err == nil {
		t.Fatal("空 Assistant 消息必须被存储边界拒绝")
	}
	if _, err := svc.HandleMessage(context.Background(), "usr-1", sessionID, "继续"); err != nil {
		t.Fatalf("中断后继续对话失败: %v", err)
	}
	inputs := m.CallInputs()
	if len(inputs) != 2 {
		t.Fatalf("模型应调用 2 次，实际: %d", len(inputs))
	}
	for _, msg := range inputs[1] {
		if msg.Role == schema.Assistant && strings.TrimSpace(msg.Content) == "" && len(msg.ToolCalls) == 0 {
			t.Fatalf("空 assistant 不得重放给模型: %+v", messageRoles(inputs[1]))
		}
	}
}

// TestCancelThenContinue 覆盖完整竞态：首轮已进入模型但尚无正文时停止，紧接着
// 在同一会话发送第二轮。第二轮模型输入不得包含 content/tool_calls 均为空的
// assistant 消息（OpenAI 兼容端点会对此返回 400）。
func TestCancelThenContinue(t *testing.T) {
	fx := newTestFixture(t)
	m := &cancelBlockingModel{MockChatModel: kernel.NewMockChatModel(), started: make(chan struct{})}
	m.SetResponse("续聊完成")
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
		BuildModel: func(context.Context, llm.Provider, string) (kernel.Model, error) { return m, nil },
	})

	firstDone := make(chan error, 1)
	go func() {
		_, err := svc.HandleMessage(context.Background(), "usr-1", "", "第一轮会被中断")
		firstDone <- err
	}()
	select {
	case <-m.started:
	case <-time.After(runtimeEventWait):
		t.Fatalf("首轮没有在 %s 内进入模型", runtimeEventWait)
	}
	sessions, err := fx.st.ListChatSessionsByUser("usr-1")
	if err != nil || len(sessions) != 1 {
		t.Fatalf("首轮应已创建会话: %v, %d", err, len(sessions))
	}
	if err := svc.CancelRun(context.Background(), "usr-1", sessions[0].ID); err != nil {
		t.Fatalf("CancelRun 失败: %v", err)
	}
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("用户取消应作为正常终态返回: %v", err)
		}
	case <-time.After(runtimeEventWait):
		t.Fatalf("首轮取消后未在 %s 内收尾", runtimeEventWait)
	}

	if _, err := svc.HandleMessage(context.Background(), "usr-1", sessions[0].ID, "继续执行"); err != nil {
		t.Fatalf("中断后继续对话失败: %v", err)
	}
	inputs := m.CallInputs()
	if len(inputs) != 1 {
		t.Fatalf("标准 mock 应收到续聊调用 1 次，实际: %d", len(inputs))
	}
	for _, msg := range inputs[0] {
		if msg.Role == schema.Assistant && strings.TrimSpace(msg.Content) == "" && len(msg.ToolCalls) == 0 {
			t.Fatalf("续聊历史含非法空 assistant: %+v", messageRoles(inputs[0]))
		}
	}
}

func TestPrecreatedSessionGetsAutomaticTitle(t *testing.T) {
	fx := newTestFixture(t)
	m := kernel.NewMockChatModel()
	m.SetResponse("已完成")
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
		BuildModel: func(context.Context, llm.Provider, string) (kernel.Model, error) { return m, nil },
	})
	now := time.Now().UTC()
	for _, sess := range []store.ChatSession{
		{ID: "cs-default", UserID: "usr-1", Title: "新会话", CreatedAt: now, UpdatedAt: now},
		{ID: "cs-custom", UserID: "usr-1", Title: "保留人工标题", CreatedAt: now, UpdatedAt: now},
	} {
		if err := fx.st.CreateChatSession(sess); err != nil {
			t.Fatalf("预创建会话失败: %v", err)
		}
	}
	if _, err := svc.HandleMessage(context.Background(), "usr-1", "cs-default", "  分析\n机器人抓取失败的原因  "); err != nil {
		t.Fatalf("默认标题会话执行失败: %v", err)
	}
	if _, err := svc.HandleMessage(context.Background(), "usr-1", "cs-custom", "不要覆盖标题"); err != nil {
		t.Fatalf("自定义标题会话执行失败: %v", err)
	}
	gotDefault, _ := fx.st.GetChatSession("cs-default")
	if gotDefault.Title != "分析 机器人抓取失败的原因" {
		t.Errorf("默认标题应按首轮输入生成，实际: %q", gotDefault.Title)
	}
	gotCustom, _ := fx.st.GetChatSession("cs-custom")
	if gotCustom.Title != "保留人工标题" {
		t.Errorf("人工标题不应被覆盖，实际: %q", gotCustom.Title)
	}
}

func TestGenericOpeningWaitsForMeaningfulAutomaticTitle(t *testing.T) {
	fx := newTestFixture(t)
	m := kernel.NewMockChatModel()
	m.SetResponse("好的")
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
		BuildModel: func(context.Context, llm.Provider, string) (kernel.Model, error) { return m, nil },
	})
	now := time.Now().UTC()
	if err := fx.st.CreateChatSession(store.ChatSession{ID: "cs-greeting", UserID: "usr-1",
		Title: "新会话", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("预创建会话失败: %v", err)
	}
	if _, err := svc.HandleMessage(context.Background(), "usr-1", "cs-greeting", "你好"); err != nil {
		t.Fatalf("问候轮失败: %v", err)
	}
	stillDefault, _ := fx.st.GetChatSession("cs-greeting")
	if stillDefault.Title != "新会话" {
		t.Fatalf("纯问候不应抢占会话标题: %q", stillDefault.Title)
	}
	if _, err := svc.HandleMessage(context.Background(), "usr-1", "cs-greeting", "诊断机械臂抓取失败"); err != nil {
		t.Fatalf("任务轮失败: %v", err)
	}
	renamed, _ := fx.st.GetChatSession("cs-greeting")
	if renamed.Title != "诊断机械臂抓取失败" {
		t.Fatalf("首个有效任务应生成标题: %q", renamed.Title)
	}
}

// TestHandleMessageErrors 验证入口校验：空消息、会话不存在、归属不符、运行失败。
func TestHandleMessageErrors(t *testing.T) {
	fx := newTestFixture(t)
	m := kernel.NewMockChatModel()
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
		BuildModel: func(context.Context, llm.Provider, string) (kernel.Model, error) { return m, nil },
	})

	if _, err := svc.HandleMessage(context.Background(), "usr-1", "", "   "); err == nil {
		t.Error("空消息应报错")
	}
	if _, err := svc.HandleMessage(context.Background(), "usr-1", "cs-x", "你好"); err == nil {
		t.Error("会话不存在应报错")
	}

	// 归属不符：usr-1 的会话 usr-2 不可见。
	if _, err := svc.HandleMessage(context.Background(), "usr-1", "", "你好"); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	sessions, _ := fx.st.ListChatSessionsByUser("usr-1")
	if _, err := svc.HandleMessage(context.Background(), "usr-2", sessions[0].ID, "越权"); err == nil {
		t.Error("归属不符应报错")
	}

	// 运行失败：run session 迁移到 failed，下行 message.done 携带 error。
	m.SetError(context.DeadlineExceeded)
	if _, err := svc.HandleMessage(context.Background(), "usr-1", sessions[0].ID, "会失败"); err == nil {
		t.Fatal("模型错误应返回")
	}
	runs, err := fx.st.ListChatMessages(sessions[0].ID, 0, 0)
	if err != nil {
		t.Fatalf("ListChatMessages 失败: %v", err)
	}
	// 用户消息已落库，失败时不应追加助手消息（末条应是本次用户消息）。
	if last := runs[len(runs)-1]; last.Message == nil ||
		last.Message.Role != schema.User || last.Message.Content != "会失败" {
		t.Errorf("运行失败不应落库助手消息，末条实际: %+v", last)
	}
	events := drainEvents(fx.events)
	last := events[len(events)-1].Payload.(ws.Envelope)
	donePayload, ok := last.Payload.(MessageDonePayload)
	if !ok || donePayload.Error == "" {
		t.Errorf("失败时应下行带 error 的 message.done，实际: %+v", last.Payload)
	}
	failedRuns, _, err := fx.st.ListRunSessions(store.RunFilter{
		ChatSessionID: sessions[0].ID,
	}, 10, 0)
	if err != nil {
		t.Fatalf("读取失败 Run 归档失败: %v", err)
	}
	if len(failedRuns) == 0 || failedRuns[0].Status != store.RunStatusFailed ||
		failedRuns[0].Error == "" {
		t.Errorf("失败 Run 应持久保存终态和错误: %+v", failedRuns)
	}
}
