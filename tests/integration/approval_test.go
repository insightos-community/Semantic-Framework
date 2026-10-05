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
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"

	"insightos.cn/semantic-framework/internal/agent/runtime"
	"insightos.cn/semantic-framework/internal/interaction"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
)

// 审批链路的集成测试：真实装配（bootstrap.Wire）+ 脚本化 mock 模型
// （SEMANTIC_MOCK_SCRIPT）驱动"模型调 artifact.put → 审批 → 恢复"全链路。

// approvalScript 生成审批场景的 mock 脚本：第一轮调 artifact.put，第二轮总结。
func approvalScript(t *testing.T, summary string) string {
	t.Helper()
	script, err := json.Marshal([]map[string]any{
		{"tool_calls": []map[string]string{{
			"id":        "call-1",
			"name":      "artifact_put",
			"arguments": `{"content":"# Q3 营收报告\n营收 100 万。","media_type":"text/markdown","summary":"Q3 营收报告"}`,
		}}},
		{"content": summary},
	})
	if err != nil {
		t.Fatalf("构造 mock 脚本失败: %v", err)
	}
	return string(script)
}

// awaitInteractionRequest 读取下行事件直到 interaction.request，返回解析后的负载。
func awaitInteractionRequest(t *testing.T, conn *websocket.Conn) interaction.RequestPayload {
	t.Helper()
	for {
		env := readEnvelope(t, conn)
		if env.Channel != ws.ChannelInteraction || env.Type != interaction.EventTypeInteractionRequest {
			continue
		}
		raw, err := json.Marshal(env.Payload)
		if err != nil {
			t.Fatalf("interaction.request 负载序列化失败: %v", err)
		}
		var payload interaction.RequestPayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("interaction.request 负载应为 RequestPayload: %v", err)
		}
		return payload
	}
}

// sendInteractionReply 发送一条 interaction.reply 上行消息。
func sendInteractionReply(t *testing.T, conn *websocket.Conn, interactionID string, approved bool) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"type": "interaction.reply", "interaction_id": interactionID, "approved": approved,
	})
	if err != nil {
		t.Fatalf("序列化应答失败: %v", err)
	}
	if err := conn.Write(context.Background(), websocket.MessageText, payload); err != nil {
		t.Fatalf("发送应答失败: %v", err)
	}
}

// awaitMessageDone 读取下行事件直到 message.done，返回解析后的负载。
func awaitMessageDone(t *testing.T, conn *websocket.Conn) runtime.MessageDonePayload {
	t.Helper()
	for {
		env := readEnvelope(t, conn)
		if env.Channel != ws.ChannelDialogue || env.Type != runtime.EventTypeMessageDone {
			continue
		}
		raw, err := json.Marshal(env.Payload)
		if err != nil {
			t.Fatalf("message.done 负载序列化失败: %v", err)
		}
		var payload runtime.MessageDonePayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("message.done 负载应为 MessageDonePayload: %v", err)
		}
		return payload
	}
}

// createSessionAndDial 建会话并建立 /ws/chat 连接。
func createSessionAndDial(t *testing.T, httpBase, wsBase, token string) (string, *websocket.Conn) {
	t.Helper()
	code, resp := chatPost(t, httpBase+"/api/v1/chat/sessions", token, `{}`)
	if code != http.StatusCreated {
		t.Fatalf("建会话应返回 201，实际: %d（%v）", code, resp)
	}
	sessionID, _ := resp["session"].(map[string]any)["id"].(string)
	if sessionID == "" {
		t.Fatalf("会话 ID 不应为空: %v", resp)
	}
	return sessionID, dialChat(t, wsBase, token, sessionID)
}

// TestApprovalApproved 批准链路：login → 建会话 → WS 发消息（mock 调
// artifact.put）→ 收到 interaction.request → 回复批准 → 工具执行 →
// message.done → 产物已写入、交互记录已应答。
func TestApprovalApproved(t *testing.T) {
	t.Setenv("SEMANTIC_MOCK_SCRIPT", approvalScript(t, "报告已为您存好。"))
	dbPath := filepath.Join(t.TempDir(), "approval.db")
	httpBase, wsBase, app, stop := startChatApp(t, dbPath)
	defer stop()

	token := login(t, httpBase)
	sessionID, conn := createSessionAndDial(t, httpBase, wsBase, token)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	// ① 发送消息：mock 第一轮调 artifact.put（risk=high → 审批中断）。
	sendChatMessage(t, conn, sessionID, "帮我把报告存起来")

	// ② 收到 interaction.request（confirm schema：interaction_id/question/risk/timeout_ts）。
	payload := awaitInteractionRequest(t, conn)
	if payload.InteractionID == "" || payload.Type != "confirm" ||
		payload.Question == "" || payload.Risk != "high" || payload.TimeoutTS == 0 {
		t.Fatalf("interaction.request 负载不符: %+v", payload)
	}

	// ③ 中断期间：run 应为 waiting_input，产物未写入。
	it, err := app.Store().GetInteraction(payload.InteractionID)
	if err != nil {
		t.Fatalf("GetInteraction 失败: %v", err)
	}
	run, err := app.Store().GetRunSession(it.RunID)
	if err != nil {
		t.Fatalf("GetRunSession 失败: %v", err)
	}
	if run.Status != store.RunStatusWaitingInput {
		t.Errorf("run 应为 waiting_input，实际: %q", run.Status)
	}
	if arts, _ := app.Store().ListArtifacts(0, 0); len(arts) != 0 {
		t.Errorf("审批前产物不应写入，实际: %d 条", len(arts))
	}

	// ④ 回复批准：工具执行 → 模型总结 → message.done。
	sendInteractionReply(t, conn, payload.InteractionID, true)
	done := awaitMessageDone(t, conn)
	if done.Error != "" || done.Text != "报告已为您存好。" {
		t.Errorf("message.done 不符: %+v", done)
	}

	// ⑤ 产物已写入（内容一致），交互记录已应答批准。
	arts, err := app.Store().ListArtifacts(0, 0)
	if err != nil || len(arts) != 1 {
		t.Fatalf("应有 1 条产物，实际: %v, %d", err, len(arts))
	}
	_, content, err := app.Store().GetArtifact(arts[0].ID)
	if err != nil {
		t.Fatalf("GetArtifact 失败: %v", err)
	}
	if string(content) != "# Q3 营收报告\n营收 100 万。" {
		t.Errorf("产物内容不符: %q", content)
	}
	it, _ = app.Store().GetInteraction(payload.InteractionID)
	if it.Status != store.InteractionStatusAnswered || it.Reply != `{"approved":true}` {
		t.Errorf("交互应为已应答批准，实际: %+v", it)
	}
	run, _ = app.Store().GetRunSession(it.RunID)
	if run.Status != store.RunStatusCompleted {
		t.Errorf("run 应迁移到 completed，实际: %q", run.Status)
	}
}

// TestApprovalRejected 拒绝链路：回复拒绝 → 工具不执行 →
// 模型如实告知未执行 → 无产物写入。
func TestApprovalRejected(t *testing.T) {
	t.Setenv("SEMANTIC_MOCK_SCRIPT", approvalScript(t, "抱歉，存储操作未获批准，报告没有保存。"))
	dbPath := filepath.Join(t.TempDir(), "approval.db")
	httpBase, wsBase, app, stop := startChatApp(t, dbPath)
	defer stop()

	token := login(t, httpBase)
	sessionID, conn := createSessionAndDial(t, httpBase, wsBase, token)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	sendChatMessage(t, conn, sessionID, "帮我把报告存起来")
	payload := awaitInteractionRequest(t, conn)

	// 回复拒绝：message.done 如实告知未执行，无产物。
	sendInteractionReply(t, conn, payload.InteractionID, false)
	done := awaitMessageDone(t, conn)
	if done.Error != "" || done.Text != "抱歉，存储操作未获批准，报告没有保存。" {
		t.Errorf("message.done 不符: %+v", done)
	}
	if arts, _ := app.Store().ListArtifacts(0, 0); len(arts) != 0 {
		t.Errorf("拒绝后不应有产物，实际: %d 条", len(arts))
	}
	it, _ := app.Store().GetInteraction(payload.InteractionID)
	if it.Status != store.InteractionStatusAnswered || it.Reply != `{"approved":false}` {
		t.Errorf("交互应为已应答拒绝，实际: %+v", it)
	}

	// 过期/已应答交互的重复应答应回 errorReply。
	sendInteractionReply(t, conn, payload.InteractionID, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("读取错误应答失败: %v", err)
	}
	var reply struct {
		Type string `json:"type"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		t.Fatalf("错误应答应为 JSON: %v（data: %s）", err, data)
	}
	if reply.Type != "error" || reply.Code != ws.CodeWSInteractionReplyFailed {
		t.Errorf("重复应答应回 INTERACTION_REPLY_FAILED，实际: %+v", reply)
	}
}
