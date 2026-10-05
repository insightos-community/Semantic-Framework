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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/skill"
	tooldef "insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/log"
)

// newTestSkillStore 在临时目录写一个技能并构建真实的 skill.Store
// （backend 适配的映射正确性用真实 Store 验证，不建桩）。
func newTestSkillStore(t *testing.T, dir, name, description, body string) *skill.Store {
	t.Helper()
	skillDir := filepath.Join(dir, "general", name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("创建技能目录失败: %v", err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n" + body
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("写入 SKILL.md 失败: %v", err)
	}
	st, err := skill.NewStore(dir, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	if err != nil {
		t.Fatalf("NewStore 失败: %v", err)
	}
	return st
}

// TestSkillBackend 验证 backend 适配：List 映射标准字段（name/description），
// Get 返回正文与技能目录，未命中返回错误。
func TestSkillBackend(t *testing.T) {
	st := newTestSkillStore(t, t.TempDir(), "test-skill", "测试技能", "# 正文\n执行要点。")
	b := skillBackend{store: st}

	matters, err := b.List(context.Background())
	if err != nil || len(matters) != 1 {
		t.Fatalf("List 应返回 1 个技能，实际: %+v (err=%v)", matters, err)
	}
	if matters[0].Name != "test-skill" || matters[0].Description != "测试技能" {
		t.Errorf("List 映射不符: %+v", matters[0])
	}
	// context 为空 = inline 模式（正文作为工具结果注入，无 fork 子 agent）。
	if matters[0].Context != "" {
		t.Errorf("Context 应为空（inline），实际: %q", matters[0].Context)
	}

	got, err := b.Get(context.Background(), "test-skill")
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if !strings.Contains(got.Content, "执行要点") {
		t.Errorf("Content 应为 SKILL.md 正文，实际: %q", got.Content)
	}
	if !strings.HasSuffix(got.BaseDirectory, filepath.Join("general", "test-skill")) {
		t.Errorf("BaseDirectory 应为技能目录，实际: %q", got.BaseDirectory)
	}

	if _, err := b.Get(context.Background(), "not-exist"); err == nil {
		t.Errorf("未命中应返回错误")
	}
}

// TestSkillMiddlewareListing 验证渐进披露的常驻段：middleware 注入系统指引
// 与 skill 工具，工具描述含按 category 分组的清单摘要（- name: description (category)）。
func TestSkillMiddlewareListing(t *testing.T) {
	st := newTestSkillStore(t, t.TempDir(), "echo-guide", "查询指引", "# 正文")
	mw, err := buildSkillMiddleware(context.Background(), st, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	if err != nil {
		t.Fatalf("buildSkillMiddleware 失败: %v", err)
	}
	if mw == nil {
		t.Fatalf("有技能时应挂接 middleware")
	}

	runCtx := &adk.ChatModelAgentContext{}
	_, runCtx, err = mw.BeforeAgent(context.Background(), runCtx)
	if err != nil {
		t.Fatalf("BeforeAgent 失败: %v", err)
	}
	if !strings.Contains(runCtx.Instruction, "Skill 系统") {
		t.Errorf("系统指引应注入 Instruction，实际: %q", runCtx.Instruction)
	}
	if len(runCtx.Tools) != 1 {
		t.Fatalf("应注入 1 个 skill 工具，实际: %d", len(runCtx.Tools))
	}
	info, err := runCtx.Tools[0].Info(context.Background())
	if err != nil {
		t.Fatalf("工具 Info 失败: %v", err)
	}
	if info.Name != skillToolName {
		t.Errorf("工具名应为 %q，实际: %q", skillToolName, info.Name)
	}
	if !strings.Contains(info.Desc, "- echo-guide: 查询指引 (general)") ||
		!strings.Contains(info.Desc, "### general") {
		t.Errorf("工具描述应含分组清单摘要，实际: %q", info.Desc)
	}
}

// TestSkillMiddlewareSkipped 验证无技能形态：store 为 nil 或快照为空时不挂接。
func TestSkillMiddlewareSkipped(t *testing.T) {
	mw, err := buildSkillMiddleware(context.Background(), nil, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	if err != nil || mw != nil {
		t.Errorf("nil store 应跳过挂接，实际: mw=%v err=%v", mw, err)
	}

	empty, err := skill.NewStore(t.TempDir(), log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	if err != nil {
		t.Fatalf("NewStore 失败: %v", err)
	}
	mw, err = buildSkillMiddleware(context.Background(), empty, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	if err != nil || mw != nil {
		t.Errorf("空快照应跳过挂接，实际: mw=%v err=%v", mw, err)
	}
}

// TestSkillBodyInjection 端到端验证命中段的 inline 注入：mock 脚本先调
// skill 工具再总结——第二轮模型输入应含 skill 工具结果消息（SKILL.md 正文
// inline）；同时断言 S1 仍在消息头部（skill 指引经 Instruction 通道占据
// 头部 system 位时，context middleware 的幂等判定按内容识别，S1 不被挡住）。
func TestSkillBodyInjection(t *testing.T) {
	st := newTestSkillStore(t, t.TempDir(), "test-skill", "测试技能", "# 正文\nSKILL_BODY_MARKER 执行要点。")

	m := NewMockChatModel()
	m.SetScript(
		MockReply{ToolCalls: []schema.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: schema.FunctionCall{
				Name:      skillToolName,
				Arguments: `{"skill":"test-skill"}`,
			},
		}}},
		MockReply{Content: "已按技能指导完成。"},
	)
	runner, err := BuildAgent(context.Background(), AgentConfig{
		Name:        "leader",
		Instruction: "# Role\n你是 Leader。",
		Model:       m,
		MaxTurns:    5,
		SkillStore:  st,
	})
	if err != nil {
		t.Fatalf("BuildAgent 失败: %v", err)
	}
	es, err := runner.Run(context.Background(), nil, "帮我查一下")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	text, _, runErr := collectRun(t, es)
	if runErr != nil {
		t.Fatalf("运行出错: %v", runErr)
	}
	if text != "已按技能指导完成。" {
		t.Errorf("最终文本不符: %q", text)
	}

	inputs := m.CallInputs()
	if len(inputs) != 2 {
		t.Fatalf("模型应被调用 2 次，实际: %d", len(inputs))
	}

	// 第一轮：[S1, skill 指引, user]——S1 在最前（不被 skill 指引挡住）。
	first := inputs[0]
	if len(first) != 3 || first[0].Role != schema.System ||
		!strings.Contains(first[0].Content, "你是 Leader。") {
		t.Fatalf("首条应为 S1 系统提示，实际: %+v", first)
	}
	if first[1].Role != schema.System || !strings.Contains(first[1].Content, "Skill 系统") {
		t.Errorf("第二条应为 skill 使用指引，实际: %+v", first[1])
	}

	// 第二轮：含 skill 工具结果消息（正文 inline 注入，eino 结果格式带
	// base directory 行）。找到该消息断言正文标记在内。
	var toolMsg *schema.Message
	for _, msg := range inputs[1] {
		if msg.Role == schema.Tool {
			toolMsg = msg
		}
	}
	if toolMsg == nil {
		t.Fatalf("第二轮输入应含工具结果消息，实际: %+v", inputs[1])
	}
	if !strings.Contains(toolMsg.Content, "SKILL_BODY_MARKER") ||
		!strings.Contains(toolMsg.Content, "Base directory") {
		t.Errorf("工具结果应为技能正文 inline（含 base directory），实际: %q", toolMsg.Content)
	}
	// S1 幂等：第二轮仍只有一条 S1（不重复注入）。
	if inputs[1][0].Role != schema.System || !strings.Contains(inputs[1][0].Content, "你是 Leader。") {
		t.Errorf("第二轮首条仍应为 S1，实际: %+v", inputs[1][0])
	}
}

// recordingGuard 是记录调用名的安全门禁桩：验证 skill 工具经豁免集绕过
// 门禁、业务工具仍正常过门禁。
type recordingGuard struct {
	// mu 保护 names。
	mu sync.Mutex

	// names 记录每次门禁判定的工具名。
	names []string
}

// WrapToolCall 记录工具名并放行。
func (g *recordingGuard) WrapToolCall(ctx context.Context, meta ToolCallMeta, argsJSON string,
	next ToolCallEndpoint) (string, error) {
	g.mu.Lock()
	g.names = append(g.names, meta.Name)
	g.mu.Unlock()
	return next(ctx, argsJSON)
}

// calledNames 返回记录到的工具名副本。
func (g *recordingGuard) calledNames() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.names...)
}

// TestSkillToolBypassesSafety 验证 skill 加载工具与 safety 门禁的共存：
// skill 工具经豁免集直接放行（门禁无感知），注册表业务工具仍过门禁。
func TestSkillToolBypassesSafety(t *testing.T) {
	st := newTestSkillStore(t, t.TempDir(), "test-skill", "测试技能", "# 正文\nSKILL_BODY_MARKER")

	echoTool := &echoTestTool{}
	reg := tooldef.NewRegistry()
	if err := reg.Register(echoTool); err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
	executor := tooldef.NewExecutor(reg, tooldef.ExecutorOptions{
		Logger: log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
	})
	tools, err := AdaptTools(reg.List(), executor)
	if err != nil {
		t.Fatalf("AdaptTools 失败: %v", err)
	}

	guard := &recordingGuard{}
	m := NewMockChatModel()
	m.SetScript(
		MockReply{ToolCalls: []schema.ToolCall{{
			ID: "call-1", Type: "function",
			Function: schema.FunctionCall{Name: skillToolName, Arguments: `{"skill":"test-skill"}`},
		}}},
		MockReply{ToolCalls: []schema.ToolCall{{
			ID: "call-2", Type: "function",
			Function: schema.FunctionCall{Name: "test_echo", Arguments: `{"text":"hi"}`},
		}}},
		MockReply{Content: "完成。"},
	)
	runner, err := BuildAgent(context.Background(), AgentConfig{
		Name:       "leader",
		Model:      m,
		MaxTurns:   5,
		Tools:      tools,
		Safety:     guard,
		SkillStore: st,
	})
	if err != nil {
		t.Fatalf("BuildAgent 失败: %v", err)
	}
	es, err := runner.Run(context.Background(), nil, "开始")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	text, _, runErr := collectRun(t, es)
	if runErr != nil {
		t.Fatalf("运行出错: %v", runErr)
	}
	if text != "完成。" {
		t.Errorf("最终文本不符: %q", text)
	}
	if echoTool.hits != 1 {
		t.Errorf("业务工具应执行 1 次，实际: %d", echoTool.hits)
	}

	// 门禁记录应只有业务工具（test.echo），skill 加载工具经豁免集绕过。
	names := guard.calledNames()
	if len(names) != 1 || names[0] != "test.echo" {
		t.Errorf("门禁应只判定业务工具，实际: %v", names)
	}
}

// echoTestTool 是测试用的普通业务工具（经门禁的注册表工具形态）。
type echoTestTool struct {
	// hits 执行次数。
	hits int
}

// Def 返回 test.echo 的契约。
func (t *echoTestTool) Def() tooldef.Definition {
	return tooldef.Definition{
		Name: "test.echo", Namespace: "test", Description: "回显文本",
		ParametersJSON: `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`,
		Annotations:    tooldef.Annotations{Risk: tooldef.RiskLow},
	}
}

// Run 执行 test.echo：计数并回显。
func (t *echoTestTool) Run(_ context.Context, _ string) (string, error) {
	t.hits++
	return tooldef.OKResult(map[string]any{"text": "ok"})
}
