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
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/store"
	tooldef "insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/log"
)

// 审批穿透（SubAgent 内的审批中断经 CompositeInterrupt 冒泡到根运行器）
// 的内核级验证：结构性正确性不依赖 WS/交互层，放在内核测试定位最快。

// buildPassthroughLeader 装配穿透测试的 leader 运行器：query-1（report.save
// 高危工具 + 审批门禁）包装为委派工具注入 leader 工具集；断点存储只挂在
// leader 一侧——SubAgent 的中断状态经 eino 桥存（bridge store）随复合中断
// 信号进父断点，SubAgent 自身不需要独立断点后端。
func buildPassthroughLeader(t *testing.T, st *store.Store, saveTool *reportSaveTool,
	leader, sub *MockChatModel) *Runner {

	t.Helper()
	reg := tooldef.NewRegistry()
	if err := reg.Register(saveTool); err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
	executor := tooldef.NewExecutor(reg, tooldef.ExecutorOptions{
		Logger: log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
	})
	subTools, err := AdaptTools(reg.List(), executor)
	if err != nil {
		t.Fatalf("AdaptTools 失败: %v", err)
	}

	subCfg := subAgentConfig(sub)
	subCfg.Tools = subTools
	subCfg.Safety = testApprovalGuard{}
	delegation := newDelegationTool(t, subCfg, testAgentToolDef())

	runner, err := BuildAgent(context.Background(), AgentConfig{
		Name:      "leader",
		Model:     leader,
		ModelName: "mock",
		Tools:     []tool.BaseTool{delegation},
		Store:     st,
		Logger:    log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
	})
	if err != nil {
		t.Fatalf("BuildAgent 失败: %v", err)
	}
	return runner
}

// passthroughScript 返回穿透场景的双方脚本：leader 轮 1 委派、轮 2 总结；
// query-1 轮 1 调 report_save（净化名），轮 2 按审批结论回报。
func passthroughScript(leaderSummary, subSummary string) (leader, sub *MockChatModel) {
	leader = NewMockChatModel()
	leader.SetScript(
		MockReply{ToolCalls: []schema.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: schema.FunctionCall{
				Name:      "ask_query",
				Arguments: `{"task":"把报告存起来"}`,
			},
		}}},
		MockReply{Content: leaderSummary},
	)
	sub = NewMockChatModel()
	sub.SetScript(
		MockReply{ToolCalls: []schema.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: schema.FunctionCall{
				Name:      "report_save",
				Arguments: `{"text":"季度报告"}`,
			},
		}}},
		MockReply{Content: subSummary},
	)
	return leader, sub
}

// collectPassthrough 消费事件流直到 EventDone，返回 leader 文本、
// SubAgent 结果事件与运行错误（中断后恢复流的通用收集器）。
func collectPassthrough(t *testing.T, es *EventStream) (leaderText string, subResult *Event, runErr error) {
	t.Helper()
	for {
		ev, ok := es.Next()
		if !ok {
			t.Fatal("事件流在未出现 EventDone 前关闭")
		}
		switch ev.Kind {
		case EventTextDelta:
			leaderText += ev.Text
		case EventSubAgentResult:
			r := ev
			subResult = &r
		case EventError:
			if runErr == nil {
				runErr = ev.Err
			}
		case EventDone:
			return leaderText, subResult, runErr
		}
	}
}

// TestSubAgentApprovalPassthroughApproved 验证批准路径的穿透链路：
// leader 委派 → query-1 内 report.save 触发审批中断 → 复合中断冒泡到根
// （根因负载是审批请求、断点随 run 持久化）→ 携带批准恢复 → 工具真实执行
// → SubAgent 结果冒泡 → leader 总结。
func TestSubAgentApprovalPassthroughApproved(t *testing.T) {
	st := openKernelTestStore(t)
	saveTool := &reportSaveTool{}
	leader, sub := passthroughScript("委派完成：报告已处理。", "报告已保存。")
	runner := buildPassthroughLeader(t, st, saveTool, leader, sub)

	runID := store.NewRunSessionID()
	createRunRow(t, st, runID)

	es, err := runner.RunWithCheckpoint(context.Background(), nil, "帮我把报告存起来", runID)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}

	// ① 中断冒泡到根：1 个根因中断点，负载是 query-1 内的审批请求；工具未执行。
	intr := collectUntilInterrupt(t, es)
	if len(intr.Interrupts) != 1 {
		t.Fatalf("应有 1 个根因中断点，实际: %+v", intr.Interrupts)
	}
	info, ok := intr.Interrupts[0].Info.(testApprovalInfo)
	if !ok {
		t.Fatalf("中断负载应为 testApprovalInfo，实际: %T", intr.Interrupts[0].Info)
	}
	if info.Tool != "report.save" {
		t.Errorf("中断负载应为 report.save 的审批请求，实际: %+v", info)
	}
	if got := atomic.LoadInt32(&saveTool.hits); got != 0 {
		t.Errorf("中断时工具不应执行，实际执行 %d 次", got)
	}

	// ② 复合断点已随 run 持久化（含 SubAgent 桥存字节）。
	if _, ok, err := st.GetRunCheckpoint(runID); err != nil || !ok {
		t.Fatalf("断点应已持久化: ok=%v, err=%v", ok, err)
	}

	// ③ 携带批准恢复：SubAgent 内工具执行 → 结果冒泡 → leader 总结。
	es2, err := runner.Resume(context.Background(), runID, map[string]any{intr.Interrupts[0].ID: true})
	if err != nil {
		t.Fatalf("Resume 失败: %v", err)
	}
	leaderText, subResult, runErr := collectPassthrough(t, es2)
	if runErr != nil {
		t.Fatalf("恢复后运行出错: %v", runErr)
	}
	if got := atomic.LoadInt32(&saveTool.hits); got != 1 {
		t.Errorf("批准后工具应执行 1 次，实际: %d", got)
	}
	if subResult == nil {
		t.Fatal("恢复后应收到 EventSubAgentResult")
	}
	if subResult.AgentID != "query-1" || subResult.Text != "报告已保存。" {
		t.Errorf("SubAgent 结果事件不符: %+v", subResult)
	}
	if subResult.Task != "把报告存起来" {
		t.Errorf("恢复流应延续委派调用的任务文本配对，实际: %q", subResult.Task)
	}
	if leaderText != "委派完成：报告已处理。" {
		t.Errorf("leader 总结文本不符: %q", leaderText)
	}
}

// TestSubAgentApprovalPassthroughRejected 验证拒绝路径：恢复携带拒绝 →
// SubAgent 内工具不执行（门禁返回结构化拒绝结果）→ SubAgent 如实回报 →
// leader 如实总结。
func TestSubAgentApprovalPassthroughRejected(t *testing.T) {
	st := openKernelTestStore(t)
	saveTool := &reportSaveTool{}
	leader, sub := passthroughScript("委派完成：报告未保存（未获批准）。", "报告未保存（未获批准）。")
	runner := buildPassthroughLeader(t, st, saveTool, leader, sub)

	runID := store.NewRunSessionID()
	createRunRow(t, st, runID)

	es, err := runner.RunWithCheckpoint(context.Background(), nil, "帮我把报告存起来", runID)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	intr := collectUntilInterrupt(t, es)
	if len(intr.Interrupts) != 1 {
		t.Fatalf("应有 1 个根因中断点，实际: %+v", intr.Interrupts)
	}

	es2, err := runner.Resume(context.Background(), runID, map[string]any{intr.Interrupts[0].ID: false})
	if err != nil {
		t.Fatalf("Resume 失败: %v", err)
	}
	leaderText, subResult, runErr := collectPassthrough(t, es2)
	if runErr != nil {
		t.Fatalf("恢复后运行出错: %v", runErr)
	}
	if got := atomic.LoadInt32(&saveTool.hits); got != 0 {
		t.Errorf("拒绝后工具不应执行，实际执行 %d 次", got)
	}
	if subResult == nil {
		t.Fatal("恢复后应收到 EventSubAgentResult")
	}
	if subResult.Text != "报告未保存（未获批准）。" {
		t.Errorf("SubAgent 应如实回报未执行，实际: %q", subResult.Text)
	}
	if leaderText != "委派完成：报告未保存（未获批准）。" {
		t.Errorf("leader 总结文本不符: %q", leaderText)
	}
}
