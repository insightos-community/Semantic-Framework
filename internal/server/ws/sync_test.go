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

package ws

import (
	"errors"
	"io"
	"testing"
	"time"

	"insightos.cn/semantic-framework/pkg/log"
)

// fakeReplayer 是脚本化的断连续传补发器（记录调用、返回预设事件/错误）。
type fakeReplayer struct {
	// envs 预设的补发事件。
	envs []Envelope

	// err 预设错误。
	err error

	// calls 调用次数。
	calls int

	// gotSession / gotLast 记录最后一次调用的参数。
	gotSession string
	gotLast    string
}

// ReplayEvents 记录调用并返回预设结果。
func (f *fakeReplayer) ReplayEvents(sessionID, lastEventID string) ([]Envelope, error) {
	f.calls++
	f.gotSession, f.gotLast = sessionID, lastEventID
	return f.envs, f.err
}

// readRaw 从连接缓冲读一条消息（带超时），不做类型断言。
func readRaw(t *testing.T, c *conn) any {
	t.Helper()
	select {
	case msg := <-c.send:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("2s 内未收到下行消息")
	}
	return nil
}

// TestHandleSyncReplay 验证 sync 补发：缺失事件按序入缓冲，
// 结尾回 sync.done（count=补发条数），会话与游标透传给补发器。
func TestHandleSyncReplay(t *testing.T) {
	replayer := &fakeReplayer{envs: []Envelope{
		NewEnvelope("cs-1", ChannelDialogue, "message.delta", ImportanceNormal, map[string]any{"text": "你"}),
		NewEnvelope("cs-1", ChannelDialogue, "message.done", ImportanceNormal, map[string]any{"text": "你好"}),
	}}
	g := &ChatGateway{syncer: replayer, logger: testLogger()}
	c := newTestConn()

	g.handleUplink(c, "usr-1", "cs-1", newUplink(t, map[string]any{
		"type": "sync", "last_event_id": "evt-0001-a",
	}))

	if replayer.calls != 1 || replayer.gotSession != "cs-1" || replayer.gotLast != "evt-0001-a" {
		t.Fatalf("补发器调用不符: calls=%d session=%q last=%q",
			replayer.calls, replayer.gotSession, replayer.gotLast)
	}
	for i := 0; i < 2; i++ {
		msg := readRaw(t, c)
		env, ok := msg.(Envelope)
		if !ok {
			t.Fatalf("第 %d 条应为 Envelope，实际: %T", i, msg)
		}
		if env.ID != replayer.envs[i].ID {
			t.Errorf("第 %d 条补发事件乱序: %q 应为 %q", i, env.ID, replayer.envs[i].ID)
		}
	}
	done, ok := readRaw(t, c).(syncDoneReply)
	if !ok {
		t.Fatal("结尾应为 syncDoneReply")
	}
	if done.Type != "sync.done" || done.Count != 2 {
		t.Errorf("sync.done 不符: %+v", done)
	}
}

// TestHandleSyncEmptyCursor 验证空游标：不调用补发器，直接回 sync.done(0)。
func TestHandleSyncEmptyCursor(t *testing.T) {
	replayer := &fakeReplayer{}
	g := &ChatGateway{syncer: replayer, logger: testLogger()}
	c := newTestConn()

	g.handleUplink(c, "usr-1", "cs-1", newUplink(t, map[string]any{"type": "sync"}))

	done, ok := readRaw(t, c).(syncDoneReply)
	if !ok {
		t.Fatal("应回 syncDoneReply")
	}
	if done.Count != 0 || replayer.calls != 0 {
		t.Errorf("空游标应回 count=0 且不补发，实际: %+v（calls=%d）", done, replayer.calls)
	}
}

// TestHandleSyncFailure 验证补发失败与未装配两条防御路径都回 SYNC_FAILED。
func TestHandleSyncFailure(t *testing.T) {
	// ① 补发器报错。
	replayer := &fakeReplayer{err: errors.New("store 炸了")}
	g := &ChatGateway{syncer: replayer, logger: testLogger()}
	c := newTestConn()
	g.handleUplink(c, "usr-1", "cs-1", newUplink(t, map[string]any{
		"type": "sync", "last_event_id": "evt-x",
	}))
	if reply := readReply(t, c); reply.Code != CodeWSSyncFailed {
		t.Errorf("补发失败应回 SYNC_FAILED，实际: %+v", reply)
	}

	// ② 服务未装配。
	g2 := &ChatGateway{logger: testLogger()}
	c2 := newTestConn()
	g2.handleUplink(c2, "usr-1", "cs-1", newUplink(t, map[string]any{
		"type": "sync", "last_event_id": "evt-x",
	}))
	if reply := readReply(t, c2); reply.Code != CodeWSSyncFailed {
		t.Errorf("未装配应回 SYNC_FAILED，实际: %+v", reply)
	}
}

// testLogger 返回静默日志器，避免测试输出被日志淹没。
func testLogger() *log.Logger {
	return log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
}
