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

// TestSummarizeSpanKinds 验证按跨度类型的条数/耗时聚合与降序排列。
func TestSummarizeSpanKinds(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)

	for _, sp := range []Span{
		{TraceID: "tr-1", Name: "Mock", Kind: "ChatModel", StartedAt: now, DurationMs: 120, Attrs: `{}`},
		{TraceID: "tr-1", Name: "Mock", Kind: "ChatModel", StartedAt: now, DurationMs: 80, Attrs: `{}`},
		{TraceID: "tr-1", Name: "artifact_put", Kind: "Tool", StartedAt: now, DurationMs: 30, Attrs: `{}`},
		{TraceID: "tr-2", Name: "chain", Kind: "Chain", StartedAt: now, DurationMs: 260, Attrs: `{}`},
	} {
		if _, err := st.InsertSpan(sp); err != nil {
			t.Fatalf("InsertSpan 失败: %v", err)
		}
	}

	stats, err := st.SummarizeSpanKinds()
	if err != nil {
		t.Fatalf("SummarizeSpanKinds 失败: %v", err)
	}
	if len(stats) != 3 {
		t.Fatalf("应有 3 个类型分组，实际: %+v", stats)
	}
	// 按耗时降序：Chain(260) > ChatModel(200) > Tool(30)。
	if stats[0].Kind != "Chain" || stats[0].Spans != 1 || stats[0].DurationMs != 260 {
		t.Errorf("首行应为 Chain 聚合，实际: %+v", stats[0])
	}
	if stats[1].Kind != "ChatModel" || stats[1].Spans != 2 || stats[1].DurationMs != 200 {
		t.Errorf("次行应为 ChatModel 聚合（合计耗时），实际: %+v", stats[1])
	}
	if stats[2].Kind != "Tool" || stats[2].Spans != 1 || stats[2].DurationMs != 30 {
		t.Errorf("末行应为 Tool 聚合，实际: %+v", stats[2])
	}
}

// TestPerfDataWindow 验证数据窗口：时间边界、链路/跨度/计量/run 规模，
// 以及空库零值形态。
func TestPerfDataWindow(t *testing.T) {
	st := openTestStore(t)

	// 空库：全部零值，不报错。
	w, err := st.PerfDataWindow()
	if err != nil {
		t.Fatalf("空库 PerfDataWindow 不应报错: %v", err)
	}
	if w.Traces != 0 || w.Spans != 0 || w.MeteringCalls != 0 || w.Runs != 0 ||
		!w.TraceEarliest.IsZero() || !w.TraceLatest.IsZero() ||
		!w.MeteringEarliest.IsZero() || !w.MeteringLatest.IsZero() {
		t.Errorf("空库窗口应为全零值，实际: %+v", w)
	}

	base := time.Now().UTC().Truncate(time.Second)
	for _, sp := range []Span{
		{TraceID: "tr-1", Name: "Mock", Kind: "ChatModel", StartedAt: base.Add(-time.Hour), DurationMs: 10, Attrs: `{}`},
		{TraceID: "tr-1", Name: "tool", Kind: "Tool", StartedAt: base, DurationMs: 5, Attrs: `{}`},
		{TraceID: "tr-2", Name: "Mock", Kind: "ChatModel", StartedAt: base.Add(time.Hour), DurationMs: 20, Attrs: `{}`},
	} {
		if _, err := st.InsertSpan(sp); err != nil {
			t.Fatalf("InsertSpan 失败: %v", err)
		}
	}
	for _, m := range []Metering{
		{TraceID: "tr-1", Agent: "leader", Model: "mock", Purpose: "chat",
			PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30, CreatedAt: base.Add(-30 * time.Minute)},
		{TraceID: "tr-2", Agent: "query-1", Model: "mock", Purpose: "query",
			PromptTokens: 5, CompletionTokens: 5, TotalTokens: 10, CreatedAt: base.Add(30 * time.Minute)},
	} {
		if _, err := st.InsertMetering(m); err != nil {
			t.Fatalf("InsertMetering 失败: %v", err)
		}
	}
	if err := st.CreateRunSession(RunSession{
		ID: "run-1", AgentName: "leader", ChatSessionID: "cs-1",
		Status: RunStatusCompleted, StartedAt: base,
	}); err != nil {
		t.Fatalf("CreateRunSession 失败: %v", err)
	}

	w, err = st.PerfDataWindow()
	if err != nil {
		t.Fatalf("PerfDataWindow 失败: %v", err)
	}
	if w.Traces != 2 || w.Spans != 3 || w.MeteringCalls != 2 || w.Runs != 1 {
		t.Errorf("窗口规模不符: %+v", w)
	}
	if !w.TraceEarliest.Equal(base.Add(-time.Hour)) || !w.TraceLatest.Equal(base.Add(time.Hour)) {
		t.Errorf("链路时间边界不符: %+v", w)
	}
	if !w.MeteringEarliest.Equal(base.Add(-30*time.Minute)) || !w.MeteringLatest.Equal(base.Add(30*time.Minute)) {
		t.Errorf("计量时间边界不符: %+v", w)
	}
}
