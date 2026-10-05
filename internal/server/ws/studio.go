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

const (
	uplinkTypeRunCancel = "run.cancel"
)

// StudioAccess 是 Store 提供的 Project 归属和写入状态检查。
type StudioAccess interface {
	ProjectOwnedByUser(userID, projectID string) error
	ProjectWritableByUser(userID, projectID string) error
	ConversationBelongsToProject(userID, projectID, sessionID string) error
	RunBelongsToProject(userID, projectID, runID string) error
	InteractionBelongsToProject(userID, projectID, interactionID string) error
}

// ProjectEventReplayer 按 Project sequence 补发增量。
type ProjectEventReplayer interface {
	ReplayProjectEvents(projectID string, afterSequence int64) ([]Envelope, error)
}

// StudioRunController 只允许按明确 Run ID 取消。
type StudioRunController interface {
	CancelRunByID(ctx context.Context, userID, projectID, runID string) error
}

// ProjectPresence 接收通过鉴权的 Studio 标签页连接变化。实现方只能据此管理
// Project 资源租约，不能把 WS 连接本身当作业务状态。
type ProjectPresence interface {
	Connected(projectID string)
	Disconnected(projectID string)
}

type studioSyncDoneReply struct {
	Type         string `json:"type"`
	Count        int    `json:"count"`
	LastSequence int64  `json:"last_sequence"`
}

// StudioGateway 是 /ws/studio 的 Project 级业务通道。连接固定绑定一个
// project_id，消息、取消和交互应答在进入 Runtime 前再次检查资源归属。
type StudioGateway struct {
	hub      *Hub
	auth     *auth.Service
	handler  MessageHandler
	replies  InteractionReplyHandler
	syncer   ProjectEventReplayer
	access   StudioAccess
	logger   *log.Logger
	presence ProjectPresence
}

func NewStudioGateway(hub *Hub, authService *auth.Service, handler MessageHandler,
	replies InteractionReplyHandler, syncer ProjectEventReplayer,
	access StudioAccess, logger *log.Logger) *StudioGateway {
	return &StudioGateway{hub: hub, auth: authService, handler: handler,
		replies: replies, syncer: syncer, access: access, logger: logger}
}

// SetProjectPresence 在 Bootstrap 装配仿真服务后接入租约管理器。
func (g *StudioGateway) SetProjectPresence(presence ProjectPresence) {
	g.presence = presence
}

// ServeHTTP 完成鉴权和 Project 归属检查后再升级连接。非活动 Project 仍可
// 打开只读工作台，产生新状态的上行命令会在 handleUplink 中单独拒绝。
func (g *StudioGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, echoProtocol := extractToken(r)
	userID, err := g.auth.ValidateToken(token)
	if err != nil {
		var authError *auth.Error
		if errors.As(err, &authError) {
			writeError(w, http.StatusUnauthorized, authError.Code, authError.Message)
		} else {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务内部错误")
		}
		return
	}
	projectID := r.URL.Query().Get("project_id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, CodeWSBadMessage, "project_id 不能为空")
		return
	}
	if g.access == nil || g.access.ProjectOwnedByUser(userID, projectID) != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "Project 不存在")
		return
	}

	acceptOptions := &websocket.AcceptOptions{}
	if echoProtocol != "" {
		acceptOptions.Subprotocols = []string{echoProtocol}
	}
	socket, err := websocket.Accept(w, r, acceptOptions)
	if err != nil {
		g.logger.WithError(err).Warn("Studio WS 升级失败", "remote", r.RemoteAddr)
		return
	}
	socket.SetReadLimit(readLimit)
	connection := newConn(socket, userID, g.logger)
	g.hub.SubscribeProject(projectID, connection)
	if g.presence != nil {
		g.presence.Connected(projectID)
	}
	g.logger.Info("Studio WS 已连接", "conn_id", connection.id,
		"user_id", userID, "project_id", projectID)
	defer func() {
		connection.close()
		g.hub.UnsubscribeProject(projectID, connection)
		if g.presence != nil {
			g.presence.Disconnected(projectID)
		}
		g.logger.Info("Studio WS 已断开", "conn_id", connection.id,
			"user_id", userID, "project_id", projectID)
	}()

	go connection.writePump()
	go connection.heartbeat()
	connection.readPumpWith(func(data []byte) {
		g.handleUplink(connection, userID, projectID, data)
	})
}

func (g *StudioGateway) handleUplink(connection *conn, userID, projectID string, data []byte) {
	var message uplinkMessage
	if err := json.Unmarshal(data, &message); err != nil {
		connection.reply(errorReply{Type: "error", Code: CodeWSBadMessage,
			Message: "消息不是合法 JSON"})
		return
	}
	switch message.Type {
	case uplinkTypeChatMessage:
		g.handleMessageCommand(connection, userID, projectID, message)
	case uplinkTypeChatCancel, uplinkTypeRunCancel:
		g.handleCancelCommand(connection, userID, projectID, message.RunID)
	case uplinkTypeInteractionReply:
		g.handleInteractionCommand(connection, userID, projectID, message)
	case uplinkTypeInteractionCancel:
		g.handleInteractionCancelCommand(connection, userID, projectID, message.InteractionID)
	case uplinkTypeSync:
		g.handleProjectSync(connection, projectID, message.AfterSequence)
	default:
		connection.reply(errorReply{Type: "error", Code: CodeWSUnknownType,
			Message: "未知消息类型: " + message.Type})
	}
}

func (g *StudioGateway) handleInteractionCancelCommand(connection *conn, userID,
	projectID, interactionID string) {
	if interactionID == "" {
		connection.reply(errorReply{Type: "error", Code: CodeWSBadMessage,
			Message: "interaction_id不能为空"})
		return
	}
	if err := g.access.ProjectWritableByUser(userID, projectID); err != nil {
		connection.reply(errorReply{Type: "error", Code: "PROJECT_INACTIVE",
			Message: "Project当前不可写，请先激活"})
		return
	}
	if err := g.access.InteractionBelongsToProject(userID, projectID, interactionID); err != nil {
		connection.reply(errorReply{Type: "error", Code: CodeWSBadMessage,
			Message: "Interaction不属于当前Project"})
		return
	}
	canceller, ok := g.replies.(StructuredInteractionCancelHandler)
	if !ok {
		connection.reply(errorReply{Type: "error", Code: CodeWSInteractionReplyFailed,
			Message: "交互取消服务未装配"})
		return
	}
	if err := canceller.CancelStructured(context.Background(), userID, projectID,
		interactionID); err != nil {
		connection.reply(errorReply{Type: "error", Code: CodeWSInteractionReplyFailed,
			Message: err.Error()})
	}
}

func (g *StudioGateway) handleMessageCommand(connection *conn, userID, projectID string, message uplinkMessage) {
	if message.SessionID == "" || (message.Text == "" && len(message.Attachments) == 0) {
		connection.reply(errorReply{Type: "error", Code: CodeWSBadMessage,
			Message: "session_id 及 text/attachments 不能为空"})
		return
	}
	if !validReasoningEffort(message.ReasoningEffort) ||
		!validReasoningVisibility(message.ReasoningVisibility) {
		connection.reply(errorReply{Type: "error", Code: CodeWSBadMessage,
			Message: "reasoning_effort 或 reasoning_visibility 非法"})
		return
	}
	if err := g.access.ProjectWritableByUser(userID, projectID); err != nil {
		connection.reply(errorReply{Type: "error", Code: "PROJECT_INACTIVE",
			Message: "Project 当前不可写，请先激活"})
		return
	}
	if err := g.access.ConversationBelongsToProject(userID, projectID,
		message.SessionID); err != nil {
		connection.reply(errorReply{Type: "error", Code: CodeWSBadMessage,
			Message: "Conversation 不属于当前 Project"})
		return
	}
	if message.InterruptCurrent {
		if message.RunID == "" ||
			!g.cancelRunByID(connection, userID, projectID, message.RunID) {
			return
		}
	}
	go g.executeMessage(connection, userID, message)
}

func (g *StudioGateway) executeMessage(connection *conn, userID string, message uplinkMessage) {
	var err error
	if handler, ok := g.handler.(RoutedMessageHandler); ok {
		_, err = handler.HandleMessageToAgent(context.Background(), userID,
			message.SessionID, message.Text, message.Attachments,
			message.ReasoningEffort, message.ReasoningVisibility, message.conversationMode(), message.SendScope.TargetAgentID)
	} else if message.SendScope.TargetAgentID != "" && message.SendScope.TargetAgentID != "leader" {
		err = errors.New("当前服务尚不支持指定 Agent")
	} else if handler, ok := g.handler.(ModeAwareMessageHandler); ok {
		_, err = handler.HandleMessageWithMode(context.Background(), userID,
			message.SessionID, message.Text, message.Attachments,
			message.ReasoningEffort, message.ReasoningVisibility, message.conversationMode())
	} else if handler, ok := g.handler.(ConfiguredMessageHandler); ok {
		_, err = handler.HandleMessageWithOptions(context.Background(), userID,
			message.SessionID, message.Text, message.Attachments,
			message.ReasoningEffort, message.ReasoningVisibility)
	} else if len(message.Attachments) > 0 {
		if handler, ok := g.handler.(AttachmentMessageHandler); ok {
			_, err = handler.HandleMessageWithAttachments(context.Background(), userID,
				message.SessionID, message.Text, message.Attachments)
		} else {
			err = errors.New("多模态消息处理未装配")
		}
	} else {
		_, err = g.handler.HandleMessage(context.Background(), userID,
			message.SessionID, message.Text)
	}
	if err != nil {
		g.logger.WithError(err).Warn("Studio 对话消息处理失败",
			"conn_id", connection.id, "session_id", message.SessionID)
		connection.reply(errorReply{Type: "error", Code: CodeWSMessageFailed,
			Message: err.Error()})
	}
}

func (g *StudioGateway) handleCancelCommand(connection *conn, userID, projectID, runID string) {
	if runID == "" {
		connection.reply(errorReply{Type: "error", Code: CodeWSBadMessage,
			Message: "run_id 不能为空"})
		return
	}
	g.cancelRunByID(connection, userID, projectID, runID)
}

func (g *StudioGateway) cancelRunByID(connection *conn, userID, projectID, runID string) bool {
	if err := g.access.RunBelongsToProject(userID, projectID, runID); err != nil {
		connection.reply(errorReply{Type: "error", Code: CodeWSBadMessage,
			Message: "Run 不属于当前 Project"})
		return false
	}
	controller, ok := g.handler.(StudioRunController)
	if !ok {
		connection.reply(errorReply{Type: "error", Code: CodeWSMessageFailed,
			Message: "精确 Run 取消服务尚未装配"})
		return false
	}
	if err := controller.CancelRunByID(context.Background(), userID, projectID, runID); err != nil {
		connection.reply(errorReply{Type: "error", Code: CodeWSMessageFailed,
			Message: err.Error()})
		return false
	}
	return true
}

func (g *StudioGateway) handleInteractionCommand(connection *conn, userID, projectID string, message uplinkMessage) {
	if message.InteractionID == "" || (len(message.Response) == 0 && message.Approved == nil) {
		connection.reply(errorReply{Type: "error", Code: CodeWSBadMessage,
			Message: "interaction_id 与 response 不能为空"})
		return
	}
	if err := g.access.ProjectWritableByUser(userID, projectID); err != nil {
		connection.reply(errorReply{Type: "error", Code: "PROJECT_INACTIVE",
			Message: "Project 当前不可写，请先激活"})
		return
	}
	if err := g.access.InteractionBelongsToProject(userID, projectID,
		message.InteractionID); err != nil {
		connection.reply(errorReply{Type: "error", Code: CodeWSBadMessage,
			Message: "Interaction 不属于当前 Project"})
		return
	}
	if g.replies == nil {
		connection.reply(errorReply{Type: "error", Code: CodeWSInteractionReplyFailed,
			Message: "交互服务未装配"})
		return
	}
	response := message.Response
	if len(response) == 0 && message.Approved != nil {
		response, _ = json.Marshal(map[string]bool{"approved": *message.Approved})
	}
	if structured, ok := g.replies.(StructuredInteractionReplyHandler); ok {
		sourceRevision := message.ExpectedStateRevision
		if message.SourceRevision != nil {
			sourceRevision = *message.SourceRevision
		}
		if err := structured.ReplyStructured(context.Background(), userID, projectID,
			message.InteractionID, response, sourceRevision); err != nil {
			connection.reply(errorReply{Type: "error", Code: CodeWSInteractionReplyFailed,
				Message: err.Error()})
		}
		return
	}
	if message.Approved == nil {
		connection.reply(errorReply{Type: "error", Code: CodeWSInteractionReplyFailed,
			Message: "当前服务只支持 confirm"})
		return
	}
	if err := g.replies.Reply(context.Background(), message.InteractionID,
		*message.Approved); err != nil {
		connection.reply(errorReply{Type: "error", Code: CodeWSInteractionReplyFailed,
			Message: err.Error()})
	}
}

func (g *StudioGateway) handleProjectSync(connection *conn, projectID string, afterSequence int64) {
	if afterSequence < 0 {
		connection.reply(errorReply{Type: "error", Code: CodeWSBadMessage,
			Message: "after_sequence 不能小于 0"})
		return
	}
	if g.syncer == nil {
		connection.reply(errorReply{Type: "error", Code: CodeWSSyncFailed,
			Message: "Project 事件补发服务未装配"})
		return
	}
	events, err := g.syncer.ReplayProjectEvents(projectID, afterSequence)
	if err != nil {
		connection.reply(errorReply{Type: "error", Code: CodeWSSyncFailed,
			Message: "增量存在缺口，请重新读取 Snapshot"})
		return
	}
	lastSequence := afterSequence
	for _, event := range events {
		connection.enqueue(event)
		if event.Sequence > lastSequence {
			lastSequence = event.Sequence
		}
	}
	connection.reply(studioSyncDoneReply{Type: "sync.done", Count: len(events),
		LastSequence: lastSequence})
}
