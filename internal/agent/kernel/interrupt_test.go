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
	"encoding/gob"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/store"
	tooldef "insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/log"
)

// testApprovalInfo 是测试用的审批中断负载（gob 注册：中断负载会进 checkpoint）。
type testApprovalInfo struct {
	// Tool 待审批的工具名。
	Tool string

	// Question 给用户的问题。
	Question string
}

func init() {
	gob.Register(testApprovalInfo{})
}

// testApprovalGuard 是测试用的安全门禁：对 report.* 工具首次执行发起中断，
// 恢复时按注入的 bool 决定放行/拒绝（模拟安全管线的 L4 审批判断）。
type testApprovalGuard struct{}

// WrapToolCall 实现 ToolCallGuard：report.* 工具走审批中断，其余直接放行。
func (testApprovalGuard) WrapToolCall(ctx context.Context, meta ToolCallMeta, argsJSON string,
	next ToolCallEndpoint) (string, error) {
	if !strings.HasPrefix(meta.Name, "report.") {
		return next(ctx, argsJSON)
	}
	if !ToolCallWasInterrupted(ctx) {
		return "", InterruptToolCall(ctx, testApprovalInfo{Tool: meta.Name, Question: "是否批准保存报告？"})
	}
	isTarget, hasData, approved := ToolCallResumed[bool](ctx)
	if !isTarget || !hasData {
		return "", InterruptToolCall(ctx, nil)
	}
	if !approved {
		return tooldef.ErrorResult("APPROVAL_REJECTED", "用户未批准该操作", false), nil
	}
	return next(ctx, argsJSON)
}

// reportSaveTool 是测试用的高危工具（计数验证是否真执行）。
type reportSaveTool struct {
	// hits 执行次数。
	hits int32
}

// Def 返回 report.save 的契约。
func (t *reportSaveTool) Def() tooldef.Definition {
	return tooldef.Definition{
		Name: "report.save", Namespace: "report", Description: "保存报告",
		ParametersJSON: `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`,
		Annotations:    tooldef.Annotations{Risk: tooldef.RiskHigh},
	}
}

// Run 执行 report.save：计数并返回成功结果。
func (t *reportSaveTool) Run(_ context.Context, _ string) (string, error) {
	atomic.AddInt32(&t.hits, 1)
	return tooldef.OKResult(map[string]any{"saved": true})
}

// buildInterruptRunner 装配中断测试的运行器：mock 脚本（先调 report.save、
// 后总结）+ report.save 工具 + 审批门禁 + 断点存储。
func buildInterruptRunner(t *testing.T, st *store.Store, saveTool *reportSaveTool) *Runner {
	t.Helper()
	m := NewMockChatModel()
	m.SetScript(
		MockReply{ToolCalls: []schema.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: schema.FunctionCall{
				// 模型侧名为净化名（端点正则约束）；执行时经适配层还原为 report.save。
				Name:      "report_save",
				Arguments: `{"text":"季度报告"}`,
			},
		}}},
		MockReply{Content: "报告已处理完毕。"},
	)

	reg := tooldef.NewRegistry()
	if err := reg.Register(saveTool); err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
	executor := tooldef.NewExecutor(reg, tooldef.ExecutorOptions{
		Logger: log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
	})
	tools, err := AdaptTools(reg.List(), executor)
	if err != nil {
		t.Fatalf("AdaptTools 失败: %v", err)
	}

	runner, err := BuildAgent(context.Background(), AgentConfig{
		Name:      "interrupt-agent",
		Model:     m,
		ModelName: "mock",
		Tools:     tools,
		Safety:    testApprovalGuard{},
		Store:     st,
		Logger:    log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
	})
	if err != nil {
		t.Fatalf("BuildAgent 失败: %v", err)
	}
	return runner
}

// createRunRow 写入断点测试依赖的 run_sessions 行（断点随 run 存）。
func createRunRow(t *testing.T, st *store.Store, runID string) {
	t.Helper()
	if err := st.CreateRunSession(store.RunSession{
		ID: runID, AgentName: "interrupt-agent", ChatSessionID: "cs-1",
		Status: store.RunStatusRunning, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("创建 run session 失败: %v", err)
	}
}

// collectUntilInterrupt 消费事件流直到中断，返回中断事件。
func collectUntilInterrupt(t *testing.T, es *EventStream) Event {
	t.Helper()
	for {
		ev, ok := es.Next()
		if !ok {
			t.Fatal("事件流在未出现 EventInterrupted 前关闭")
		}
		if ev.Kind == EventError {
			t.Fatalf("运行出错: %v", ev.Err)
		}
		if ev.Kind == EventInterrupted {
			return ev
		}
	}
}

// TestInterruptResumeApproved 验证完整的中断→恢复链路（批准路径）：
// mock 调 report.save → 门禁中断 → 断点持久化 → 恢复携带批准 →
// 工具真实执行 → 模型总结 → EventDone。
func TestInterruptResumeApproved(t *testing.T) {
	st := openKernelTestStore(t)
	saveTool := &reportSaveTool{}
	runner := buildInterruptRunner(t, st, saveTool)

	runID := store.NewRunSessionID()
	createRunRow(t, st, runID)

	es, err := runner.RunWithCheckpoint(context.Background(), nil, "帮我把报告存起来", runID)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}

	// ① 中断事件：1 个根因中断点，负载是审批请求；工具未执行。
	intr := collectUntilInterrupt(t, es)
	if len(intr.Interrupts) != 1 {
		t.Fatalf("应有 1 个根因中断点，实际: %+v", intr.Interrupts)
	}
	info, ok := intr.Interrupts[0].Info.(testApprovalInfo)
	if !ok {
		t.Fatalf("中断负载应为 testApprovalInfo，实际: %T", intr.Interrupts[0].Info)
	}
	if info.Tool != "report.save" || info.Question == "" {
		t.Errorf("中断负载不符: %+v", info)
	}
	if got := atomic.LoadInt32(&saveTool.hits); got != 0 {
		t.Errorf("中断时工具不应执行，实际执行 %d 次", got)
	}
	if intr.Turns != 1 || intr.Usage.TotalTokens != 30 {
		t.Errorf("中断前应累计 1 轮 30 tokens，实际: turns=%d usage=%+v", intr.Turns, intr.Usage)
	}

	// ② 断点已持久化（先持久化断点再处理中断原因）。
	if _, ok, err := st.GetRunCheckpoint(runID); err != nil || !ok {
		t.Fatalf("断点应已持久化: ok=%v, err=%v", ok, err)
	}

	// ③ 携带批准恢复：工具执行 → 模型总结 → EventDone。
	es2, err := runner.Resume(context.Background(), runID, map[string]any{intr.Interrupts[0].ID: true})
	if err != nil {
		t.Fatalf("Resume 失败: %v", err)
	}
	text, done, runErr := collectRun(t, es2)
	if runErr != nil {
		t.Fatalf("恢复后运行出错: %v", runErr)
	}
	if got := atomic.LoadInt32(&saveTool.hits); got != 1 {
		t.Errorf("批准后工具应执行 1 次，实际: %d", got)
	}
	if text != "报告已处理完毕。" {
		t.Errorf("恢复后总结文本不符: %q", text)
	}
	if done.Turns != 1 {
		t.Errorf("恢复流应累计 1 轮（总结），实际: %d", done.Turns)
	}
}

// TestInterruptResumeRejected 验证拒绝路径：恢复携带拒绝 → 工具不执行，
// 模型收到结构化拒绝结果后如实总结。
func TestInterruptResumeRejected(t *testing.T) {
	st := openKernelTestStore(t)
	saveTool := &reportSaveTool{}

	// 第二轮脚本改为"如实告知未执行"（模拟模型读到拒绝结果后的回复）。
	m := NewMockChatModel()
	m.SetScript(
		MockReply{ToolCalls: []schema.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: schema.FunctionCall{
				// 模型侧名为净化名（端点正则约束）；执行时经适配层还原为 report.save。
				Name:      "report_save",
				Arguments: `{"text":"季度报告"}`,
			},
		}}},
		MockReply{Content: "抱歉，保存操作未获批准，报告未存储。"},
	)
	reg := tooldef.NewRegistry()
	if err := reg.Register(saveTool); err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
	executor := tooldef.NewExecutor(reg, tooldef.ExecutorOptions{
		Logger: log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
	})
	tools, err := AdaptTools(reg.List(), executor)
	if err != nil {
		t.Fatalf("AdaptTools 失败: %v", err)
	}
	runner, err := BuildAgent(context.Background(), AgentConfig{
		Name: "interrupt-agent", Model: m, ModelName: "mock",
		Tools:  tools,
		Safety: testApprovalGuard{},
		Store:  st,
		Logger: log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
	})
	if err != nil {
		t.Fatalf("BuildAgent 失败: %v", err)
	}

	runID := store.NewRunSessionID()
	createRunRow(t, st, runID)

	es, err := runner.RunWithCheckpoint(context.Background(), nil, "帮我把报告存起来", runID)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	intr := collectUntilInterrupt(t, es)

	es2, err := runner.Resume(context.Background(), runID, map[string]any{intr.Interrupts[0].ID: false})
	if err != nil {
		t.Fatalf("Resume 失败: %v", err)
	}
	text, _, runErr := collectRun(t, es2)
	if runErr != nil {
		t.Fatalf("恢复后运行出错: %v", runErr)
	}
	if got := atomic.LoadInt32(&saveTool.hits); got != 0 {
		t.Errorf("拒绝后工具不应执行，实际执行 %d 次", got)
	}
	if text != "抱歉，保存操作未获批准，报告未存储。" {
		t.Errorf("拒绝后总结文本不符: %q", text)
	}

	// 恢复后模型应读到结构化拒绝结果（第二轮输入含拒绝工具结果）。
	inputs := m.CallInputs()
	if len(inputs) != 2 {
		t.Fatalf("模型应被调用 2 次，实际: %d", len(inputs))
	}
	var sawRejection bool
	for _, msg := range inputs[1] {
		if msg.Role == schema.Tool && strings.Contains(msg.Content, "APPROVAL_REJECTED") {
			sawRejection = true
		}
	}
	if !sawRejection {
		t.Errorf("恢复后模型输入应包含 APPROVAL_REJECTED 工具结果，实际: %+v", messageRoles(inputs[1]))
	}
}

// messageRoles 提取消息 (role, content) 序列（测试断言辅助）。
func messageRoles(msgs []*schema.Message) [][2]string {
	out := make([][2]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, [2]string{string(m.Role), m.Content})
	}
	return out
}
