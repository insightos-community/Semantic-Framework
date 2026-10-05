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
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/log"
)

// weatherInput 是天气查询工具的入参。
type weatherInput struct {
	// City 城市名。
	City string `json:"city" jsonschema:"description=城市名"`
}

// collectRun 消费事件流直到结束，返回拼接文本、终止事件与首个错误。
func collectRun(t *testing.T, es *EventStream) (text string, done Event, runErr error) {
	t.Helper()
	var sb strings.Builder
	for {
		ev, ok := es.Next()
		if !ok {
			if runErr != nil {
				return sb.String(), done, runErr
			}
			t.Fatal("事件流在未出现 EventDone 或 EventError 前关闭")
		}
		switch ev.Kind {
		case EventTextDelta:
			sb.WriteString(ev.Text)
		case EventError:
			if runErr == nil {
				runErr = ev.Err
			}
		case EventDone:
			return sb.String(), ev, runErr
		}
	}
}

// TestEventStreamErrorIsTerminal 验证上游错误只产生 EventError，不能在流尾
// 再伪造 EventDone；失败 Run 的恢复期状态也必须释放。
func TestEventStreamErrorIsTerminal(t *testing.T) {
	iter, gen := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	var terminalCalls int32
	es := &EventStream{ch: make(chan Event, 4), terminalDone: func() {
		atomic.AddInt32(&terminalCalls, 1)
	}}
	wantErr := errors.New("upstream failed")
	go es.pump(context.Background(), iter)
	gen.Send(&adk.AgentEvent{Err: wantErr})
	gen.Close()

	var gotError, gotDone bool
	for ev := range es.ch {
		switch ev.Kind {
		case EventError:
			gotError = errors.Is(ev.Err, wantErr)
		case EventDone:
			gotDone = true
		}
	}
	if !gotError || gotDone {
		t.Fatalf("错误流终态不符: error=%v done=%v", gotError, gotDone)
	}
	if got := atomic.LoadInt32(&terminalCalls); got != 1 {
		t.Fatalf("失败终态应清理一次运行状态，实际: %d", got)
	}
}

// TestBuildAgentReturnDirectly 验证有持久副作用边界的工具完成后不再调用模型。
func TestBuildAgentReturnDirectly(t *testing.T) {
	m := NewMockChatModel()
	m.SetScript(
		MockReply{ToolCalls: []schema.ToolCall{{
			ID: "call-run", Type: "function",
			Function: schema.FunctionCall{Name: "robot_run", Arguments: `{"skill":"grasp-object"}`},
		}}},
		MockReply{Content: "这次展示性尾句不应触发第二次模型调用"},
	)
	var toolCalls int32
	runTool, err := utils.InferTool("robot_run", "运行Robot Skill",
		func(_ context.Context, _ map[string]any) (string, error) {
			atomic.AddInt32(&toolCalls, 1)
			return `{"accepted":true,"execution_id":"rex-1"}`, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := BuildAgent(context.Background(), AgentConfig{
		Name: "robot-worker", Model: m, Tools: []Tool{runTool}, MaxTurns: 5,
		ReturnDirectly: map[string]bool{"robot.run": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	es, err := runner.Run(context.Background(), nil, "执行当前SubTask")
	if err != nil {
		t.Fatal(err)
	}
	_, done, runErr := collectRun(t, es)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if got := len(m.CallInputs()); got != 1 {
		t.Fatalf("robot.run accepted后不得再调用模型，实际%d次", got)
	}
	if got := atomic.LoadInt32(&toolCalls); got != 1 {
		t.Fatalf("robot.run应执行一次，实际%d次", got)
	}
	if done.Turns != 1 {
		t.Fatalf("直接返回应只消耗一轮模型，实际%d", done.Turns)
	}
}

// TestBuildAgentSmoke agent 冒烟：mock 模型预设"先工具调用、后总结文本"，
// Runner 跑完一轮 ReAct 循环，断言工具被执行、delta 序列拼出最终文本、
// 轮次与用量正确、trace_spans 与 metering 均落库。
func TestBuildAgentSmoke(t *testing.T) {
	st := openKernelTestStore(t)
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})

	// 脚本：第一轮要求调 get_weather，第二轮返回总结文本。
	m := NewMockChatModel()
	m.SetScript(
		MockReply{ToolCalls: []schema.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: schema.FunctionCall{
				Name:      "get_weather",
				Arguments: `{"city":"北京"}`,
			},
		}}},
		MockReply{Content: "北京今天晴，25°C。"},
	)

	var toolCalls int32
	weatherTool, err := utils.InferTool("get_weather", "查询城市天气",
		func(_ context.Context, in weatherInput) (string, error) {
			atomic.AddInt32(&toolCalls, 1)
			return "晴，25°C", nil
		})
	if err != nil {
		t.Fatalf("构建工具失败: %v", err)
	}

	runner, err := BuildAgent(context.Background(), AgentConfig{
		Name:      "smoke-agent",
		Role:      "coordinator",
		Model:     m,
		ModelName: "mock",
		Tools:     []Tool{weatherTool},
		MaxTurns:  10,
		Store:     st,
		Purpose:   "chat",
		Logger:    logger,
	})
	if err != nil {
		t.Fatalf("BuildAgent 失败: %v", err)
	}

	es, err := runner.Run(context.Background(), nil, "北京天气怎么样？")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	finalText, done, runErr := collectRun(t, es)
	if runErr != nil {
		t.Fatalf("agent 运行出错: %v", runErr)
	}

	// ① 工具被执行且仅一次；② delta 拼出脚本总结文本；
	// ③ 模型共调用两轮（先工具调用、后总结），用量为两次之和。
	if got := atomic.LoadInt32(&toolCalls); got != 1 {
		t.Errorf("工具应被执行 1 次，实际: %d", got)
	}
	if finalText != "北京今天晴，25°C。" {
		t.Errorf("最终响应文本不符: %q", finalText)
	}
	if done.Turns != 2 {
		t.Errorf("运行轮次应为 2，实际: %d", done.Turns)
	}
	if done.Usage.TotalTokens != 60 {
		t.Errorf("两轮用量应累计 60 tokens，实际: %+v", done.Usage)
	}
	if got := m.GenerateCallCount() + m.StreamCallCount(); got != 2 {
		t.Errorf("模型应被调用 2 次，实际: %d", got)
	}

	// ④ trace/metering 落库（流式时机的处理器在 goroutine 中消费，
	// 以轮询等待落库完成，规避竞态而非引入固定睡眠）。
	traceID := es.TraceID()
	if traceID == "" {
		t.Fatal("挂接 Store 后 TraceID 不应为空")
	}
	waitFor(t, 5*time.Second, func() bool {
		spans, err := st.QuerySpans(traceID)
		if err != nil {
			t.Fatalf("QuerySpans 失败: %v", err)
		}
		return len(spans) >= 3
	})
	spans, err := st.QuerySpans(traceID)
	if err != nil {
		t.Fatalf("QuerySpans 失败: %v", err)
	}
	spanIDs := make(map[string]struct{}, len(spans))
	var roots, models, tools int
	for _, sp := range spans {
		spanIDs[strconv.FormatInt(sp.ID, 10)] = struct{}{}
		switch sp.Kind {
		case "Agent":
			if sp.ParentID == "" {
				roots++
			}
		case "ChatModel":
			models++
		case "Tool":
			tools++
		}
	}
	if roots != 1 || models != 2 || tools == 0 {
		t.Fatalf("Trace 应有一个 Agent 根跨度、两次模型跨度和工具跨度，实际: %+v", spans)
	}
	for _, sp := range spans {
		if sp.ParentID == "" {
			continue
		}
		if _, ok := spanIDs[sp.ParentID]; !ok {
			t.Errorf("跨度 %d 引用了不存在的父跨度 %q: %+v", sp.ID, sp.ParentID, sp)
		}
	}

	waitFor(t, 5*time.Second, func() bool {
		records, err := st.QueryMetering(store.MeteringFilter{Model: "mock"})
		if err != nil {
			t.Fatalf("QueryMetering 失败: %v", err)
		}
		return len(records) == 2
	})
	records, err := st.QueryMetering(store.MeteringFilter{Model: "mock"})
	if err != nil {
		t.Fatalf("QueryMetering 失败: %v", err)
	}
	totalTokens := 0
	for _, rec := range records {
		if rec.Agent != "smoke-agent" || rec.Role != "coordinator" || rec.Purpose != "chat" {
			t.Errorf("计量归因字段不符: %+v", rec)
		}
		totalTokens += rec.TotalTokens
	}
	if totalTokens != 60 {
		t.Errorf("两次调用应计量 60 tokens，实际: %d", totalTokens)
	}
}

// TestRunnerCreatesIndependentTracePerConcurrentRun 验证可缓存 Runner 被多个
// Run 并发复用时，每次执行仍有独立 trace_id、根 Span 和模型子 Span。
func TestRunnerCreatesIndependentTracePerConcurrentRun(t *testing.T) {
	st := openKernelTestStore(t)
	model := NewMockChatModel()
	model.SetResponse("ok")
	runner, err := BuildAgent(context.Background(), AgentConfig{
		Name: "concurrent-agent", Role: "coordinator", Model: model,
		ModelName: "mock", Store: st, Purpose: "chat",
		Logger: log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
	})
	if err != nil {
		t.Fatalf("BuildAgent 失败: %v", err)
	}

	const runs = 4
	type result struct {
		traceID string
		err     error
	}
	results := make(chan result, runs)
	var wg sync.WaitGroup
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			stream, runErr := runner.Run(context.Background(), nil, fmt.Sprintf("消息-%d", index))
			if runErr != nil {
				results <- result{err: runErr}
				return
			}
			for {
				event, ok := stream.Next()
				if !ok {
					results <- result{err: errors.New("事件流缺少终态")}
					return
				}
				switch event.Kind {
				case EventError:
					results <- result{err: event.Err}
					return
				case EventDone:
					results <- result{traceID: stream.TraceID()}
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(results)

	traceIDs := make(map[string]struct{}, runs)
	for got := range results {
		if got.err != nil {
			t.Fatalf("并发 Run 失败: %v", got.err)
		}
		if got.traceID == "" {
			t.Fatal("并发 Run 的 trace_id 不应为空")
		}
		if _, duplicate := traceIDs[got.traceID]; duplicate {
			t.Fatalf("不同 Run 复用了 trace_id: %s", got.traceID)
		}
		traceIDs[got.traceID] = struct{}{}
	}
	if len(traceIDs) != runs {
		t.Fatalf("应得到 %d 条独立 Trace，实际: %v", runs, traceIDs)
	}

	for traceID := range traceIDs {
		waitFor(t, 5*time.Second, func() bool {
			spans, queryErr := st.QuerySpans(traceID)
			return queryErr == nil && len(spans) >= 2
		})
		spans, queryErr := st.QuerySpans(traceID)
		if queryErr != nil {
			t.Fatalf("查询 Trace %s 失败: %v", traceID, queryErr)
		}
		if len(spans) != 2 {
			t.Fatalf("Trace %s 应只有根与一次模型调用，实际: %+v", traceID, spans)
		}
		if spans[0].Kind != "Agent" || spans[0].ParentID != "" ||
			spans[1].Kind != "ChatModel" || spans[1].ParentID != strconv.FormatInt(spans[0].ID, 10) {
			t.Fatalf("Trace %s 父子结构不符: %+v", traceID, spans)
		}
	}
}

// TestBuildAgentNoModel 验证缺少模型时构建报错。
func TestBuildAgentNoModel(t *testing.T) {
	if _, err := BuildAgent(context.Background(), AgentConfig{}); err == nil {
		t.Fatal("缺少模型应返回错误")
	}
}

// TestBuildAgentStoreWithoutLogger 验证 Store 非空但 Logger 缺失时构建报错。
func TestBuildAgentStoreWithoutLogger(t *testing.T) {
	st := openKernelTestStore(t)
	if _, err := BuildAgent(context.Background(), AgentConfig{
		Model: NewMockChatModel(),
		Store: st,
	}); err == nil {
		t.Fatal("Store 非空且 Logger 缺失应返回错误")
	}
}

// waitFor 以 20ms 间隔轮询 cond 直到为真或超时。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待条件在 %s 内未满足", timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
