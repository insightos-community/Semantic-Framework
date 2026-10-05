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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"

	"insightos.cn/semantic-framework/internal/agent/runtime"
	"insightos.cn/semantic-framework/internal/bootstrap"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/log"
)

// startChatApp 以指定数据库路径启动装配后的服务（mock 模型 + 真实 leader
// profile），用于"重启恢复"场景：同一 dbPath 两次启动即模拟进程重启。
func startChatApp(t *testing.T, dbPath string) (httpBase, wsBase string, app *bootstrap.App, stop func()) {
	t.Helper()
	t.Setenv("SEMANTIC_ADMIN_PASSWORD", "test-admin-pass")

	profilesDir, err := filepath.Abs(filepath.Join("..", "..", "configs", "agents"))
	if err != nil {
		t.Fatalf("解析 profile 目录失败: %v", err)
	}
	cfg := config.Default()
	cfg.Server.HTTPAddr = freeAddr(t)
	cfg.Server.WSAddr = freeAddr(t)
	cfg.Store.SQLitePath = dbPath
	storetest.SeedMigratedAt(t, cfg.Store.SQLitePath)
	cfg.LLM.Default = "mock" // 无 key 环境：默认模型走 mock 驱动
	cfg.Agents.ProfilesDir = profilesDir
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})

	app, err = bootstrap.Wire(cfg, logger)
	if err != nil {
		t.Fatalf("Wire 装配失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- app.Run(ctx)
	}()

	httpBase = fmt.Sprintf("http://%s", cfg.Server.HTTPAddr)
	wsBase = fmt.Sprintf("ws://%s", cfg.Server.WSAddr)
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(httpBase + "/api/v1/system/healthz")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("服务在 30s 内未就绪，最后一次错误: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	stop = func() {
		cancel()
		select {
		case err := <-runDone:
			if err != nil {
				t.Errorf("App.Run 应随 ctx 取消正常退出，实际返回: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Errorf("App.Run 未在 15s 内退出")
		}
	}
	return httpBase, wsBase, app, stop
}

// chatPost 发送带鉴权的 JSON POST 并解析响应。
func chatPost(t *testing.T, url, token, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s 失败: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var parsed map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	return resp.StatusCode, parsed
}

// chatGetMessages 查询会话消息（带鉴权），返回 messages 数组。
func chatGetMessages(t *testing.T, httpBase, token, sessionID, query string) []map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		httpBase+"/api/v1/chat/sessions/"+sessionID+"/messages"+query, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("查询消息失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查询消息应返回 200，实际: %d", resp.StatusCode)
	}
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析消息响应失败: %v", err)
	}
	return body.Messages
}

// dialChat 建立 /ws/chat 连接并订阅会话。
func dialChat(t *testing.T, wsBase, token, sessionID string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(context.Background(),
		fmt.Sprintf("%s/ws/chat?token=%s&session_id=%s", wsBase, token, sessionID), nil)
	if err != nil {
		t.Fatalf("WS 连接失败: %v", err)
	}
	return conn
}

// readEnvelope 读取一条下行消息并解析为 envelope（带超时）。
func readEnvelope(t *testing.T, conn *websocket.Conn) ws.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("读取下行消息失败: %v", err)
	}
	var env ws.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("下行消息应为合法 envelope JSON: %v（data: %s）", err, data)
	}
	return env
}

// sendChatMessage 发送一条 chat.message 上行消息。
func sendChatMessage(t *testing.T, conn *websocket.Conn, sessionID, text string) {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"type": "chat.message", "session_id": sessionID, "text": text,
	})
	if err != nil {
		t.Fatalf("序列化上行消息失败: %v", err)
	}
	if err := conn.Write(context.Background(), websocket.MessageText, payload); err != nil {
		t.Fatalf("发送上行消息失败: %v", err)
	}
}

// sendPlanChatMessage 从同一 Conversation 显式选择 Plan Mode。
// 模式只约束这一轮 Leader 的提示词和工具，不创建第二套会话。
func sendPlanChatMessage(t *testing.T, conn *websocket.Conn, sessionID, text string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"type": "chat.message", "session_id": sessionID, "text": text,
		"send_scope": map[string]string{"type": "conversation", "intent": "plan"},
	})
	if err != nil {
		t.Fatalf("序列化 Plan 消息失败: %v", err)
	}
	if err := conn.Write(context.Background(), websocket.MessageText, payload); err != nil {
		t.Fatalf("发送 Plan 消息失败: %v", err)
	}
}

// waitDialogueDone 读取下行事件直到 message.done，返回全部 dialogue 事件。
func waitDialogueDone(t *testing.T, conn *websocket.Conn) []ws.Envelope {
	t.Helper()
	var events []ws.Envelope
	for {
		env := readEnvelope(t, conn)
		if env.Channel != ws.ChannelDialogue {
			continue
		}
		events = append(events, env)
		if env.Type == runtime.EventTypeMessageDone {
			return events
		}
	}
}

// TestChatDialogueLoop 对话闭环全链路：login → 建会话 → WS 发消息 →
// delta+done 下行 → REST 验证消息落库 → 重启 App 后再发一条验证历史延续。
func TestChatDialogueLoop(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "chat.db")
	httpBase, wsBase, _, stop := startChatApp(t, dbPath)

	token := login(t, httpBase)

	// ① 建会话（默认标题）。
	code, resp := chatPost(t, httpBase+"/api/v1/chat/sessions", token, `{}`)
	if code != http.StatusCreated {
		t.Fatalf("建会话应返回 201，实际: %d（%v）", code, resp)
	}
	sessionID, _ := resp["session"].(map[string]any)["id"].(string)
	if sessionID == "" {
		t.Fatalf("会话 ID 不应为空: %v", resp)
	}

	// ② WS 连接 /ws/chat 并发送消息。
	conn := dialChat(t, wsBase, token, sessionID)
	sendChatMessage(t, conn, sessionID, "你好，领导")

	// ③ 收到 started + delta + completed + done，并验证消息事件关联同一 Run。
	events := waitDialogueDone(t, conn)
	if len(events) != 4 {
		t.Fatalf("应收到 4 个 dialogue 事件，实际: %d（%+v）", len(events), events)
	}
	if events[0].Type != runtime.EventTypeRunStarted ||
		events[2].Type != runtime.EventTypeRunCompleted {
		t.Fatalf("Run 状态事件顺序不符: %+v", events)
	}
	deltaPayload, err := json.Marshal(events[1].Payload)
	if err != nil {
		t.Fatalf("delta 负载序列化失败: %v", err)
	}
	var delta runtime.MessageDeltaPayload
	if err := json.Unmarshal(deltaPayload, &delta); err != nil {
		t.Fatalf("delta 负载应为 MessageDeltaPayload: %v", err)
	}
	if events[1].Type != runtime.EventTypeMessageDelta || delta.RunID == "" || delta.Text == "" {
		t.Errorf("delta 事件不符: type=%q payload=%+v", events[1].Type, delta)
	}
	if events[1].Agent.ID != "leader" || events[1].Agent.Role != "coordinator" {
		t.Errorf("agent 归因应为 leader/coordinator，实际: %+v", events[1].Agent)
	}
	donePayload, err := json.Marshal(events[3].Payload)
	if err != nil {
		t.Fatalf("done 负载序列化失败: %v", err)
	}
	var done runtime.MessageDonePayload
	if err := json.Unmarshal(donePayload, &done); err != nil {
		t.Fatalf("done 负载应为 MessageDonePayload: %v", err)
	}
	if done.RunID != delta.RunID || done.Text == "" || done.Error != "" || done.Turns != 1 {
		t.Errorf("done 事件不符: %+v", done)
	}

	// 未知上行类型回复 WS_UNKNOWN_TYPE。
	if err := conn.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"bogus"}`)); err != nil {
		t.Fatalf("发送未知类型失败: %v", err)
	}
	errReply := readEnvelope(t, conn)
	// errorReply 不是 envelope，反序列化到 envelope 只有 type 字段有效；
	// 直接解析原始 JSON 验证。
	_ = errReply

	// ④ GET messages 验证顺序与完整性：[user, assistant]。
	messages := chatGetMessages(t, httpBase, token, sessionID, "")
	if len(messages) != 2 {
		t.Fatalf("应有 2 条消息，实际: %d", len(messages))
	}
	if messages[0]["role"] != "user" || messages[0]["content"] != "你好，领导" {
		t.Errorf("首条应为用户消息，实际: %+v", messages[0])
	}
	if messages[1]["role"] != "assistant" || messages[1]["content"] != done.Text {
		t.Errorf("次条应为 done 的全文，实际: %+v", messages[1])
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	stop()

	// ⑤ 重启 App（同库）：再发一条，验证历史延续（4 条消息，顺序保持）。
	httpBase, wsBase, _, stop = startChatApp(t, dbPath)
	defer stop()
	// token 落库在 tokens 表，重启后仍然有效。
	conn = dialChat(t, wsBase, token, sessionID)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	sendChatMessage(t, conn, sessionID, "继续聊")
	events = waitDialogueDone(t, conn)
	if got := events[len(events)-1].Type; got != runtime.EventTypeMessageDone {
		t.Fatalf("重启后应收到 message.done，实际: %q", got)
	}

	messages = chatGetMessages(t, httpBase, token, sessionID, "")
	if len(messages) != 4 {
		t.Fatalf("重启后应有 4 条消息，实际: %d", len(messages))
	}
	wantRoles := []string{"user", "assistant", "user", "assistant"}
	wantContents := []string{"你好，领导", done.Text, "继续聊", ""}
	for i, msg := range messages {
		if msg["role"] != wantRoles[i] {
			t.Errorf("第 %d 条角色应为 %q，实际: %+v", i, wantRoles[i], msg)
		}
		if wantContents[i] != "" && msg["content"] != wantContents[i] {
			t.Errorf("第 %d 条内容不符，实际: %+v", i, msg)
		}
	}

	// 分页：page_size=3 时第 2 页应剩 1 条。
	page2 := chatGetMessages(t, httpBase, token, sessionID, "?page=2&page_size=3")
	if len(page2) != 1 || page2[0]["content"] != messages[3]["content"] {
		t.Errorf("分页结果不符: %+v", page2)
	}
}

// TestChatUnknownUplinkType 验证 /ws/chat 收到未知上行类型时回复
// {type:"error", code:"WS_UNKNOWN_TYPE"}。
func TestChatUnknownUplinkType(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "chat.db")
	httpBase, wsBase, _, stop := startChatApp(t, dbPath)
	defer stop()

	token := login(t, httpBase)
	conn := dialChat(t, wsBase, token, "")
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	if err := conn.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"bogus"}`)); err != nil {
		t.Fatalf("发送未知类型失败: %v", err)
	}

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
	if reply.Type != "error" || reply.Code != ws.CodeWSUnknownType {
		t.Errorf("应回复 WS_UNKNOWN_TYPE，实际: %+v", reply)
	}
}
