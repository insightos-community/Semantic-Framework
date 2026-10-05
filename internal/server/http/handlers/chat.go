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

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/agent/runtime"
	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/log"
)

// 对话域的错误码，与 HTTP 响应体 error.code 一致。
const (
	// CodeBadRequest 请求体或参数非法。
	CodeBadRequest = "BAD_REQUEST"

	// CodeSessionNotFound 会话不存在或不属于当前用户。
	CodeSessionNotFound = "CHAT_SESSION_NOT_FOUND"

	// CodeSessionBusy 会话存在活动模型或工具运行，暂时不能切换模型。
	CodeSessionBusy = "SESSION_BUSY"

	// CodeInternal 服务内部错误。
	CodeInternal = "INTERNAL_ERROR"
)

const maxImageAttachmentBytes = 20 << 20

// maxWorkspaceArtifactBytes 限制从 Project workspace 显式登记的单文件大小。
const maxWorkspaceArtifactBytes = 100 << 20

// defaultSessionTitle 是创建会话未给标题时的默认文案。
const defaultSessionTitle = "新会话"

// defaultPageSize / maxPageSize 是消息分页的默认与上限。
const (
	defaultPageSize = 50
	maxPageSize     = 200
)

// maxListPageSize 是观测类只读列表端点（traces/metering/interactions）的
// page_size 上限：严于消息分页，聚合/明细视图无单页大列表需求。
const maxListPageSize = 100

// ChatHandler 是对话域的 REST 处理器：会话 CRUD 与消息查询。
// 全部端点经 auth 中间件注入的 user_id 做归属过滤——只能看到本人的会话。
type ChatHandler struct {
	// st 元数据存储。
	st *store.Store

	// rt AgentRuntime：删除会话时剔除进程内运行态缓存。
	rt *runtime.Service

	// events 复用 Project 事件总线。旧 /chat 路由仍处于迁移期，但它创建或
	// 归档 Conversation 时也必须通知 Studio，不能要求其他连接等待刷新。
	events projectEventBus

	// logger 结构化日志器。
	logger *log.Logger
}

// NewChatHandler 创建对话域处理器。
func NewChatHandler(st *store.Store, rt *runtime.Service, logger *log.Logger,
	events ...projectEventBus) *ChatHandler {
	var publisher projectEventBus
	if len(events) > 0 {
		publisher = events[0]
	}
	return &ChatHandler{st: st, rt: rt, events: publisher, logger: logger}
}

// createSessionRequest 是 POST /chat/sessions 的请求体（全部字段可选）。
type createSessionRequest struct {
	// Title 会话标题；为空时用默认文案。
	Title string `json:"title"`

	// ProjectID 会话所属 Project；为空时使用当前用户 Default Project。
	ProjectID string `json:"project_id"`
}

// sessionView 是会话的响应视图。
type sessionView struct {
	ID         string     `json:"id"`
	ProjectID  string     `json:"project_id"`
	Title      string     `json:"title"`
	ArchivedAt *time.Time `json:"archived_at"`
	Revision   int64      `json:"revision"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// messageView 是消息的响应视图。
type messageView struct {
	ID           string          `json:"id"`
	Role         string          `json:"role"`
	Content      string          `json:"content"`
	AgentID      string          `json:"agent_id,omitempty"`
	RunID        string          `json:"run_id,omitempty"`
	TraceID      string          `json:"trace_id,omitempty"`
	Provider     string          `json:"provider,omitempty"`
	Endpoint     string          `json:"endpoint,omitempty"`
	Model        string          `json:"model,omitempty"`
	ArtifactRefs []string        `json:"artifact_refs,omitempty"`
	Metadata     json.RawMessage `json:"metadata"`
	CreatedAt    time.Time       `json:"created_at"`
	StartedAt    *time.Time      `json:"started_at,omitempty"`
}

type attachmentView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	MediaType  string `json:"media_type"`
	Size       int64  `json:"size"`
	ContentURL string `json:"content_url"`
}

// artifactView 是 Artifact 资源面的元数据视图，不内联文件本体。
type artifactView struct {
	ID         string          `json:"id"`
	URI        string          `json:"uri"`
	MediaType  string          `json:"media_type"`
	Summary    string          `json:"summary"`
	Size       int64           `json:"size"`
	Metadata   json.RawMessage `json:"metadata"`
	ContentURL string          `json:"content_url"`
	CreatedAt  time.Time       `json:"created_at"`
}

// registerWorkspaceArtifactRequest 指定要从 Project workspace 复制登记的
// 文件；path 必须是相对工作区根目录的路径。
type registerWorkspaceArtifactRequest struct {
	ProjectID string `json:"project_id"`
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	Summary   string `json:"summary"`
}

// updateHostExecutionRequest 是会话执行策略请求。mode 与宿主开关一起提交，
// 避免两次请求之间出现 Runner 使用半更新策略的窗口。
type updateHostExecutionRequest struct {
	Mode    string `json:"mode"`
	Enabled bool   `json:"enabled"`
}

// toSessionView 转换存储模型为响应视图（user_id 不下发——调用方即归属人）。
func toSessionView(s store.ChatSession) sessionView {
	return sessionView{ID: s.ID, ProjectID: s.ProjectID, Title: s.Title,
		ArchivedAt: s.ArchivedAt, Revision: s.Revision,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}
}

// publishConversationEvent 让迁移期 /chat 写入口与 Project 公共接口产生同形
// Envelope。sequence 仍由 Aggregator 分配，Handler 只提供资源与 revision。
func (h *ChatHandler) publishConversationEvent(session store.ChatSession, typ string) {
	if h.events == nil {
		return
	}
	view := toSessionView(session)
	envelope := ws.NewEnvelope(session.ID, ws.ChannelDialogue, typ,
		ws.ImportanceNormal, map[string]any{"conversation": view})
	envelope.ProjectID = session.ProjectID
	envelope.ResourceType = "conversation"
	envelope.ResourceID = session.ID
	envelope.Revision = session.Revision
	h.events.Publish(event.TopicAgentEvents, envelope)
}

// HandleCreateSession 处理 POST /api/v1/chat/sessions：创建会话。
func (h *ChatHandler) HandleCreateSession(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())

	var req createSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "请求体不是合法 JSON")
		return
	}
	title := req.Title
	if title == "" {
		title = defaultSessionTitle
	}

	if req.ProjectID == "" {
		project, err := h.st.GetActiveProject(userID)
		if errors.Is(err, store.ErrNotFound) {
			project, err = h.st.EnsureDefaultProject(userID)
			if err == nil && !project.IsActive {
				project, err = h.st.ActivateProject(userID, project.ID)
			}
		}
		if err != nil {
			h.logger.WithError(err).Error("解析活动 Project 失败", "user_id", userID)
			writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
			return
		}
		req.ProjectID = project.ID
	}
	now := time.Now().UTC()
	sess := store.ChatSession{
		ID: store.NewChatSessionID(), UserID: userID, ProjectID: req.ProjectID,
		Title: title, CreatedAt: now, UpdatedAt: now,
	}
	if err := h.st.CreateChatSession(sess); err != nil {
		h.logger.WithError(err).Error("创建会话失败", "user_id", userID)
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	if err := h.rt.InitializeSessionModels(sess.ID); err != nil {
		// 会话与 Agent 模型快照必须作为一个完整结果暴露给客户端。快照
		// 初始化失败时删除刚创建的空会话，不留下行为不确定的数据。
		_ = h.st.DeleteChatSession(sess.ID)
		h.logger.WithError(err).Error("初始化会话 Agent 模型失败", "session_id", sess.ID)
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	sess, err := h.st.GetChatSession(sess.ID)
	if err != nil {
		h.logger.WithError(err).Error("读取新建会话失败", "session_id", sess.ID)
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	h.logger.Info("会话已创建", "session_id", sess.ID, "user_id", userID)
	h.publishConversationEvent(sess, "conversation.created")
	writeJSON(w, http.StatusCreated, map[string]any{"session": toSessionView(sess)})
}

// HandleListSessions 处理 GET /api/v1/chat/sessions：列出本人的全部会话
// （按最近活跃倒序）。
func (h *ChatHandler) HandleListSessions(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())

	sessions, err := h.st.ListChatSessionsByUser(userID)
	if err != nil {
		h.logger.WithError(err).Error("查询会话列表失败", "user_id", userID)
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	views := make([]sessionView, 0, len(sessions))
	for _, s := range sessions {
		views = append(views, toSessionView(s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": views})
}

// HandleGetHostExecution 返回会话执行模式、宿主开关和服务端硬开关。
func (h *ChatHandler) HandleGetHostExecution(w http.ResponseWriter, r *http.Request) {
	policy, err := h.rt.GetSessionExecutionPolicy(auth.UserIDFromContext(r.Context()), chi.URLParam(r, "id"))
	if errors.Is(err, runtime.ErrSessionNotFound) {
		writeError(w, http.StatusNotFound, CodeSessionNotFound, "会话不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"execution": policy})
}

// HandleUpdateHostExecution 更新当前会话的最小执行策略。活动 Run 存在时
// 返回 409，禁止在一轮工具调用中途改变审批和可见性。
func (h *ChatHandler) HandleUpdateHostExecution(w http.ResponseWriter, r *http.Request) {
	var req updateHostExecutionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "请求体不是合法 JSON")
		return
	}
	if req.Mode == "" {
		req.Mode = store.ExecutionModeAsk
	}
	policy, err := h.rt.SetSessionExecutionPolicy(auth.UserIDFromContext(r.Context()),
		chi.URLParam(r, "id"), req.Mode, req.Enabled)
	if writeConversationWriteError(w, err) {
		return
	}
	switch {
	case errors.Is(err, runtime.ErrSessionBusy):
		writeError(w, http.StatusConflict, CodeSessionBusy, "会话正在运行，空闲后才能修改执行策略")
	case err != nil:
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"execution": policy})
	}
}

// HandleListMessages 处理 GET /api/v1/chat/sessions/{id}/messages：
// 分页查询会话消息（page/page_size，按时间升序）。
func (h *ChatHandler) HandleListMessages(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	sessionID := chi.URLParam(r, "id")

	if _, err := h.ownedSession(sessionID, userID); err != nil {
		writeError(w, http.StatusNotFound, CodeSessionNotFound, "会话不存在")
		return
	}

	page, pageSize := parsePage(r)
	messages, err := h.st.ListChatMessages(sessionID, pageSize, (page-1)*pageSize)
	if err != nil {
		h.logger.WithError(err).Error("查询会话消息失败", "session_id", sessionID)
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	total, err := h.st.CountChatMessages(sessionID)
	if err != nil {
		h.logger.WithError(err).Error("统计会话消息失败", "session_id", sessionID)
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}

	views := make([]messageView, 0, len(messages))
	for _, m := range messages {
		if m.Message == nil {
			h.logger.Error("会话消息缺少 schema.Message", "session_id", sessionID, "message_id", m.ID)
			writeError(w, http.StatusInternalServerError, CodeInternal, "会话消息数据损坏")
			return
		}
		views = append(views, messageView{
			ID: m.ID, Role: string(m.Message.Role), Content: m.Message.Content,
			AgentID: m.AgentID, RunID: m.RunID, TraceID: m.TraceID,
			Provider: m.Provider, Endpoint: m.Endpoint, Model: m.Model,
			ArtifactRefs: m.ArtifactRefs, Metadata: rawJSON(m.Metadata, "{}"), CreatedAt: m.CreatedAt, StartedAt: m.StartedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"messages": views, "page": page, "page_size": pageSize, "total": total,
	})
}

// HandleDeleteSession 处理 DELETE /api/v1/chat/sessions/{id}：
// 归档会话并剔除运行时缓存；消息、Run、Interaction 与 Trace 继续保留。
func (h *ChatHandler) HandleDeleteSession(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	sessionID := chi.URLParam(r, "id")

	if _, err := h.ownedSession(sessionID, userID); err != nil {
		writeError(w, http.StatusNotFound, CodeSessionNotFound, "会话不存在")
		return
	}
	evictCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := h.rt.EvictSession(evictCtx, sessionID); err != nil {
		h.logger.WithError(err).Warn("会话运行尚未结束，取消删除", "session_id", sessionID)
		writeError(w, http.StatusConflict, CodeBadRequest, "会话正在结束，请稍后重试删除")
		return
	}
	session, _ := h.st.GetChatSession(sessionID)
	if err := h.st.ArchiveChatSession(userID, session.ProjectID, sessionID,
		time.Now().UTC()); err != nil {
		h.logger.WithError(err).Error("归档会话失败", "session_id", sessionID)
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	archived, err := h.st.GetChatSession(sessionID)
	if err != nil {
		h.logger.WithError(err).Error("读取已归档会话失败", "session_id", sessionID)
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	h.publishConversationEvent(archived, "conversation.archived")
	h.logger.Info("会话已归档", "session_id", sessionID, "user_id", userID)
	w.WriteHeader(http.StatusNoContent)
}

// HandleUploadAttachment 接收对话图片，内容进入 artifact store，消息只保存引用。
func (h *ChatHandler) HandleUploadAttachment(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, maxImageAttachmentBytes+(1<<20))
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "请选择图片文件")
		return
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, maxImageAttachmentBytes+1))
	if err != nil || len(content) > maxImageAttachmentBytes {
		writeError(w, http.StatusRequestEntityTooLarge, CodeBadRequest, "图片不能超过 20MB")
		return
	}
	mediaType := http.DetectContentType(content)
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	default:
		writeError(w, http.StatusBadRequest, CodeBadRequest, "仅支持 JPEG、PNG、GIF、WebP 图片")
		return
	}
	name := filepath.Base(header.Filename)
	meta, _ := json.Marshal(map[string]string{"filename": name, "source": "chat_upload"})
	artifact, err := h.st.PutUserArtifact(userID, mediaType, name, string(meta), content)
	if err != nil {
		h.logger.WithError(err).Error("上传对话图片失败", "user_id", userID)
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"attachment": attachmentView{
		ID: artifact.ID, Name: name, MediaType: mediaType, Size: artifact.Size,
		ContentURL: "/api/v1/chat/attachments/" + artifact.ID,
	}})
}

// HandleGetAttachment 鉴权返回本人上传的图片本体。
func (h *ChatHandler) HandleGetAttachment(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	artifact, content, err := h.st.GetArtifact(id)
	if err != nil || artifact.OwnerID != userID {
		writeError(w, http.StatusNotFound, CodeSessionNotFound, "图片不存在")
		return
	}
	w.Header().Set("Content-Type", artifact.MediaType)
	w.Header().Set("Content-Length", fmt.Sprint(len(content)))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// HandleRegisterWorkspaceArtifact 把工作区现有文件显式复制到 Artifact Store。
// 普通 execute 输出不会自动走此路径，只有用户或 Agent 明确要求稳定引用时登记。
func (h *ChatHandler) HandleRegisterWorkspaceArtifact(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	var req registerWorkspaceArtifactRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ProjectID == "" || strings.TrimSpace(req.Path) == "" {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "project_id 和 path 不能为空")
		return
	}
	project, err := h.st.GetProject(req.ProjectID)
	if err != nil || project.OwnerID != userID {
		writeError(w, http.StatusNotFound, CodeSessionNotFound, "Project 不存在")
		return
	}
	absPath, relativePath, err := tool.ResolveWorkspacePath(project.WorkspaceRoot, req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	info, err := os.Stat(absPath)
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "只能登记 Project workspace 内的普通文件")
		return
	}
	if info.Size() > maxWorkspaceArtifactBytes {
		writeError(w, http.StatusRequestEntityTooLarge, CodeBadRequest, "Artifact 文件不能超过 100MB")
		return
	}
	content, err := os.ReadFile(absPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "读取工作区文件失败")
		return
	}
	mediaType := strings.TrimSpace(req.MediaType)
	if mediaType == "" {
		mediaType = mime.TypeByExtension(filepath.Ext(absPath))
	}
	if mediaType == "" {
		mediaType = http.DetectContentType(content)
	}
	summary := strings.TrimSpace(req.Summary)
	if summary == "" {
		summary = filepath.Base(absPath)
	}
	metadata, _ := json.Marshal(map[string]string{
		"source": "project_workspace", "project_id": project.ID, "workspace_path": relativePath,
	})
	artifact, err := h.st.PutUserArtifact(userID, mediaType, summary, string(metadata), content)
	if err != nil {
		h.logger.WithError(err).Error("登记工作区 Artifact 失败", "project_id", project.ID, "path", relativePath)
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"artifact": toArtifactView(artifact)})
}

// HandleListArtifacts 返回当前用户显式登记或上传的 Artifact 元数据。
func (h *ChatHandler) HandleListArtifacts(w http.ResponseWriter, r *http.Request) {
	page, pageSize := parsePage(r)
	artifacts, err := h.st.ListArtifactsByOwner(auth.UserIDFromContext(r.Context()),
		pageSize, (page-1)*pageSize)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	views := make([]artifactView, 0, len(artifacts))
	for _, artifact := range artifacts {
		views = append(views, toArtifactView(artifact))
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": views, "page": page, "page_size": pageSize})
}

// HandleDeleteArtifact 删除当前用户的 Artifact。已有消息引用时默认拒绝；
// force=true 只删除本体，历史消息仍保留引用并由前端显示缺失状态。
func (h *ChatHandler) HandleDeleteArtifact(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	artifact, err := h.st.GetArtifactMeta(id)
	if err != nil || artifact.OwnerID != userID {
		writeError(w, http.StatusNotFound, CodeSessionNotFound, "Artifact 不存在")
		return
	}
	refs, err := h.st.ArtifactReferenceCount(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	force, _ := strconv.ParseBool(r.URL.Query().Get("force"))
	if refs > 0 && !force {
		writeError(w, http.StatusConflict, "ARTIFACT_REFERENCED",
			fmt.Sprintf("Artifact 仍被 %d 条消息引用；确认强制删除后历史将显示缺失", refs))
		return
	}
	if err := h.st.DeleteArtifact(id); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// toArtifactView 转换 Artifact 元数据并提供统一内容读取地址。
func toArtifactView(artifact store.Artifact) artifactView {
	return artifactView{ID: artifact.ID, URI: artifact.URI, MediaType: artifact.MediaType,
		Summary: artifact.Summary, Size: artifact.Size, Metadata: rawJSON(artifact.Metadata, "{}"),
		ContentURL: "/api/v1/chat/artifacts/" + artifact.ID, CreatedAt: artifact.CreatedAt}
}

// ownedSession 校验会话存在且归属当前用户；任一不符统一返回"不存在"，
// 不暴露会话存在性。
func (h *ChatHandler) ownedSession(sessionID, userID string) (store.ChatSession, error) {
	sess, err := h.st.GetChatSession(sessionID)
	if err != nil {
		return store.ChatSession{}, err
	}
	if sess.UserID != userID {
		return store.ChatSession{}, store.ErrNotFound
	}
	return sess, nil
}

// parsePage 解析分页参数：page 从 1 开始，page_size 默认 50、上限 200。
func parsePage(r *http.Request) (page, pageSize int) {
	return parsePageLimit(r, maxPageSize)
}

// parsePageLimit 解析分页参数并按给定上限收敛 page_size（消息分页用
// maxPageSize，观测类只读列表端点用 maxListPageSize）。
func parsePageLimit(r *http.Request, max int) (page, pageSize int) {
	page, _ = strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ = strconv.Atoi(r.URL.Query().Get("page_size"))
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	if pageSize > max {
		pageSize = max
	}
	return page, pageSize
}

// writeError 以统一格式 {"error":{"code","message"}} 写出错误响应。
// 与 internal/server/http.WriteError 的线上格式一致；本包不引用 http 包
// 是为了避免 http 路由装配依赖本包时形成包循环（与 auth 包的先例一致）。
func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": msg},
	})
}

// writeJSON 以 JSON 格式写出正常响应体。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// rawJSON 把库存 JSON 文本转为可内嵌响应的 RawMessage；空串或非法 JSON
// 回落为 fallback（脏数据不拖垮整个响应）。
func rawJSON(s, fallback string) json.RawMessage {
	if s != "" && json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	return json.RawMessage(fallback)
}
