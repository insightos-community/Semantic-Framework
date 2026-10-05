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

package store

import (
	"testing"
	"time"
)

// TestSpanInsertQuery 验证 trace 跨度的写入与按链路查询。
func TestSpanInsertQuery(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)

	for _, sp := range []Span{
		{TraceID: "tr-1", Name: "Mock", Kind: "ChatModel", StartedAt: now, DurationMs: 12, Attrs: `{"model":"mock"}`},
		{TraceID: "tr-1", Name: "get_weather", Kind: "Tool", StartedAt: now, DurationMs: 3, Attrs: `{}`},
		{TraceID: "tr-2", Name: "OpenAI", Kind: "ChatModel", StartedAt: now, DurationMs: 100, Attrs: `{}`},
	} {
		id, err := st.InsertSpan(sp)
		if err != nil {
			t.Fatalf("InsertSpan 失败: %v", err)
		}
		if id <= 0 {
			t.Errorf("InsertSpan 应返回正的自增 ID，实际: %d", id)
		}
	}

	spans, err := st.QuerySpans("tr-1")
	if err != nil {
		t.Fatalf("QuerySpans 失败: %v", err)
	}
	if len(spans) != 2 {
		t.Fatalf("tr-1 应有 2 条跨度，实际: %d", len(spans))
	}
	if spans[0].Name != "Mock" || spans[0].Kind != "ChatModel" || spans[0].DurationMs != 12 {
		t.Errorf("首条跨度字段不符: %+v", spans[0])
	}
	if spans[0].Attrs != `{"model":"mock"}` {
		t.Errorf("attrs 往返不一致: %q", spans[0].Attrs)
	}
	if !spans[0].StartedAt.Equal(now) {
		t.Errorf("started_at 往返不一致: 写入 %s，读出 %s", now, spans[0].StartedAt)
	}

	// 不存在的链路返回空列表而非错误。
	empty, err := st.QuerySpans("tr-x")
	if err != nil {
		t.Fatalf("QuerySpans(tr-x) 不应返回错误: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("不存在的链路应返回空列表，实际: %d 条", len(empty))
	}
}

// TestMeteringInsertQuery 验证计量记录的写入与多条件过滤查询。
func TestMeteringInsertQuery(t *testing.T) {
	st := openTestStore(t)
	base := time.Now().UTC().Truncate(time.Second)

	records := []Metering{
		{TraceID: "tr-1", Agent: "robot", Model: "deepseek-chat", Purpose: "chat",
			PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30, CostEstimate: 0.00005,
			CreatedAt: base.Add(-time.Hour)},
		{TraceID: "tr-2", Agent: "robot", Model: "deepseek-reasoner", Purpose: "task",
			PromptTokens: 100, CompletionTokens: 200, TotalTokens: 300, CostEstimate: 0.0036,
			CreatedAt: base},
		{TraceID: "tr-3", Agent: "leader", Model: "deepseek-chat", Purpose: "chat",
			PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3, CostEstimate: 0.000005,
			CreatedAt: base.Add(time.Hour)},
	}
	for _, m := range records {
		if _, err := st.InsertMetering(m); err != nil {
			t.Fatalf("InsertMetering 失败: %v", err)
		}
	}

	// 无过滤：全部返回。
	all, err := st.QueryMetering(MeteringFilter{})
	if err != nil {
		t.Fatalf("QueryMetering 失败: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("无过滤应返回 3 条，实际: %d", len(all))
	}
	if all[0].Model != "deepseek-chat" || all[0].TotalTokens != 30 || all[0].CostEstimate != 0.00005 {
		t.Errorf("首条记录字段不符: %+v", all[0])
	}

	// 按 model 过滤。
	byModel, err := st.QueryMetering(MeteringFilter{Model: "deepseek-chat"})
	if err != nil {
		t.Fatalf("QueryMetering(model) 失败: %v", err)
	}
	if len(byModel) != 2 {
		t.Errorf("按 model=deepseek-chat 应返回 2 条，实际: %d", len(byModel))
	}

	// 按 agent 过滤。
	byAgent, err := st.QueryMetering(MeteringFilter{Agent: "leader"})
	if err != nil {
		t.Fatalf("QueryMetering(agent) 失败: %v", err)
	}
	if len(byAgent) != 1 || byAgent[0].TraceID != "tr-3" {
		t.Errorf("按 agent=leader 应返回 tr-3 一条，实际: %+v", byAgent)
	}

	// 按时间范围过滤：[base-30m, base+30m] 只命中 tr-2。
	byTime, err := st.QueryMetering(MeteringFilter{
		Since: base.Add(-30 * time.Minute),
		Until: base.Add(30 * time.Minute),
	})
	if err != nil {
		t.Fatalf("QueryMetering(time) 失败: %v", err)
	}
	if len(byTime) != 1 || byTime[0].TraceID != "tr-2" {
		t.Errorf("时间范围过滤应只命中 tr-2，实际: %+v", byTime)
	}

	// 组合过滤 + Limit。
	combo, err := st.QueryMetering(MeteringFilter{Model: "deepseek-chat", Limit: 1})
	if err != nil {
		t.Fatalf("QueryMetering(combo) 失败: %v", err)
	}
	if len(combo) != 1 {
		t.Errorf("Limit=1 应只返回 1 条，实际: %d", len(combo))
	}
}

// TestMigrateV2Idempotent 验证 v2 迁移可重复执行（旧库升级路径幂等）。
func TestMigrateV2Idempotent(t *testing.T) {
	st := openTestStore(t)
	if err := st.Migrate(); err != nil {
		t.Errorf("重复 Migrate 应幂等成功，实际返回: %v", err)
	}
}

// TestListTraces 验证 trace 聚合视图：分组聚合、倒序分页、trace_id 过滤与总数。
func TestListTraces(t *testing.T) {
	st := openTestStore(t)
	base := time.Now().UTC().Truncate(time.Second)

	// tr-1：2 条跨度（模型 12ms + 工具 3ms）；tr-2：1 条跨度；tr-3：1 条跨度。
	for _, sp := range []Span{
		{TraceID: "tr-1", Name: "Mock", Kind: "ChatModel", StartedAt: base.Add(time.Second), DurationMs: 12, Attrs: `{"model":"mock"}`},
		{TraceID: "tr-1", Name: "get_weather", Kind: "Tool", StartedAt: base.Add(2 * time.Second), DurationMs: 3, Attrs: `{}`},
		{TraceID: "tr-2", Name: "OpenAI", Kind: "ChatModel", StartedAt: base.Add(3 * time.Second), DurationMs: 100, Attrs: `{}`},
		{TraceID: "tr-3", Name: "Mock", Kind: "ChatModel", StartedAt: base.Add(4 * time.Second), DurationMs: 8, Attrs: `{}`},
	} {
		if _, err := st.InsertSpan(sp); err != nil {
			t.Fatalf("InsertSpan 失败: %v", err)
		}
	}

	// 无过滤：按开始时间倒序 tr-3 → tr-2 → tr-1。
	all, total, err := st.ListTraces(TraceFilter{}, 10, 0)
	if err != nil {
		t.Fatalf("ListTraces 失败: %v", err)
	}
	if total != 3 || len(all) != 3 {
		t.Fatalf("应有 3 条链路（total=3），实际: total=%d len=%d", total, len(all))
	}
	if all[0].TraceID != "tr-3" || all[1].TraceID != "tr-2" || all[2].TraceID != "tr-1" {
		t.Errorf("链路应按开始时间倒序，实际: %+v", all)
	}

	// 聚合字段：name/kind 取最早开始的跨度，duration 为合计，count 为跨度数。
	tr1 := all[2]
	if tr1.Name != "Mock" || tr1.Kind != "ChatModel" || tr1.SpanCount != 2 || tr1.DurationMs != 15 {
		t.Errorf("tr-1 聚合不符: %+v", tr1)
	}
	if !tr1.StartedAt.Equal(base.Add(time.Second)) {
		t.Errorf("tr-1 started_at 应为最早跨度的开始时间，实际: %s", tr1.StartedAt)
	}

	// 分页：page_size=2 第 2 页只剩 tr-1。
	page2, total2, err := st.ListTraces(TraceFilter{}, 2, 2)
	if err != nil {
		t.Fatalf("ListTraces 分页失败: %v", err)
	}
	if total2 != 3 || len(page2) != 1 || page2[0].TraceID != "tr-1" {
		t.Errorf("分页结果不符: total=%d %+v", total2, page2)
	}

	// trace_id 精确过滤。
	filtered, total3, err := st.ListTraces(TraceFilter{TraceID: "tr-2"}, 10, 0)
	if err != nil {
		t.Fatalf("ListTraces 过滤失败: %v", err)
	}
	if total3 != 1 || len(filtered) != 1 || filtered[0].TraceID != "tr-2" || filtered[0].Name != "OpenAI" {
		t.Errorf("过滤结果不符: total=%d %+v", total3, filtered)
	}

	// 不存在的 trace_id：空列表，total=0。
	none, total4, err := st.ListTraces(TraceFilter{TraceID: "tr-x"}, 10, 0)
	if err != nil {
		t.Fatalf("ListTraces(tr-x) 不应报错: %v", err)
	}
	if total4 != 0 || len(none) != 0 {
		t.Errorf("不存在的链路应返回空，实际: total=%d len=%d", total4, len(none))
	}
}

// TestGetSpansOrdered 验证 GetSpans 按开始时间升序（同刻按写入序）返回。
func TestGetSpansOrdered(t *testing.T) {
	st := openTestStore(t)
	base := time.Now().UTC().Truncate(time.Second)

	// 写入序与开始时间序不一致：后写入的跨度开始更早（嵌套回调的真实形态）。
	for _, sp := range []Span{
		{TraceID: "tr-1", Name: "child", Kind: "ChatModel", StartedAt: base.Add(time.Second), DurationMs: 5, Attrs: `{}`},
		{TraceID: "tr-1", Name: "root", Kind: "ChatModelAgent", StartedAt: base, DurationMs: 9, Attrs: `{}`},
	} {
		if _, err := st.InsertSpan(sp); err != nil {
			t.Fatalf("InsertSpan 失败: %v", err)
		}
	}

	spans, err := st.GetSpans("tr-1")
	if err != nil {
		t.Fatalf("GetSpans 失败: %v", err)
	}
	if len(spans) != 2 || spans[0].Name != "root" || spans[1].Name != "child" {
		t.Fatalf("GetSpans 应按开始时间升序，实际: %+v", spans)
	}
}

// TestQueryMeteringTracePage 验证计量查询的 trace_id 过滤、分页偏移与计数。
func TestQueryMeteringTracePage(t *testing.T) {
	st := openTestStore(t)
	base := time.Now().UTC().Truncate(time.Second)

	for _, m := range []Metering{
		{TraceID: "tr-1", Agent: "leader", Model: "mock", Purpose: "chat",
			PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3, CreatedAt: base},
		{TraceID: "tr-1", Agent: "leader", Model: "mock", Purpose: "chat",
			PromptTokens: 4, CompletionTokens: 5, TotalTokens: 9, CreatedAt: base.Add(time.Second)},
		{TraceID: "tr-2", Agent: "robot", Model: "mock", Purpose: "task",
			PromptTokens: 7, CompletionTokens: 8, TotalTokens: 15, CreatedAt: base.Add(2 * time.Second)},
	} {
		if _, err := st.InsertMetering(m); err != nil {
			t.Fatalf("InsertMetering 失败: %v", err)
		}
	}

	// trace_id 过滤 + 计数。
	byTrace, err := st.QueryMetering(MeteringFilter{TraceID: "tr-1"})
	if err != nil {
		t.Fatalf("QueryMetering(trace_id) 失败: %v", err)
	}
	if len(byTrace) != 2 || byTrace[0].TotalTokens != 3 || byTrace[1].TotalTokens != 9 {
		t.Fatalf("tr-1 应有 2 条记录（按 ID 升序），实际: %+v", byTrace)
	}
	total, err := st.CountMetering(MeteringFilter{TraceID: "tr-1"})
	if err != nil {
		t.Fatalf("CountMetering 失败: %v", err)
	}
	if total != 2 {
		t.Errorf("tr-1 计数应为 2，实际: %d", total)
	}

	// 分页偏移：tr-1 第 2 页（limit=1 offset=1）只剩 9 tokens 那条。
	paged, err := st.QueryMetering(MeteringFilter{TraceID: "tr-1", Limit: 1, Offset: 1})
	if err != nil {
		t.Fatalf("QueryMetering(offset) 失败: %v", err)
	}
	if len(paged) != 1 || paged[0].TotalTokens != 9 {
		t.Errorf("偏移分页结果不符: %+v", paged)
	}
}

// TestSummarizeMetering 验证按 模型/Agent/用途 分组的聚合与时间窗过滤。
func TestSummarizeMetering(t *testing.T) {
	st := openTestStore(t)
	base := time.Now().UTC().Truncate(time.Second)

	for _, m := range []Metering{
		{TraceID: "tr-1", Agent: "leader", Model: "mock", Purpose: "chat",
			PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30, CostEstimate: 0.001,
			CreatedAt: base.Add(-time.Hour)},
		{TraceID: "tr-2", Agent: "leader", Model: "mock", Purpose: "chat",
			PromptTokens: 100, CompletionTokens: 200, TotalTokens: 300, CostEstimate: 0.01,
			CreatedAt: base},
		{TraceID: "tr-3", Agent: "robot", Model: "deepseek-chat", Purpose: "task",
			PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3, CostEstimate: 0.0001,
			CreatedAt: base},
	} {
		if _, err := st.InsertMetering(m); err != nil {
			t.Fatalf("InsertMetering 失败: %v", err)
		}
	}

	// 全量聚合：mock/leader/chat 两条合并，按总 token 降序排前。
	all, err := st.SummarizeMetering(time.Time{})
	if err != nil {
		t.Fatalf("SummarizeMetering 失败: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("应有 2 个分组，实际: %+v", all)
	}
	head := all[0]
	if head.Model != "mock" || head.Agent != "leader" || head.Purpose != "chat" ||
		head.Calls != 2 || head.PromptTokens != 110 || head.CompletionTokens != 220 ||
		head.TotalTokens != 330 {
		t.Errorf("首分组聚合不符: %+v", head)
	}
	if diff := head.CostEstimate - 0.011; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("成本合计应为 0.011，实际: %v", head.CostEstimate)
	}

	// 时间窗：since=base-30m 只含 base 时刻的两条（各自成组）。
	windowed, err := st.SummarizeMetering(base.Add(-30 * time.Minute))
	if err != nil {
		t.Fatalf("SummarizeMetering(since) 失败: %v", err)
	}
	if len(windowed) != 2 {
		t.Fatalf("窗口内应有 2 个分组，实际: %+v", windowed)
	}
	for _, ms := range windowed {
		if ms.Calls != 1 {
			t.Errorf("窗口内每组应各 1 次调用，实际: %+v", ms)
		}
	}
}
