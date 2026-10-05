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
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/server/ws"
)

// login 调用登录接口获取 token。
func login(t *testing.T, httpBase string) string {
	t.Helper()
	resp, err := http.Post(httpBase+"/api/v1/auth/login", "application/json",
		bytes.NewBufferString(`{"username":"admin","password":"test-admin-pass"}`))
	if err != nil {
		t.Fatalf("登录请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("登录应返回 200，实际: %d", resp.StatusCode)
	}
	var body struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析登录响应失败: %v", err)
	}
	if body.Token == "" || body.ExpiresAt == "" {
		t.Fatalf("登录响应应包含 token 与 expires_at，实际: %+v", body)
	}
	return body.Token
}

// TestAccessAuthFlow 验证 HTTP 鉴权链路：登录拿 token 后带 token 请求
// /api/v1/system/ping 返回 200 + pong + user_id；无 token 请求返回 401
// 统一错误格式。
func TestAccessAuthFlow(t *testing.T) {
	httpBase, _, _, stop := startApp(t)
	defer stop()

	token := login(t, httpBase)

	// 带 token：200 + pong + user_id。
	req, err := http.NewRequest(http.MethodGet, httpBase+"/api/v1/system/ping", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("ping 请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("带 token 的 ping 应返回 200，实际: %d", resp.StatusCode)
	}
	var pingBody struct {
		Pong   bool   `json:"pong"`
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pingBody); err != nil {
		t.Fatalf("解析 ping 响应失败: %v", err)
	}
	if !pingBody.Pong || pingBody.UserID == "" {
		t.Errorf("ping 响应应为 pong=true 且含 user_id，实际: %+v", pingBody)
	}

	// 无 token：401 + 统一错误格式 {"error":{"code","message"}}。
	resp2, err := http.Get(httpBase + "/api/v1/system/ping")
	if err != nil {
		t.Fatalf("ping 请求失败: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 token 的 ping 应返回 401，实际: %d", resp2.StatusCode)
	}
	var errBody struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&errBody); err != nil {
		t.Fatalf("401 响应应为统一错误格式: %v", err)
	}
	if errBody.Error.Code == "" || errBody.Error.Message == "" {
		t.Errorf("错误响应应包含非空 code 与 message，实际: %+v", errBody.Error)
	}
}

// TestAccessEventDownlink 验证事件下行全链路：bootstrap 内 event.Bus
// 发布 envelope → 桥接到 WS hub → 按 session_id 投递 → WS 客户端
// 收到合法的 envelope JSON。
func TestAccessEventDownlink(t *testing.T) {
	// 登录走 HTTP 监听，事件下行走 WS 监听（两个独立 listener）。
	httpBase, wsBase, app, stop := startApp(t)
	defer stop()

	token := login(t, httpBase)

	// 建立订阅 cs-it 会话的 WS 连接。
	conn, _, err := websocket.Dial(context.Background(),
		wsBase+"/ws/agent-events?token="+token+"&session_id=cs-it", nil)
	if err != nil {
		t.Fatalf("WS 连接失败: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	// 连接注册与 Publish 存在微小竞态（握手响应先于 Subscribe 返回）。
	// coder/websocket 在 Read 的 ctx 到期时会关闭整条连接，因此不能对
	// 同一条连接做短超时重试；改为后台重复发布，客户端一次长超时 Read。
	newEvent := func() ws.Envelope {
		env := ws.NewEnvelope("cs-it", ws.ChannelArtifact, "artifact.created", ws.ImportanceNormal,
			map[string]any{"artifact_id": "art-demo"})
		env.Agent = ws.AgentRef{ID: "robot-a", Role: "robot", Name: "星海图 A 号"}
		env.Parent = ws.ParentRef{RunID: "run-001", TraceID: "trace-007"}
		return env
	}

	stopPub := make(chan struct{})
	defer close(stopPub)
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		app.EventBus().Publish(event.TopicAgentEvents, newEvent())
		for {
			select {
			case <-stopPub:
				return
			case <-ticker.C:
				app.EventBus().Publish(event.TopicAgentEvents, newEvent())
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("5s 内未收到任何下行事件: %v", err)
	}
	var got ws.Envelope
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("下行消息应为合法 envelope JSON: %v（data: %s）", err, data)
	}

	// 校验 envelope 协议字段（docs/architecture/02 §3.3）。
	if got.ID == "" {
		t.Error("envelope.id 不应为空")
	}
	if got.SessionID != "cs-it" {
		t.Errorf("envelope.session_id 应为 cs-it，实际: %q", got.SessionID)
	}
	if got.Ts.IsZero() {
		t.Error("envelope.ts 不应为零值")
	}
	if got.Channel != ws.ChannelArtifact || got.Type != "artifact.created" {
		t.Errorf("envelope channel/type 不符: %+v", got)
	}
	if got.Importance != ws.ImportanceNormal {
		t.Errorf("envelope.importance 应为 normal，实际: %q", got.Importance)
	}
	if got.Agent.ID != "robot-a" || got.Parent.TraceID != "trace-007" {
		t.Errorf("envelope agent/parent 不符: %+v", got)
	}
}
