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

package kernel

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/mcpregistry"
	tooldef "insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/log"
)

// namedTestTool 是可指名的计数测试工具（ToolSearch 用例的 pinned/动态集成员）。
type namedTestTool struct {
	// name 工具全名（test 命名空间）。
	name string

	// hits 执行计数指针（与调用方共享）。
	hits *int
}

// Def 返回该测试工具的契约。
func (t *namedTestTool) Def() tooldef.Definition {
	return tooldef.Definition{
		Name: t.name, Namespace: "test", Description: "测试工具 " + t.name,
		ParametersJSON: `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`,
		Annotations:    tooldef.Annotations{Risk: tooldef.RiskLow},
	}
}

// Run 执行：计数并回显。
func (t *namedTestTool) Run(_ context.Context, _ string) (string, error) {
	*t.hits++
	return tooldef.OKResult(map[string]any{"text": "ok"})
}

// adaptNamedTools 把一批测试工具注册、适配为内核工具（toolsearch 用例的
// pinned/DynamicTools 来源，与真实链路同一份适配逻辑）。
func adaptNamedTools(t *testing.T, tools ...tooldef.Tool) []Tool {
	t.Helper()
	reg := tooldef.NewRegistry()
	for _, tt := range tools {
		if err := reg.Register(tt); err != nil {
			t.Fatalf("注册工具失败: %v", err)
		}
	}
	executor := tooldef.NewExecutor(reg, tooldef.ExecutorOptions{
		Logger: log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
	})
	adapted, err := AdaptTools(reg.List(), executor)
	if err != nil {
		t.Fatalf("AdaptTools 失败: %v", err)
	}
	return adapted
}

// TestBuildToolSearchMiddlewareSkipped 验证挂接条件：开关关闭、或开关打开
// 但动态集为空时都不挂接（全量直通形态，与存量行为一致）。
func TestBuildToolSearchMiddlewareSkipped(t *testing.T) {
	echo := &namedTestTool{name: "test.echo", hits: new(int)}
	dynamic := adaptNamedTools(t, echo)

	// 开关关闭：即使有动态集也不挂接（动态集应经 Tools 全量注入）。
	mw, err := buildToolSearchMiddleware(context.Background(), AgentConfig{DynamicTools: dynamic})
	if err != nil || mw != nil {
		t.Errorf("开关关闭应跳过挂接，实际: mw=%v err=%v", mw, err)
	}

	// 开关打开但动态集为空：不挂接。
	mw, err = buildToolSearchMiddleware(context.Background(), AgentConfig{ToolSearch: true})
	if err != nil || mw != nil {
		t.Errorf("空动态集应跳过挂接，实际: mw=%v err=%v", mw, err)
	}
}

// TestToolSearchMiddlewareToolsInjection 验证挂接后的 tools 节点注入：
// tool_search 元工具与动态集都进入 runCtx.Tools（可执行面）。
func TestToolSearchMiddlewareToolsInjection(t *testing.T) {
	echo := &namedTestTool{name: "test.echo", hits: new(int)}
	calc := &namedTestTool{name: "test.calc", hits: new(int)}
	mw, err := buildToolSearchMiddleware(context.Background(), AgentConfig{
		ToolSearch:   true,
		DynamicTools: adaptNamedTools(t, echo, calc),
	})
	if err != nil {
		t.Fatalf("buildToolSearchMiddleware 失败: %v", err)
	}
	if mw == nil {
		t.Fatalf("开关打开且动态集非空时应挂接")
	}

	runCtx := &adk.ChatModelAgentContext{}
	_, runCtx, err = mw.BeforeAgent(context.Background(), runCtx)
	if err != nil {
		t.Fatalf("BeforeAgent 失败: %v", err)
	}
	names := make([]string, 0, len(runCtx.Tools))
	for _, tt := range runCtx.Tools {
		info, err := tt.Info(context.Background())
		if err != nil {
			t.Fatalf("工具 Info 失败: %v", err)
		}
		names = append(names, info.Name)
	}
	// 动态集顺序跟随 AdaptTools（注册表按名升序）：calc 在 echo 前。
	want := []string{toolSearchToolName, "test_calc", "test_echo"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("tools 节点应含 tool_search + 动态集，实际: %v", names)
	}
}

// collectRunToolEvents 收集运行事件流：返回累计文本、工具结果事件的工具名
// 序列（按出现序）与运行错误。
func collectRunToolEvents(t *testing.T, es *EventStream) (string, []string, error) {
	t.Helper()
	var sb strings.Builder
	var tools []string
	for {
		ev, ok := es.Next()
		if !ok {
			t.Fatal("事件流在未出现 EventDone 前关闭")
		}
		switch ev.Kind {
		case EventTextDelta:
			sb.WriteString(ev.Text)
		case EventToolResult:
			tools = append(tools, ev.ToolName)
		case EventError:
			return sb.String(), tools, ev.Err
		case EventDone:
			return sb.String(), tools, nil
		}
	}
}

// TestToolSearchFullChain 全链路验证 ToolSearch 语义（mock 模型按脚本驱动）：
//  1. pinned 工具直通：首轮不经检索直接调用 test.pinned（常驻可见）；
//  2. 检索：调用 tool_search 元工具（select:test_echo）；
//  3. 命中后调用：动态工具 test.echo 检索后出现并执行；
//  4. safety 共存：tool_search 经豁免集放行（门禁无感知），pinned/动态
//     业务工具照常过门禁且按契约原名（nameMap 覆盖 DynamicTools）；
//  5. 检索事件进运行事件流（EventToolResult 携带 tool_search）。
func TestToolSearchFullChain(t *testing.T) {
	pinnedHits := new(int)
	echoHits := new(int)
	pinned := &namedTestTool{name: "test.pinned", hits: pinnedHits}
	echo := &namedTestTool{name: "test.echo", hits: echoHits}

	guard := &recordingGuard{}
	m := NewMockChatModel()
	m.SetScript(
		MockReply{ToolCalls: []schema.ToolCall{{
			ID: "call-1", Type: "function",
			Function: schema.FunctionCall{Name: "test_pinned", Arguments: `{"text":"p"}`},
		}}},
		MockReply{ToolCalls: []schema.ToolCall{{
			ID: "call-2", Type: "function",
			Function: schema.FunctionCall{Name: toolSearchToolName, Arguments: `{"query":"select:test_echo"}`},
		}}},
		MockReply{ToolCalls: []schema.ToolCall{{
			ID: "call-3", Type: "function",
			Function: schema.FunctionCall{Name: "test_echo", Arguments: `{"text":"hi"}`},
		}}},
		MockReply{Content: "检索调用完成。"},
	)
	runner, err := BuildAgent(context.Background(), AgentConfig{
		Name:         "leader",
		Model:        m,
		MaxTurns:     6,
		Tools:        adaptNamedTools(t, pinned),
		ToolSearch:   true,
		DynamicTools: adaptNamedTools(t, echo),
		Safety:       guard,
	})
	if err != nil {
		t.Fatalf("BuildAgent 失败: %v", err)
	}
	es, err := runner.Run(context.Background(), nil, "开始")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	text, toolEvents, runErr := collectRunToolEvents(t, es)
	if runErr != nil {
		t.Fatalf("运行出错: %v", runErr)
	}
	if text != "检索调用完成。" {
		t.Errorf("最终文本不符: %q", text)
	}

	// pinned 直通与动态工具命中后调用都真实执行。
	if *pinnedHits != 1 {
		t.Errorf("pinned 工具应直通执行 1 次，实际: %d", *pinnedHits)
	}
	if *echoHits != 1 {
		t.Errorf("动态工具应检索后执行 1 次，实际: %d", *echoHits)
	}

	// 门禁记录：tool_search 经豁免集不记录；业务工具按契约原名记录
	// （nameMap 覆盖 Tools 与 DynamicTools——动态工具未被还原时会记成
	// 净化名 test_echo）。
	names := guard.calledNames()
	if len(names) != 2 || names[0] != "test.pinned" || names[1] != "test.echo" {
		t.Errorf("门禁应只判定两个业务工具（原名），实际: %v", names)
	}

	// 检索事件进运行事件流：tool_search 的工具结果事件出现。
	joined := strings.Join(toolEvents, ",")
	for _, want := range []string{"test_pinned", toolSearchToolName, "test_echo"} {
		if !strings.Contains(joined, want) {
			t.Errorf("工具结果事件序列应含 %q，实际: %v", want, toolEvents)
		}
	}

	// 首轮模型输入含 toolsearch 提醒消息（<available-deferred-tools> 清单）：
	// 只列动态集（test_echo），不含 pinned（常驻工具无需检索发现）。
	inputs := m.CallInputs()
	if len(inputs) != 4 {
		t.Fatalf("模型应被调用 4 次，实际: %d", len(inputs))
	}
	var reminder string
	for _, msg := range inputs[0] {
		if msg.Role == schema.User && strings.Contains(msg.Content, "<available-deferred-tools>") {
			reminder = msg.Content
		}
	}
	if reminder == "" {
		t.Fatalf("首轮输入应含 toolsearch 提醒消息，实际: %+v", inputs[0])
	}
	if !strings.Contains(reminder, "test_echo") {
		t.Errorf("提醒清单应含动态工具 test_echo，实际: %q", reminder)
	}
	if strings.Contains(reminder, "test_pinned") {
		t.Errorf("提醒清单不应含 pinned 工具（直通无需检索），实际: %q", reminder)
	}
}

// TestToolSearchDynamicMCPToolSafetyNameMapping 验证动态工具不依赖内置
// toolAdapter 具体类型：MCP 适配器经检索后可执行，门禁收到契约原名。
func TestToolSearchDynamicMCPToolSafetyNameMapping(t *testing.T) {
	caller := &fakeMCPCaller{out: `{"ok":true,"data":"echo: hi"}`}
	dynamic, err := AdaptMCPTools(
		[]mcpregistry.Entry{mcpTestEntry("test.echo", "test", "echo")},
		caller,
	)
	if err != nil {
		t.Fatalf("AdaptMCPTools 失败: %v", err)
	}

	guard := &recordingGuard{}
	m := NewMockChatModel()
	m.SetScript(
		MockReply{ToolCalls: []schema.ToolCall{{
			ID: "call-1", Type: "function",
			Function: schema.FunctionCall{Name: toolSearchToolName, Arguments: `{"query":"select:test_echo"}`},
		}}},
		MockReply{ToolCalls: []schema.ToolCall{{
			ID: "call-2", Type: "function",
			Function: schema.FunctionCall{Name: "test_echo", Arguments: `{"text":"hi"}`},
		}}},
		MockReply{Content: "完成。"},
	)
	runner, err := BuildAgent(context.Background(), AgentConfig{
		Name: "leader", Model: m, MaxTurns: 5,
		ToolSearch: true, DynamicTools: dynamic, Safety: guard,
	})
	if err != nil {
		t.Fatalf("BuildAgent 失败: %v", err)
	}
	events, err := runner.Run(context.Background(), nil, "开始")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	_, _, runErr := collectRunToolEvents(t, events)
	if runErr != nil {
		t.Fatalf("运行出错: %v", runErr)
	}
	if caller.server != "test" || caller.toolName != "echo" {
		t.Errorf("MCP 动态工具应真实执行，实际: server=%q tool=%q", caller.server, caller.toolName)
	}
	if names := guard.calledNames(); len(names) != 1 || names[0] != "test.echo" {
		t.Errorf("门禁应收到 MCP 契约原名 test.echo，实际: %v", names)
	}
}
