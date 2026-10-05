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
	"net/http"

	"github.com/coder/websocket"

	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/pkg/log"
)

// 对话上行消息的类型取值与错误码（/ws/chat 协议的一部分，禁止改名）。
const (
	// uplinkTypeChatMessage 用户对话消息。
	uplinkTypeChatMessage = "chat.message"

	// uplinkTypeChatCancel 用户中断当前会话正在执行的 run。
	uplinkTypeChatCancel = "chat.cancel"

	// uplinkTypeInteractionReply 交互应答（confirm 审批的批准/拒绝）。
	uplinkTypeInteractionReply  = "interaction.reply"
	uplinkTypeInteractionCancel = "interaction.cancel"

	// uplinkTypeSync 断连续传请求（重连后按 last_event_id 补发缺口）。
	uplinkTypeSync = "sync"

	// CodeWSUnknownType 上行消息类型未知。
	CodeWSUnknownType = "WS_UNKNOWN_TYPE"

	// CodeWSBadMessage 上行消息格式或参数非法。
	CodeWSBadMessage = "WS_BAD_MESSAGE"

	// CodeWSMessageFailed 消息处理失败（运行时错误）。
	CodeWSMessageFailed = "CHAT_MESSAGE_FAILED"

	// CodeWSInteractionReplyFailed 交互应答处理失败（含不存在/已终结/非法）。
	CodeWSInteractionReplyFailed = "INTERACTION_REPLY_FAILED"

	// CodeWSSyncFailed 断连续传补发失败（含服务未装配）。
	CodeWSSyncFailed = "SYNC_FAILED"
)

// MessageHandler 是对话消息的处理接口（由 AgentRuntime 实现）。
// 为什么定义在 ws 包：runtime 需要引用本包的 Envelope 发布下行事件，
// 若本包再引用 runtime 会形成包循环——接口倒置保持依赖单向。
type MessageHandler interface {
	// HandleMessage 处理一条用户消息，返回本次运行 ID。
	HandleMessage(ctx context.Context, userID, sessionID, text string) (string, error)
}

// AttachmentMessageHandler 是 MessageHandler 可选实现的多模态输入能力。
type AttachmentMessageHandler interface {
	HandleMessageWithAttachments(ctx context.Context, userID, sessionID, text string, attachmentIDs []string) (string, error)
}

// ConfiguredMessageHandler 是 MessageHandler 的可选单轮配置能力。空字符串表示
// 继承 Agent 配置；auto 表示本轮显式启用自动策略。
type ConfiguredMessageHandler interface {
	HandleMessageWithOptions(ctx context.Context, userID, sessionID, text string,
		attachmentIDs []string, reasoningEffort, reasoningVisibility string) (string, error)
}

// ModeAwareMessageHandler 在同一 Conversation 入口中传递本轮交互模式。
// mode 只允许 collaboration/plan；它只收窄本轮提示词与工具权限。
type ModeAwareMessageHandler interface {
	HandleMessageWithMode(ctx context.Context, userID, sessionID, text string,
		attachmentIDs []string, reasoningEffort, reasoningVisibility, mode string) (string, error)
}

// RoutedMessageHandler 接收用户从 Agent 目录选择的实例身份。
type RoutedMessageHandler interface {
	HandleMessageToAgent(ctx context.Context, userID, sessionID, text string,
		attachmentIDs []string, reasoningEffort, reasoningVisibility, mode, agentID string) (string, error)
}

// RunController 是 MessageHandler 可选实现的运行控制能力。
type RunController interface {
	CancelRun(ctx context.Context, userID, sessionID string) error
}

// InteractionReplyHandler 是交互应答的处理接口（由 internal/interaction
// 实现，同一接口倒置理由）。应答语义：先落库再唤醒等待中的 run。
type InteractionReplyHandler interface {
	// Reply 处理一次用户应答：interactionID 定位交互，approved 为批准/拒绝。
	Reply(ctx context.Context, interactionID string, approved bool) error
}

// StructuredInteractionReplyHandler 是 Studio v0.3 的通用交互应答入口。
type StructuredInteractionReplyHandler interface {
	ReplyStructured(ctx context.Context, userID, projectID, interactionID string,
		response json.RawMessage, sourceRevision int64) error
}

type StructuredInteractionCancelHandler interface {
	CancelStructured(ctx context.Context, userID, projectID, interactionID string) error
}

// InteractionReplyAuthorizer 是 InteractionReplyHandler 的可选归属校验能力。
// 生产 Interaction 服务实现该接口，旧测试替身和迁移期实现无需被迫扩展。
// 网关在调用 Reply 前先确认交互属于当前连接订阅的 Conversation，且该
// Conversation 属于通过 token 识别出的用户，避免用已知 ID 跨会话应答。
type InteractionReplyAuthorizer interface {
	AuthorizeReply(ctx context.Context, userID, sessionID, interactionID string) error
}

// EventReplayer 是断连续传的事件补发接口（由 aggregate.Aggregator 实现，
// 同一接口倒置理由：ws 不引用 aggregate，保持依赖单向）。
type EventReplayer interface {
	// ReplayEvents 返回会话中 id 晚于 lastEventID 的缺失事件
	// （按 id 升序，可丢的 trace 频道不在其列）。
	ReplayEvents(sessionID, lastEventID string) ([]Envelope, error)
}

// uplinkMessage 是 /ws/chat 上行消息的协议结构。
type uplinkMessage struct {
	// Type 消息类型（chat.message / interaction.reply / sync）。
	Type string `json:"type"`

	// SessionID 目标会话 ID（chat.message 必填）。
	SessionID string `json:"session_id"`

	// RunID 是 Studio 精确取消的目标 Run。
	RunID string `json:"run_id"`

	// AfterSequence 是 Project Snapshot 后的事件位置。
	AfterSequence int64 `json:"after_sequence"`

	// Text 消息文本（chat.message 必填）。
	Text string `json:"text"`

	// Attachments 已上传图片的 artifact ID。
	Attachments []string `json:"attachments"`

	// InterruptCurrent 发送前先中断当前 run（运行中纠偏）。
	InterruptCurrent bool `json:"interrupt_current"`

	// ReasoningEffort 本轮推理强度覆盖：inherit/auto/low/medium/high。
	ReasoningEffort string `json:"reasoning_effort"`

	// ReasoningVisibility 本轮思考展示覆盖：inherit/auto/show/hide。
	ReasoningVisibility string `json:"reasoning_visibility"`

	// SendScope 是 Studio 输入框的显示与路由范围。v0.3 只从 Conversation
	// scope 读取 intent=plan；其他值按 collaboration 处理。
	SendScope struct {
		Type          string `json:"type"`
		Intent        string `json:"intent"`
		TargetAgentID string `json:"target_agent_id"`
	} `json:"send_scope"`

	// InteractionID 目标交互 ID（interaction.reply 必填）。
	InteractionID string `json:"interaction_id"`

	// Approved 是旧 confirm 客户端的兼容字段。
	Approved *bool `json:"approved"`

	// Response 与 ExpectedStateRevision 是 v0.3 通用结构化应答。
	Response              json.RawMessage `json:"response"`
	ExpectedStateRevision int64           `json:"expected_state_revision"`
	SourceRevision        *int64          `json:"source_revision"`

	// LastEventID 断连续传游标（sync 必填；为空表示不补发、只回 sync.done）。
	LastEventID string `json:"last_event_id"`
}

// errorReply 是上行消息的直接错误应答（协议应答，非 envelope）。
type errorReply struct {
	// Type 固定为 error。
	Type string `json:"type"`

	// Code 机器可读错误码。
	Code string `json:"code"`

	// Message 人类可读错误描述。
	Message string `json:"message"`
}

// syncDoneReply 是 sync 上行处理完成的协议应答（协议应答，非 envelope）。
type syncDoneReply struct {
	// Type 固定为 sync.done。
	Type string `json:"type"`

	// Count 本次补发的事件条数。
	Count int `json:"count"`
}

// ChatGateway 是 /ws/chat 的接入处理器：鉴权升级后，上行 chat.message
// 交给 MessageHandler（AgentRuntime）执行，interaction.reply 交给
// InteractionReplyHandler（交互服务）；下行 dialogue/interaction 事件
// 复用 Hub 按 session_id 投递（连接经 query 参数 session_id 订阅）。
type ChatGateway struct {
	// hub 连接中枢，负责按会话投递事件。
	hub *Hub

	// auth 认证服务，校验连接携带的 token。
	auth *auth.Service

	// handler 对话消息处理器。
	handler MessageHandler

	// replies 交互应答处理器；nil 时 interaction.reply 上行回错误（服务未装配）。
	replies InteractionReplyHandler

	// syncer 断连续传补发器；nil 时 sync 上行回错误（服务未装配）。
	syncer EventReplayer

	// logger 结构化日志器。
	logger *log.Logger
}

// NewChatGateway 创建对话 WS 网关。
func NewChatGateway(hub *Hub, authSvc *auth.Service, handler MessageHandler,
	replies InteractionReplyHandler, syncer EventReplayer, logger *log.Logger) *ChatGateway {
	return &ChatGateway{hub: hub, auth: authSvc, handler: handler,
		replies: replies, syncer: syncer, logger: logger}
}

// ServeHTTP 处理 GET /ws/chat：鉴权 → 升级 → 注册 → 泵循环。
// token 与 session_id 的来源约定同 /ws/agent-events（见 Gateway.ServeHTTP）。
func (g *ChatGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, echoProtocol := extractToken(r)
	userID, err := g.auth.ValidateToken(token)
	if err != nil {
		var aerr *auth.Error
		if errors.As(err, &aerr) {
			writeError(w, http.StatusUnauthorized, aerr.Code, aerr.Message)
			return
		}
		g.logger.WithError(err).Error("WS 鉴权内部错误")
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务内部错误")
		return
	}

	acceptOpts := &websocket.AcceptOptions{}
	if echoProtocol != "" {
		acceptOpts.Subprotocols = []string{echoProtocol}
	}
	wsConn, err := websocket.Accept(w, r, acceptOpts)
	if err != nil {
		g.logger.WithError(err).Warn("WS 升级失败", "remote", r.RemoteAddr)
		return
	}
	wsConn.SetReadLimit(readLimit)

	sessionID := r.URL.Query().Get("session_id")
	c := newConn(wsConn, userID, g.logger)
	g.hub.Subscribe(sessionID, c)
	g.logger.Info("WS 对话连接已建立",
		"conn_id", c.id, "user_id", userID, "session_id", sessionID, "remote", r.RemoteAddr)
	defer func() {
		c.close()
		g.hub.Unsubscribe(sessionID, c)
		g.logger.Info("WS 对话连接已断开", "conn_id", c.id, "user_id", userID, "session_id", sessionID)
	}()

	go c.writePump()
	go c.heartbeat()
	c.readPumpWith(func(data []byte) {
		g.handleUplink(c, userID, sessionID, data)
	})
}

// handleUplink 处理一条上行消息：解析协议结构后按类型分发。
// chat.message 在独立 goroutine 中执行：一次 run 可能长时间阻塞
// （危险工具等待人工审批），读泵必须保持可读——否则同连接的
// interaction.reply 永远到不了，审批死锁。同会话的串行语义由
// runtime 的会话锁兜底；消息不依赖连接存活——即使处理期间断连，
// 回复照常落库，重连后经 REST 拉取（消息不丢）。
// interaction.reply 与 sync 是快路径（落库 + 唤醒 / 查询 + 补发），
// 在读泵内同步处理，保证应答与错误回复的顺序确定。
func (g *ChatGateway) handleUplink(c *conn, userID, sessionID string, data []byte) {
	var msg uplinkMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		c.reply(errorReply{Type: "error", Code: CodeWSBadMessage, Message: "消息不是合法 JSON"})
		return
	}

	switch msg.Type {
	case uplinkTypeChatMessage:
		if msg.SessionID == "" || (msg.Text == "" && len(msg.Attachments) == 0) {
			c.reply(errorReply{Type: "error", Code: CodeWSBadMessage, Message: "session_id 及 text/attachments 不能为空"})
			return
		}
		if !validReasoningEffort(msg.ReasoningEffort) ||
			!validReasoningVisibility(msg.ReasoningVisibility) {
			c.reply(errorReply{Type: "error", Code: CodeWSBadMessage,
				Message: "reasoning_effort 或 reasoning_visibility 非法"})
			return
		}
		if msg.InterruptCurrent {
			if !g.cancelRun(c, userID, msg.SessionID) {
				return
			}
		}
		go g.handleChatMessage(c, userID, msg.SessionID, msg.Text, msg.Attachments,
			msg.ReasoningEffort, msg.ReasoningVisibility, msg.conversationMode(), msg.SendScope.TargetAgentID)
	case uplinkTypeChatCancel:
		if msg.SessionID == "" {
			c.reply(errorReply{Type: "error", Code: CodeWSBadMessage, Message: "session_id 不能为空"})
			return
		}
		g.cancelRun(c, userID, msg.SessionID)
	case uplinkTypeInteractionReply:
		g.handleInteractionReply(c, userID, sessionID, msg)
	case uplinkTypeSync:
		handleSync(c, g.syncer, sessionID, msg.LastEventID, g.logger)
	default:
		c.reply(errorReply{Type: "error", Code: CodeWSUnknownType, Message: "未知消息类型: " + msg.Type})
	}
}

func validReasoningEffort(value string) bool {
	return value == "" || value == "inherit" || value == "auto" || value == "low" ||
		value == "medium" || value == "high"
}

func validReasoningVisibility(value string) bool {
	return value == "" || value == "inherit" || value == "auto" || value == "show" ||
		value == "hide"
}

func (m uplinkMessage) conversationMode() string {
	if m.SendScope.Type == "conversation" && m.SendScope.Intent == "plan" {
		return "plan"
	}
	return "collaboration"
}

func (g *ChatGateway) cancelRun(c *conn, userID, sessionID string) bool {
	controller, ok := g.handler.(RunController)
	if !ok {
		c.reply(errorReply{Type: "error", Code: CodeWSMessageFailed, Message: "运行控制未装配"})
		return false
	}
	if err := controller.CancelRun(context.Background(), userID, sessionID); err != nil {
		c.reply(errorReply{Type: "error", Code: CodeWSMessageFailed, Message: err.Error()})
		return false
	}
	return true
}

// handleChatMessage 执行一条对话消息（独立 goroutine，见 handleUplink）。
// ctx 用 Background：run 与连接解耦（runtime 内部再派生），
// 断连不中断执行，结果照常落库。
func (g *ChatGateway) handleChatMessage(c *conn, userID, sessionID, text string, attachments []string,
	reasoningEffort, reasoningVisibility, mode, agentID string) {
	var err error
	if handler, ok := g.handler.(RoutedMessageHandler); ok {
		_, err = handler.HandleMessageToAgent(context.Background(), userID, sessionID, text,
			attachments, reasoningEffort, reasoningVisibility, mode, agentID)
	} else if agentID != "" && agentID != "leader" {
		err = errors.New("当前服务尚不支持指定 Agent")
	} else if handler, ok := g.handler.(ModeAwareMessageHandler); ok {
		_, err = handler.HandleMessageWithMode(context.Background(), userID, sessionID, text,
			attachments, reasoningEffort, reasoningVisibility, mode)
	} else if handler, ok := g.handler.(ConfiguredMessageHandler); ok {
		_, err = handler.HandleMessageWithOptions(context.Background(), userID, sessionID, text,
			attachments, reasoningEffort, reasoningVisibility)
	} else if len(attachments) > 0 {
		if handler, ok := g.handler.(AttachmentMessageHandler); ok {
			_, err = handler.HandleMessageWithAttachments(context.Background(), userID, sessionID, text, attachments)
		} else {
			err = errors.New("多模态消息处理未装配")
		}
	} else {
		_, err = g.handler.HandleMessage(context.Background(), userID, sessionID, text)
	}
	if err != nil {
		g.logger.WithError(err).Warn("对话消息处理失败",
			"conn_id", c.id, "user_id", userID, "session_id", sessionID)
		c.reply(errorReply{Type: "error", Code: CodeWSMessageFailed, Message: err.Error()})
	}
}

// handleInteractionReply 处理一条交互应答：参数校验后交给交互服务；
// 非法/过期应答回 errorReply（交互不存在、已终结、approved 缺省）。
func (g *ChatGateway) handleInteractionReply(c *conn, userID, sessionID string, msg uplinkMessage) {
	if msg.InteractionID == "" || msg.Approved == nil {
		c.reply(errorReply{Type: "error", Code: CodeWSBadMessage, Message: "interaction_id 与 approved 不能为空"})
		return
	}
	if g.replies == nil {
		c.reply(errorReply{Type: "error", Code: CodeWSInteractionReplyFailed, Message: "交互服务未装配"})
		return
	}
	if authorizer, ok := g.replies.(InteractionReplyAuthorizer); ok {
		if err := authorizer.AuthorizeReply(context.Background(), userID, sessionID, msg.InteractionID); err != nil {
			g.logger.WithError(err).Warn("交互应答归属校验失败",
				"conn_id", c.id, "user_id", userID, "session_id", sessionID,
				"interaction_id", msg.InteractionID)
			c.reply(errorReply{Type: "error", Code: CodeWSInteractionReplyFailed, Message: err.Error()})
			return
		}
	}
	if err := g.replies.Reply(context.Background(), msg.InteractionID, *msg.Approved); err != nil {
		g.logger.WithError(err).Warn("交互应答处理失败",
			"conn_id", c.id, "user_id", userID, "interaction_id", msg.InteractionID)
		c.reply(errorReply{Type: "error", Code: CodeWSInteractionReplyFailed, Message: err.Error()})
		return
	}
	g.logger.Info("交互应答已受理",
		"conn_id", c.id, "user_id", userID, "interaction_id", msg.InteractionID, "approved", *msg.Approved)
}
