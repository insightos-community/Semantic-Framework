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
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"

	"insightos.cn/semantic-framework/internal/server/ws"
)

// 断连续传（sync）的集成测试：真实装配（bootstrap.Wire）+ mock 模型。
// 事件面链路：runtime → bus → 聚合器（归一化/分级/落库）→ hub → WS 客户端。

// downlink 是一条下行消息的通用解析形态：envelope（channel 非空）或
// 协议应答（sync.done / error，channel 为空）。
type downlink struct {
	// Type 消息类型（envelope 的 type 字段或 sync.done/error）。
	Type string `json:"type"`

	// Channel envelope 频道；协议应答为空。
	Channel string `json:"channel"`

	// ID envelope 事件 id。
	ID string `json:"id"`

	// Count sync.done 的补发条数。
	Count int `json:"count"`

	// Code error 应答的错误码。
	Code string `json:"code"`
}

// readDownlink 读取一条下行消息（带超时）。
func readDownlink(t *testing.T, conn *websocket.Conn) downlink {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("读取下行消息失败: %v", err)
	}
	var msg downlink
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("下行消息应为合法 JSON: %v（data: %s）", err, data)
	}
	return msg
}

// sendSync 发送一条 sync 上行消息。
func sendSync(t *testing.T, conn *websocket.Conn, lastEventID string) {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"type": "sync", "last_event_id": lastEventID,
	})
	if err != nil {
		t.Fatalf("序列化 sync 失败: %v", err)
	}
	if err := conn.Write(context.Background(), websocket.MessageText, payload); err != nil {
		t.Fatalf("发送 sync 失败: %v", err)
	}
}

// awaitSyncDone 读取下行消息直到 sync.done（跳过途中的 envelope），
// 返回途中收到的全部 envelope 与 sync.done 的 count。
func awaitSyncDone(t *testing.T, conn *websocket.Conn) ([]downlink, int) {
	t.Helper()
	var envs []downlink
	for {
		msg := readDownlink(t, conn)
		if msg.Channel != "" {
			envs = append(envs, msg)
			continue
		}
		if msg.Type == "sync.done" {
			return envs, msg.Count
		}
		t.Fatalf("补发期间收到意外协议消息: %+v", msg)
	}
}

// TestAggregateSyncReplay 断连续传全链路：
// 连接收第一条回复（记录 done 游标）→ 断开 → 离线期间服务端再产生两条
// 回复（直接调 runtime.HandleMessage）→ 重连发 sync 补发缺口 →
// 游标推进后重复 sync 幂等（无重复补发）→ agent-events 通道同样支持 sync。
func TestAggregateSyncReplay(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sync.db")
	httpBase, wsBase, app, stop := startChatApp(t, dbPath)
	defer stop()

	token := login(t, httpBase)
	sessionID, conn := createSessionAndDial(t, httpBase, wsBase, token)

	// ① 在线收第一条回复，记录 message.done 的事件 id 作为断点游标。
	sendChatMessage(t, conn, sessionID, "第一条消息")
	events := waitDialogueDone(t, conn)
	cursor := events[len(events)-1].ID
	if cursor == "" {
		t.Fatal("message.done 的事件 id 不应为空（聚合器应已重发 id）")
	}
	if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("关闭连接失败: %v", err)
	}

	// ② 离线期间再产生两条回复（不经 WS：直接调运行时，事件照常落库）。
	sess, err := app.Store().GetChatSession(sessionID)
	if err != nil {
		t.Fatalf("查询会话失败: %v", err)
	}
	for _, text := range []string{"第二条消息", "第三条消息"} {
		if _, err := app.Runtime().HandleMessage(context.Background(), sess.UserID, sessionID, text); err != nil {
			t.Fatalf("HandleMessage(%q) 失败: %v", text, err)
		}
	}

	// 聚合器异步入库：Conversation 创建事件 1 条，每轮包含
	// started、delta、completed、done，共 13 条。
	deadline := time.Now().Add(30 * time.Second)
	for {
		records, err := app.Store().ListEventsAfter(sessionID, "", 0)
		if err == nil && len(records) == 13 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("30s 内事件未全部落库，当前: %d/13（err: %v）", len(records), err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// ③ 重连并发 sync：应补发两轮共 8 条状态与消息事件，
	// id 全部晚于游标且严格递增，随后 sync.done count=8。
	conn = dialChat(t, wsBase, token, sessionID)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	sendSync(t, conn, cursor)
	replayed, count := awaitSyncDone(t, conn)
	if count != 8 || len(replayed) != 8 {
		t.Fatalf("应补发 8 条，实际 count=%d len=%d（%+v）", count, len(replayed), replayed)
	}
	wantTypes := []string{
		"run.started", "message.delta", "run.completed", "message.done",
		"run.started", "message.delta", "run.completed", "message.done",
	}
	for i, msg := range replayed {
		if msg.Channel != string(ws.ChannelDialogue) || msg.Type != wantTypes[i] {
			t.Errorf("第 %d 条补发事件不符: %+v", i, msg)
		}
		if msg.ID <= cursor {
			t.Errorf("第 %d 条补发 id %q 应晚于游标 %q", i, msg.ID, cursor)
		}
		if i > 0 && msg.ID <= replayed[i-1].ID {
			t.Errorf("补发 id 应严格递增: %q 应大于 %q", msg.ID, replayed[i-1].ID)
		}
	}

	// ④ 幂等：游标推进到补发末尾后重复 sync，无重复补发（count=0）。
	sendSync(t, conn, replayed[len(replayed)-1].ID)
	envs, count := awaitSyncDone(t, conn)
	if count != 0 || len(envs) != 0 {
		t.Errorf("重复 sync 应无补发，实际 count=%d envs=%+v", count, envs)
	}

	// ⑤ 空游标：只要实时流，回 sync.done(0) 不补发。
	sendSync(t, conn, "")
	envs, count = awaitSyncDone(t, conn)
	if count != 0 || len(envs) != 0 {
		t.Errorf("空游标应回 count=0 且不补发，实际 count=%d envs=%+v", count, envs)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")

	// ⑥ /ws/agent-events 通道同样支持 sync（同一游标语义）。
	eventsConn, _, err := websocket.Dial(context.Background(),
		wsBase+"/ws/agent-events?token="+token+"&session_id="+sessionID, nil)
	if err != nil {
		t.Fatalf("agent-events 连接失败: %v", err)
	}
	defer func() { _ = eventsConn.Close(websocket.StatusNormalClosure, "") }()
	sendSync(t, eventsConn, cursor)
	replayed, count = awaitSyncDone(t, eventsConn)
	if count != 8 || len(replayed) != 8 {
		t.Errorf("agent-events 应补发 8 条，实际 count=%d len=%d", count, len(replayed))
	}
}
