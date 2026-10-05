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

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// 观测补齐（R18）的集成测试：真实装配 + 脚本化 mock 模型跑一轮带审批的
// 对话（产生 trace/metering/interaction 数据），随后逐一验证三组只读
// REST 端点的契约。

// obsGet 发送带鉴权的 GET 并解析响应体。
func obsGet(t *testing.T, httpBase, token, path string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, httpBase+path, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s 失败: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("GET %s 响应不是合法 JSON: %v", path, err)
	}
	return resp.StatusCode, body
}

// TestObservabilityREST 全链路：对话（mock 调 artifact.put → 审批 → 批准 →
// 总结）产生观测数据后——traces 列表/span 树、metering summary 分组与任务
// 明细、interactions pending→answered 状态迁移（审批卡恢复路径）。
func TestObservabilityREST(t *testing.T) {
	t.Setenv("SEMANTIC_MOCK_SCRIPT", approvalScript(t, "报告已为您存好。"))
	dbPath := filepath.Join(t.TempDir(), "observability.db")
	httpBase, wsBase, _, stop := startChatApp(t, dbPath)
	defer stop()

	token := login(t, httpBase)
	sessionID, conn := createSessionAndDial(t, httpBase, wsBase, token)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	// ① 发消息触发审批中断：mock 第一轮调 artifact.put。
	sendChatMessage(t, conn, sessionID, "帮我把报告存起来")
	payload := awaitInteractionRequest(t, conn)

	// ② 中断期间：interactions?status=pending 应有 1 条（审批卡恢复数据源），
	// payload 为 JSON 对象、reply 为 null；answered 为 0。
	code, body := obsGet(t, httpBase, token,
		fmt.Sprintf("/api/v1/interactions?status=pending&session_id=%s", sessionID))
	if code != http.StatusOK || body["total"].(float64) != 1 {
		t.Fatalf("pending 交互应有 1 条，实际: %d %v", code, body)
	}
	card := body["interactions"].([]any)[0].(map[string]any)
	if card["id"] != payload.InteractionID || card["status"] != "pending" ||
		card["session_id"] != sessionID || card["type"] != "confirm" || card["run_id"] == "" {
		t.Errorf("pending 审批卡不符: %v", card)
	}
	if cardPayload, ok := card["payload"].(map[string]any); !ok || cardPayload["question"] == "" {
		t.Errorf("审批卡 payload 应为含 question 的对象，实际: %v", card["payload"])
	}
	if card["reply"] != nil || card["answered_at"] != nil {
		t.Errorf("未应答交互的 reply/answered_at 应为 null，实际: %v", card)
	}
	_, body = obsGet(t, httpBase, token, "/api/v1/interactions?status=answered")
	if body["total"].(float64) != 0 {
		t.Errorf("应答前 answered 应为 0，实际: %v", body)
	}

	// ③ 批准并跑完：工具执行 → 模型总结 → message.done。
	sendInteractionReply(t, conn, payload.InteractionID, true)
	done := awaitMessageDone(t, conn)
	if done.Error != "" {
		t.Fatalf("message.done 不应报错: %+v", done)
	}

	// ④ 应答后：answered 有 1 条（reply 对象 + answered_at），pending 归 0。
	_, body = obsGet(t, httpBase, token, "/api/v1/interactions?status=answered")
	if body["total"].(float64) != 1 {
		t.Fatalf("answered 交互应有 1 条，实际: %v", body)
	}
	answered := body["interactions"].([]any)[0].(map[string]any)
	if answered["id"] != payload.InteractionID || answered["answered_at"] == nil {
		t.Errorf("answered 条目不符: %v", answered)
	}
	if reply, ok := answered["reply"].(map[string]any); !ok || reply["approved"] != true {
		t.Errorf("应答负载应为 {\"approved\":true}，实际: %v", answered["reply"])
	}
	_, body = obsGet(t, httpBase, token, "/api/v1/interactions?status=pending")
	if body["total"].(float64) != 0 {
		t.Errorf("应答后 pending 应归 0，实际: %v", body)
	}

	// ⑤ traces 列表：流式回调在 goroutine 中消费落库，轮询等待 span 齐。
	var traces []any
	waitForCondition(t, 10*time.Second, func() bool {
		_, body = obsGet(t, httpBase, token, "/api/v1/traces")
		traces, _ = body["traces"].([]any)
		return body["total"].(float64) >= 1 && len(traces) >= 1 &&
			traces[0].(map[string]any)["span_count"].(float64) >= 2
	}, "traces 列表应有 span_count>=2 的链路")
	trace := traces[0].(map[string]any)
	traceID, _ := trace["trace_id"].(string)
	if traceID == "" || trace["name"] == "" || trace["kind"] == "" || trace["started_at"] == nil {
		t.Errorf("trace 条目字段不全: %v", trace)
	}

	// 分页 page_size=1 只返回一条链路。
	_, body = obsGet(t, httpBase, token, "/api/v1/traces?page_size=1")
	if len(body["traces"].([]any)) != 1 {
		t.Errorf("page_size=1 应只回 1 条，实际: %v", body)
	}

	// ⑥ span 树：按开始时间升序、含 ChatModel 跨度、attrs 为对象。
	code, body = obsGet(t, httpBase, token, "/api/v1/traces/"+traceID+"/spans")
	if code != http.StatusOK {
		t.Fatalf("spans 端点应返回 200，实际: %d %v", code, body)
	}
	spans := body["spans"].([]any)
	if len(spans) < 2 {
		t.Fatalf("链路应有 ≥2 条跨度，实际: %v", body)
	}
	hasModelSpan := false
	var prevStarted string
	for i, s := range spans {
		span := s.(map[string]any)
		if span["name"] == "" || span["started_at"] == nil {
			t.Errorf("第 %d 条跨度字段不全: %v", i, span)
		}
		if _, ok := span["attrs"].(map[string]any); !ok {
			t.Errorf("第 %d 条跨度 attrs 应为对象: %v", i, span["attrs"])
		}
		started, _ := span["started_at"].(string)
		if i > 0 && started < prevStarted {
			t.Errorf("跨度应按开始时间升序，第 %d 条回退: %v", i, spans)
		}
		prevStarted = started
		if kind, _ := span["kind"].(string); kind == "ChatModel" {
			hasModelSpan = true
		}
	}
	if !hasModelSpan {
		t.Errorf("链路应含 ChatModel 跨度，实际: %v", spans)
	}

	// ⑦ metering summary：mock 分组（agent=leader/purpose=chat）calls>=2
	// （审批前后两轮模型调用），token 合计为正。
	var rows []any
	waitForCondition(t, 10*time.Second, func() bool {
		_, body = obsGet(t, httpBase, token, "/api/v1/metering/summary?window=24h")
		rows, _ = body["rows"].([]any)
		for _, r := range rows {
			row := r.(map[string]any)
			if row["model"] == "mock" && row["agent"] == "leader" &&
				row["purpose"] == "chat" && row["calls"].(float64) >= 2 &&
				row["total_tokens"].(float64) > 0 {
				return true
			}
		}
		return false
	}, "summary 应有 mock/leader/chat 分组且 calls>=2")

	// ⑧ 任务计量明细：全部记录归属本 trace，与 summary 口径一致。
	_, body = obsGet(t, httpBase, token, "/api/v1/metering/traces/"+traceID)
	if body["total"].(float64) < 2 {
		t.Fatalf("任务计量明细应 ≥2 条，实际: %v", body)
	}
	for _, r := range body["records"].([]any) {
		rec := r.(map[string]any)
		if rec["trace_id"] != traceID || rec["model"] != "mock" || rec["purpose"] != "chat" ||
			rec["total_tokens"].(float64) <= 0 || rec["created_at"] == nil {
			t.Errorf("计量明细不符: %v", rec)
		}
	}

	// ⑨ 三组端点均在受保护组：无 token 401。
	for _, path := range []string{
		"/api/v1/traces", "/api/v1/traces/x/spans",
		"/api/v1/metering/summary", "/api/v1/metering/traces/x", "/api/v1/interactions",
	} {
		req, err := http.NewRequest(http.MethodGet, httpBase+path, nil)
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s 失败: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("无 token 访问 %s 应返回 401，实际: %d", path, resp.StatusCode)
		}
	}
}
