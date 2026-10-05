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
	"net/http"
	"strings"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/agent/runtime"
	"insightos.cn/semantic-framework/internal/store"
)

// 委派链路集成测试：真实装配（bootstrap.Wire + 真实 configs/agents Team）
// Team）+ 路由形态 mock 脚本（SEMANTIC_MOCK_SCRIPT 对象形态：leader 与
// query 各自的独立 mock 实例按系统提示身份词路由到各自剧本）驱动
// "leader 调 ask_query → query 回答 → 冒泡事件下行 → 独立 Trace → leader
// 总结" 全链路（架构文档 04 §7.3 agent-as-tool）。

// delegationScript 生成委派场景的路由脚本：leader 命中"面向用户的 Leader"
// （AGENT.md 身份标识）：轮 1 调 ask_query，轮 2 总结；query 命中
// "系统与产物查询助手"：直接回答清单。
func delegationScript(t *testing.T) string {
	t.Helper()
	taskArgs, err := json.Marshal(map[string]string{"task": "查 artifact 列表"})
	if err != nil {
		t.Fatalf("构造委派参数失败: %v", err)
	}
	script, err := json.Marshal(map[string]any{
		"scripts": []map[string]any{
			{
				"match": "面向用户的 Leader",
				"replies": []map[string]any{
					{"tool_calls": []map[string]string{{
						"id": "call-1", "name": "ask_query", "arguments": string(taskArgs),
					}}},
					{"content": "当前共有 3 个产物：report.pdf、log.txt、img.png。"},
				},
			},
			{
				"match":   "系统与产物查询助手",
				"replies": []map[string]any{{"content": "当前产物：report.pdf、log.txt、img.png"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("构造 mock 脚本失败: %v", err)
	}
	return string(script)
}

// TestSubAgentDelegation 验证 R12 委派全链路：
//  1. leader 第一轮发起 ask_query 委派（脚本驱动），query 执行并回答；
//  2. WS 下行收到归因 query-1 的 subagent.delta / subagent.result 事件；
//  3. Query 与 Leader 运行分别生成 Trace，Leader Run 精确关联会话；
//  4. leader 第二轮总结经 message.done 收尾。
func TestSubAgentDelegation(t *testing.T) {
	t.Setenv("SEMANTIC_MOCK_SCRIPT", delegationScript(t))
	httpBase, wsBase, app, stop := startTeamApp(t)
	defer stop()

	token := login(t, httpBase)
	code, resp := chatPost(t, httpBase+"/api/v1/chat/sessions", token, `{}`)
	if code != http.StatusCreated {
		t.Fatalf("建会话应返回 201，实际: %d（%v）", code, resp)
	}
	sessionID, _ := resp["session"].(map[string]any)["id"].(string)
	if sessionID == "" {
		t.Fatalf("会话 ID 不应为空: %v", resp)
	}

	conn := dialChat(t, wsBase, token, sessionID)
	defer func() { _ = conn.Close(1000, "done") }()
	sendChatMessage(t, conn, sessionID, "查一下有哪些产物")

	// 收集 dialogue 事件直到 message.done。
	events := waitDialogueDone(t, conn)

	var subDeltaText strings.Builder
	var subResult *runtime.SubAgentResultPayload
	var done *runtime.MessageDonePayload
	for _, env := range events {
		raw, err := json.Marshal(env.Payload)
		if err != nil {
			t.Fatalf("事件负载序列化失败: %v", err)
		}
		switch env.Type {
		case runtime.EventTypeSubAgentDelta:
			if env.Agent.ID != "query-1" || env.Agent.Role != "service" {
				t.Errorf("subagent.delta 归因不符: %+v", env.Agent)
			}
			var p runtime.SubAgentDeltaPayload
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatalf("subagent.delta 负载不符: %v（raw: %s）", err, raw)
			}
			subDeltaText.WriteString(p.Text)
		case runtime.EventTypeSubAgentResult:
			if env.Agent.ID != "query-1" || env.Agent.Role != "service" {
				t.Errorf("subagent.result 归因不符: %+v", env.Agent)
			}
			var p runtime.SubAgentResultPayload
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatalf("subagent.result 负载不符: %v（raw: %s）", err, raw)
			}
			subResult = &p
		case runtime.EventTypeMessageDone:
			if env.Agent.ID != "leader" {
				t.Errorf("message.done 应归因 leader，实际: %+v", env.Agent)
			}
			var p runtime.MessageDonePayload
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatalf("message.done 负载不符: %v（raw: %s）", err, raw)
			}
			done = &p
		}
	}

	// ① 冒泡事件：query-1 的增量与结果（任务/结果全文）。
	if subDeltaText.Len() == 0 {
		t.Error("未收到 query-1 的 subagent.delta 事件")
	}
	if subResult == nil {
		t.Fatal("未收到 query-1 的 subagent.result 事件")
	}
	if subResult.Task != "查 artifact 列表" ||
		subResult.Text != "当前产物：report.pdf、log.txt、img.png" {
		t.Errorf("subagent.result 负载不符: %+v", subResult)
	}

	// ② leader 总结输出，并且终态事件精确携带 Run/Trace。
	if done == nil || done.Text != "当前共有 3 个产物：report.pdf、log.txt、img.png。" {
		t.Fatalf("leader 总结文本不符: %+v", done)
	}
	run, err := app.Store().GetRunSession(done.RunID)
	if err != nil || run.ChatSessionID != sessionID || run.Status != store.RunStatusCompleted ||
		run.TraceID != done.TraceID {
		t.Fatalf("Leader Run 归属或终态不符: run=%+v err=%v", run, err)
	}
	if done.Model == nil || run.Provider != done.Model.Provider ||
		run.Endpoint != done.Model.ResolvedEndpoint || run.Model != done.Model.ResolvedModel {
		t.Fatalf("Leader Run 模型归档不符: run=%+v model=%+v", run, done.Model)
	}

	// ③ Query 是本次 Leader Run Trace 下的 Agent 子跨度，模型/工具跨度
	// 继续挂在它下面；不会形成无法关联到 Run 的孤立 Trace。
	var spans []store.Span
	waitForCondition(t, 5*time.Second, func() bool {
		var queryFound bool
		spans, err = app.Store().QuerySpans(done.TraceID)
		if err != nil {
			return false
		}
		for _, span := range spans {
			if span.Kind == "Agent" && span.Name == "query-1" && span.ParentID != "" {
				queryFound = true
			}
		}
		return queryFound
	}, "Query 应作为 Leader Run Trace 的 Agent 子跨度")
}
