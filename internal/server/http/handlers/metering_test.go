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

package handlers

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/log"
)

// newMeteringTestRouter 装配计量端点的测试路由（真实 store）。
func newMeteringTestRouter(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st := storetest.OpenMigrated(t, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))

	h := NewMeteringHandler(st, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	r := chi.NewRouter()
	r.Get("/api/v1/metering/summary", h.HandleSummary)
	r.Get("/api/v1/metering/traces/{id}", h.HandleTraceMetering)
	return r, st
}

// seedMetering 写入三条计量记录：tr-1 两条（mock/leader/chat）、tr-2 一条
// （deepseek-chat/robot/task，2 小时前）。
func seedMetering(t *testing.T, st *store.Store) {
	t.Helper()
	base := time.Now().UTC().Truncate(time.Second)
	for _, m := range []store.Metering{
		{TraceID: "tr-1", Agent: "leader", Role: "coordinator", Model: "mock", Purpose: "chat",
			PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30, CostEstimate: 0.001,
			CreatedAt: base},
		{TraceID: "tr-1", Agent: "leader", Role: "coordinator", Model: "mock", Purpose: "chat",
			PromptTokens: 100, CompletionTokens: 200, TotalTokens: 300, CostEstimate: 0.01,
			CreatedAt: base.Add(time.Second)},
		{TraceID: "tr-2", Agent: "robot", Model: "deepseek-chat", Purpose: "task",
			PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3, CostEstimate: 0.0001,
			CreatedAt: base.Add(-2 * time.Hour)},
	} {
		if _, err := st.InsertMetering(m); err != nil {
			t.Fatalf("InsertMetering 失败: %v", err)
		}
	}
}

// TestMeteringSummaryEndpoint 验证聚合端点：分组维度、合计与 window 参数。
func TestMeteringSummaryEndpoint(t *testing.T) {
	router, st := newMeteringTestRouter(t)
	seedMetering(t, st)

	// 默认 24h 窗：两个分组，mock/leader/chat 合并合计排前。
	code, body := getJSON(t, router, "/api/v1/metering/summary")
	if code != http.StatusOK {
		t.Fatalf("应返回 200，实际: %d（%v）", code, body)
	}
	if body["window"] != "24h0m0s" || body["since"] == nil {
		t.Errorf("响应应含 window/since: %v", body)
	}
	rows := body["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("应有 2 个分组，实际: %v", body)
	}
	head := rows[0].(map[string]any)
	if head["model"] != "mock" || head["agent"] != "leader" || head["purpose"] != "chat" ||
		head["calls"].(float64) != 2 || head["prompt_tokens"].(float64) != 110 ||
		head["completion_tokens"].(float64) != 220 || head["total_tokens"].(float64) != 330 {
		t.Errorf("首分组聚合不符: %v", head)
	}
	if cost := head["cost_estimate"].(float64); cost < 0.0109 || cost > 0.0111 {
		t.Errorf("成本合计应约为 0.011，实际: %v", cost)
	}

	// window=1h：2 小时前的 tr-2 被排除，只剩 1 个分组。
	_, body = getJSON(t, router, "/api/v1/metering/summary?window=1h")
	rows = body["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["model"] != "mock" {
		t.Errorf("1h 窗口应只含 mock 分组，实际: %v", body)
	}

	// 非法 window：400 BAD_REQUEST。
	code, body = getJSON(t, router, "/api/v1/metering/summary?window=abc")
	if code != http.StatusBadRequest ||
		body["error"].(map[string]any)["code"] != CodeBadRequest {
		t.Errorf("非法 window 应返回 400，实际: %d %v", code, body)
	}
}

// TestTraceMeteringEndpoint 验证链路计量明细端点的列表与分页。
func TestTraceMeteringEndpoint(t *testing.T) {
	router, st := newMeteringTestRouter(t)
	seedMetering(t, st)

	code, body := getJSON(t, router, "/api/v1/metering/traces/tr-1")
	if code != http.StatusOK {
		t.Fatalf("应返回 200，实际: %d（%v）", code, body)
	}
	if body["total"].(float64) != 2 {
		t.Errorf("tr-1 应有 2 条明细，实际: %v", body)
	}
	records := body["records"].([]any)
	first := records[0].(map[string]any)
	if first["trace_id"] != "tr-1" || first["agent"] != "leader" || first["role"] != "coordinator" ||
		first["model"] != "mock" || first["purpose"] != "chat" ||
		first["prompt_tokens"].(float64) != 10 || first["total_tokens"].(float64) != 30 ||
		first["created_at"] == nil {
		t.Errorf("首条明细不符: %v", first)
	}

	// 分页：page_size=1 第 2 页。
	_, body = getJSON(t, router, "/api/v1/metering/traces/tr-1?page=2&page_size=1")
	records = body["records"].([]any)
	if len(records) != 1 || records[0].(map[string]any)["total_tokens"].(float64) != 300 {
		t.Errorf("分页明细不符: %v", body)
	}

	// 不存在的链路：空列表 total=0。
	_, body = getJSON(t, router, "/api/v1/metering/traces/tr-x")
	if body["total"].(float64) != 0 || len(body["records"].([]any)) != 0 {
		t.Errorf("不存在的链路应返回空，实际: %v", body)
	}
}
