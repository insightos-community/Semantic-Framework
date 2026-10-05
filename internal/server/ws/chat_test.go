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
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"insightos.cn/semantic-framework/pkg/log"
)

// fakeReplyHandler 是测试用的交互应答处理器：脚本化错误，记录调用。
type fakeReplyHandler struct {
	// err 预设错误。
	err error

	// calls 记录每次调用的 (interactionID, approved)。
	calls [][2]any
}

// fakeScopedReplyHandler 额外实现可选归属校验，验证网关在 Reply 前拦截
// 跨 Conversation 应答；原有 fakeReplyHandler 保持最小接口不变。
type fakeScopedReplyHandler struct {
	*fakeReplyHandler
	authorizeErr   error
	authorizeCalls [][3]string
}

func (f *fakeScopedReplyHandler) AuthorizeReply(_ context.Context,
	userID, sessionID, interactionID string) error {
	f.authorizeCalls = append(f.authorizeCalls, [3]string{userID, sessionID, interactionID})
	return f.authorizeErr
}

// Reply 记录调用并返回预设错误。
func (f *fakeReplyHandler) Reply(_ context.Context, interactionID string, approved bool) error {
	f.calls = append(f.calls, [2]any{interactionID, approved})
	return f.err
}

// fakeMessageHandler 是测试用的对话消息处理器（阻塞可配，验证异步分发）。
type fakeMessageHandler struct {
	// started 处理开始时关闭（通知测试）。
	started chan struct{}

	// unblock 阻塞直到关闭（模拟长 run）。
	unblock chan struct{}

	options     [2]string
	attachments []string
	mode        string
}

// HandleMessage 通知开始并阻塞到 unblock。
func (f *fakeMessageHandler) HandleMessage(_ context.Context, _, _, _ string) (string, error) {
	close(f.started)
	<-f.unblock
	return "run-1", nil
}

func (f *fakeMessageHandler) HandleMessageWithOptions(_ context.Context, _, _, _ string,
	attachments []string, effort, visibility string) (string, error) {
	f.options = [2]string{effort, visibility}
	f.attachments = append([]string(nil), attachments...)
	close(f.started)
	<-f.unblock
	return "run-1", nil
}

func (f *fakeMessageHandler) HandleMessageWithMode(ctx context.Context, userID, sessionID,
	text string, attachments []string, effort, visibility, mode string) (string, error) {
	f.mode = mode
	return f.HandleMessageWithOptions(ctx, userID, sessionID, text, attachments, effort, visibility)
}

// newTestConn 构造不依赖真实 websocket 的测试连接（reply 写入 send 缓冲）。
func newTestConn() *conn {
	return &conn{
		id:     "conn-test",
		send:   make(chan any, sendBuffer),
		done:   make(chan struct{}),
		logger: log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
	}
}

// readReply 从连接缓冲读一条协议应答（带超时）。
func readReply(t *testing.T, c *conn) errorReply {
	t.Helper()
	select {
	case msg := <-c.send:
		reply, ok := msg.(errorReply)
		if !ok {
			t.Fatalf("应答应为 errorReply，实际: %T", msg)
		}
		return reply
	case <-time.After(2 * time.Second):
		t.Fatal("2s 内未收到协议应答")
	}
	return errorReply{}
}

// newUplink 序列化一条上行消息。
func newUplink(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("序列化上行消息失败: %v", err)
	}
	return data
}

// TestChatGatewayInteractionReply 验证 interaction.reply 上行：
// 合法应答透传给交互服务；缺参数/服务错误回 errorReply。
func TestChatGatewayInteractionReply(t *testing.T) {
	replies := &fakeReplyHandler{}
	g := &ChatGateway{
		replies: replies,
		logger:  log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
	}
	c := newTestConn()

	// ① 合法批准：透传 interactionID 与 approved，无错误应答。
	g.handleUplink(c, "usr-1", "cs-1", newUplink(t, map[string]any{
		"type": "interaction.reply", "interaction_id": "int-1", "approved": true,
	}))
	if len(replies.calls) != 1 || replies.calls[0][0] != "int-1" || replies.calls[0][1] != true {
		t.Fatalf("应答应透传，实际: %+v", replies.calls)
	}
	select {
	case msg := <-c.send:
		t.Errorf("合法应答不应有错误回复，实际: %+v", msg)
	default:
	}

	// ② 显式拒绝（approved=false 与缺省要区分开）。
	g.handleUplink(c, "usr-1", "cs-1", newUplink(t, map[string]any{
		"type": "interaction.reply", "interaction_id": "int-2", "approved": false,
	}))
	if len(replies.calls) != 2 || replies.calls[1][1] != false {
		t.Errorf("显式拒绝应透传 false，实际: %+v", replies.calls)
	}

	// ③ 缺 interaction_id / 缺 approved：WS_BAD_MESSAGE。
	g.handleUplink(c, "usr-1", "cs-1", newUplink(t, map[string]any{
		"type": "interaction.reply", "approved": true,
	}))
	if reply := readReply(t, c); reply.Code != CodeWSBadMessage {
		t.Errorf("缺 interaction_id 应回 WS_BAD_MESSAGE，实际: %+v", reply)
	}
	g.handleUplink(c, "usr-1", "cs-1", newUplink(t, map[string]any{
		"type": "interaction.reply", "interaction_id": "int-3",
	}))
	if reply := readReply(t, c); reply.Code != CodeWSBadMessage {
		t.Errorf("缺 approved 应回 WS_BAD_MESSAGE，实际: %+v", reply)
	}

	// ④ 交互服务拒绝（已终结/不存在）：INTERACTION_REPLY_FAILED。
	replies.err = errors.New("交互已终结，无法应答")
	g.handleUplink(c, "usr-1", "cs-1", newUplink(t, map[string]any{
		"type": "interaction.reply", "interaction_id": "int-4", "approved": true,
	}))
	if reply := readReply(t, c); reply.Code != CodeWSInteractionReplyFailed || reply.Message == "" {
		t.Errorf("服务拒绝应回 INTERACTION_REPLY_FAILED，实际: %+v", reply)
	}
}

// TestChatGatewayInteractionReplyScope 验证实现可选归属校验的生产处理器
// 会在 Reply 前收到当前连接的用户和 Conversation；跨会话时不能落库应答。
func TestChatGatewayInteractionReplyScope(t *testing.T) {
	base := &fakeReplyHandler{}
	replies := &fakeScopedReplyHandler{
		fakeReplyHandler: base,
		authorizeErr:     errors.New("交互不存在"),
	}
	g := &ChatGateway{
		replies: replies,
		logger:  log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
	}
	c := newTestConn()

	g.handleUplink(c, "usr-1", "cs-connection", newUplink(t, map[string]any{
		"type": "interaction.reply", "interaction_id": "int-other-session", "approved": true,
	}))
	if len(replies.authorizeCalls) != 1 ||
		replies.authorizeCalls[0] != [3]string{"usr-1", "cs-connection", "int-other-session"} {
		t.Fatalf("归属校验参数不符: %+v", replies.authorizeCalls)
	}
	if len(base.calls) != 0 {
		t.Fatalf("归属校验失败后不应调用 Reply，实际: %+v", base.calls)
	}
	if reply := readReply(t, c); reply.Code != CodeWSInteractionReplyFailed {
		t.Errorf("跨会话应答应被拒绝，实际: %+v", reply)
	}
}

// TestChatGatewayReplyUnblockedDuringRun 验证审批不死锁：chat.message
// 在 goroutine 中执行（run 阻塞期间），同连接的 interaction.reply 仍被处理。
func TestChatGatewayReplyUnblockedDuringRun(t *testing.T) {
	handler := &fakeMessageHandler{started: make(chan struct{}), unblock: make(chan struct{})}
	replies := &fakeReplyHandler{}
	g := &ChatGateway{
		handler: handler,
		replies: replies,
		logger:  log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
	}
	c := newTestConn()

	// 对话消息开始处理（run 阻塞中）。
	g.handleUplink(c, "usr-1", "cs-1", newUplink(t, map[string]any{
		"type": "chat.message", "session_id": "cs-1", "text": "帮我把报告存起来",
		"attachments": []string{"art-1"}, "reasoning_effort": "high",
		"reasoning_visibility": "show",
		"send_scope":           map[string]any{"type": "conversation", "intent": "plan"},
	}))
	select {
	case <-handler.started:
	case <-time.After(2 * time.Second):
		t.Fatal("chat.message 未被分发")
	}
	if handler.mode != "plan" || handler.options != [2]string{"high", "show"} || len(handler.attachments) != 1 ||
		handler.attachments[0] != "art-1" {
		t.Fatalf("模式、推理与附件配置未透传: mode=%s options=%v attachments=%v",
			handler.mode, handler.options, handler.attachments)
	}

	// run 阻塞期间，同连接的 interaction.reply 必须能被处理（读泵不被拖死）。
	g.handleUplink(c, "usr-1", "cs-1", newUplink(t, map[string]any{
		"type": "interaction.reply", "interaction_id": "int-1", "approved": true,
	}))
	if len(replies.calls) != 1 {
		t.Errorf("run 阻塞期间 interaction.reply 应被处理，实际调用: %+v", replies.calls)
	}
	close(handler.unblock)
}

// TestChatGatewayReplyWithoutService 验证交互服务未装配时的防御应答。
func TestChatGatewayReplyWithoutService(t *testing.T) {
	g := &ChatGateway{
		logger: log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
	}
	c := newTestConn()
	g.handleUplink(c, "usr-1", "cs-1", newUplink(t, map[string]any{
		"type": "interaction.reply", "interaction_id": "int-1", "approved": true,
	}))
	if reply := readReply(t, c); reply.Code != CodeWSInteractionReplyFailed {
		t.Errorf("服务未装配件应回 INTERACTION_REPLY_FAILED，实际: %+v", reply)
	}
}

func TestChatGatewayRejectsInvalidReasoningBeforeDispatch(t *testing.T) {
	handler := &fakeMessageHandler{started: make(chan struct{}), unblock: make(chan struct{})}
	g := &ChatGateway{handler: handler,
		logger: log.New(log.Options{Level: log.LevelError, Writer: io.Discard})}
	c := newTestConn()
	g.handleUplink(c, "usr-1", "cs-1", newUplink(t, map[string]any{
		"type": "chat.message", "session_id": "cs-1", "text": "任务",
		"reasoning_effort": "maximum",
	}))
	if reply := readReply(t, c); reply.Code != CodeWSBadMessage {
		t.Fatalf("非法推理配置应在分发前返回 WS_BAD_MESSAGE: %+v", reply)
	}
	select {
	case <-handler.started:
		t.Fatal("非法推理配置不得进入运行时")
	default:
	}
}
