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
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/install"
	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/log"
)

const maxProjectMemoryBytes = 1 << 20

// SessionInitializer 是创建 Conversation 后建立 Agent 模型快照的最小能力。
type SessionInitializer interface {
	InitializeSessionModels(sessionID string) error
}

// RunByIDController 按明确 Run ID 取消运行。不能用“取消 Conversation 当前
// Run”代替，否则旧请求可能误伤刚启动的新一轮。
type RunByIDController interface {
	CancelRunByID(ctx context.Context, userID, projectID, runID string) error
}

// SessionEvictor 在归档成功后移除 Conversation 的空闲进程内缓存。
// 未结束 Run 或待应答 Interaction 已由 Store 拒绝，不在归档操作里隐式停止。
type SessionEvictor interface {
	EvictSession(ctx context.Context, sessionID string) error
}

type ProjectSimulationLifecycle interface {
	ValidateRuntimePreference(profileID, installationID string) error
	ReleaseProject(context.Context, string) error
}

// ProjectsHandler 实现 v0.2 Project、Conversation、Run、Memory 和 Studio
// Snapshot 的公共接口。runtime 使用 any 保存，使 Store/API 分支可先独立合并；
// 精确取消未装配时明确返回 503，不伪造已经取消。
type ProjectsHandler struct {
	imports             *install.Inbox
	components          *install.ComponentStore
	componentRemoval    func(context.Context, install.InstalledComponent) error
	componentApply      func(context.Context, string, string) error
	st                  *store.Store
	runtime             any
	workflowApp         WorkflowApplication
	events              projectEventBus
	simulationLifecycle ProjectSimulationLifecycle
	logger              *log.Logger
}

// projectEventBus 是 HTTP 写操作使用的最小事件出口。事件仍统一进入现有
// Aggregator，由它分配 sequence、持久化并下发，Handler 不建立第二条事件链。
type projectEventBus interface {
	Publish(topic string, payload any)
}

// NewProjectsHandler 的 events 参数保持可选，便于不关心增量事件的旧单测继续
// 使用原装配；正式 Server 必须传入全局 Event Bus。
func NewProjectsHandler(st *store.Store, runtime any, logger *log.Logger,
	events ...projectEventBus) *ProjectsHandler {
	var publisher projectEventBus
	if len(events) > 0 {
		publisher = events[0]
	}
	return &ProjectsHandler{st: st, runtime: runtime, events: publisher, logger: logger}
}

// SetWorkflowApplication 显式装配 v0.3 Workflow 应用服务。它与 v0.2 的
// Conversation/Run runtime 分开，避免 Handler 把两类生命周期混为一体。
func (h *ProjectsHandler) SetWorkflowApplication(application WorkflowApplication) {
	h.workflowApp = application
}

func (h *ProjectsHandler) SetSimulationLifecycle(service ProjectSimulationLifecycle) {
	h.simulationLifecycle = service
}

type createProjectRequest struct {
	Name                           string `json:"name"`
	RuntimeProfileID               string `json:"runtime_profile_id,omitempty"`
	PreferredRuntimeInstallationID string `json:"preferred_runtime_installation_id,omitempty"`
}

type updateProjectRequest struct {
	Name     string `json:"name"`
	Revision int64  `json:"revision"`
}

type saveMemoryRequest struct {
	Content  string `json:"content"`
	Revision int64  `json:"revision"`
}

type archiveProjectRequest struct {
	Revision int64 `json:"revision"`
}

// HandleListProjects 处理 GET /api/v1/projects。
func (h *ProjectsHandler) HandleListProjects(w http.ResponseWriter, r *http.Request) {
	includeArchived, _ := strconv.ParseBool(r.URL.Query().Get("include_archived"))
	projects, err := h.st.ListProjects(auth.UserIDFromContext(r.Context()), includeArchived)
	if err != nil {
		h.internalError(w, "查询 Project 列表失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

// HandleCreateProject 处理 POST /api/v1/projects。
func (h *ProjectsHandler) HandleCreateProject(w http.ResponseWriter, r *http.Request) {
	var request createProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil ||
		strings.TrimSpace(request.Name) == "" {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "name 不能为空")
		return
	}
	request.RuntimeProfileID = strings.TrimSpace(request.RuntimeProfileID)
	request.PreferredRuntimeInstallationID = strings.TrimSpace(request.PreferredRuntimeInstallationID)
	if request.RuntimeProfileID != "" || request.PreferredRuntimeInstallationID != "" {
		if h.simulationLifecycle == nil {
			writeError(w, http.StatusServiceUnavailable, "SIMULATION_UNAVAILABLE", "仿真服务未装配")
			return
		}
		if err := h.simulationLifecycle.ValidateRuntimePreference(
			request.RuntimeProfileID, request.PreferredRuntimeInstallationID,
		); err != nil {
			writeError(w, http.StatusBadRequest, "RUNTIME_INSTALLATION_INVALID", err.Error())
			return
		}
	}
	project, err := h.st.CreateProjectWithRuntimePreference(
		auth.UserIDFromContext(r.Context()), request.Name,
		request.RuntimeProfileID, request.PreferredRuntimeInstallationID,
	)
	if err != nil {
		h.internalError(w, "创建 Project 失败", err)
		return
	}
	h.publishResourceEvent(project.ID, "", "project", project.ID, project.Revision,
		"project.created", map[string]any{"project": project}, ws.ParentRef{})
	writeJSON(w, http.StatusCreated, map[string]any{"project": project})
}

// HandleGetProject 处理 GET /api/v1/projects/{id}。
func (h *ProjectsHandler) HandleGetProject(w http.ResponseWriter, r *http.Request) {
	project, ok := h.ownedProject(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": project})
}

// HandleUpdateProject 处理 PATCH /api/v1/projects/{id}。v0.2 只允许改名；
// mode=running 不提供手工入口。
func (h *ProjectsHandler) HandleUpdateProject(w http.ResponseWriter, r *http.Request) {
	var request updateProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "请求体不是合法 JSON")
		return
	}
	project, err := h.st.UpdateProjectName(auth.UserIDFromContext(r.Context()),
		chi.URLParam(r, "id"), request.Name, request.Revision)
	if h.writeStoreError(w, err) {
		return
	}
	h.publishResourceEvent(project.ID, "", "project", project.ID, project.Revision,
		"project.updated", map[string]any{"project": project}, ws.ParentRef{})
	writeJSON(w, http.StatusOK, map[string]any{"project": project})
}

// HandleArchiveProject 处理 DELETE /api/v1/projects/{id}。revision 可放在
// JSON body 或 query 中，便于不支持 DELETE body 的客户端调用。
func (h *ProjectsHandler) HandleArchiveProject(w http.ResponseWriter, r *http.Request) {
	revision, _ := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
	if revision == 0 && r.Body != nil {
		var request archiveProjectRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil &&
			!errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "请求体不是合法 JSON")
			return
		}
		revision = request.Revision
	}
	if revision <= 0 {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "revision 必须大于 0")
		return
	}
	userID := auth.UserIDFromContext(r.Context())
	previous, previousErr := h.st.GetProject(chi.URLParam(r, "id"))
	// 若用户直接以普通 Project 开始工作，此时可能尚未创建 Default Project。
	// 归档前幂等补齐它，活动项目归档后始终有可回到的工作区。
	if _, err := h.st.EnsureDefaultProject(userID); err != nil {
		h.internalError(w, "准备 Default Project 失败", err)
		return
	}
	project, err := h.st.ArchiveProject(userID, chi.URLParam(r, "id"), revision)
	if h.writeStoreError(w, err) {
		return
	}
	h.publishResourceEvent(project.ID, "", "project", project.ID, project.Revision,
		"project.archived", map[string]any{"project": project}, ws.ParentRef{})
	// 场景可以在离开工作区后继续运行，归档时也要清理非活动 Project 的实例。
	if previousErr == nil && h.simulationLifecycle != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		if releaseErr := h.simulationLifecycle.ReleaseProject(ctx, previous.ID); releaseErr != nil {
			h.logger.WithError(releaseErr).Warn("归档 Project 后清理仿真失败", "project_id", previous.ID)
		}
		cancel()
	}
	if previousErr == nil && previous.IsActive {
		if replacement, getErr := h.st.GetActiveProject(userID); getErr == nil {
			h.publishResourceEvent(replacement.ID, "", "project", replacement.ID,
				replacement.Revision, "project.activated",
				map[string]any{"project": replacement}, ws.ParentRef{})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": project})
}

// HandleActivateProject 处理 POST /api/v1/projects/{id}/activate。
func (h *ProjectsHandler) HandleActivateProject(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	previous, previousErr := h.st.GetActiveProject(userID)
	project, err := h.st.ActivateProject(userID,
		chi.URLParam(r, "id"))
	if h.writeStoreError(w, err) {
		return
	}
	if previousErr == nil && previous.ID != project.ID {
		if deactivated, getErr := h.st.GetProject(previous.ID); getErr == nil {
			h.publishResourceEvent(deactivated.ID, "", "project", deactivated.ID,
				deactivated.Revision, "project.deactivated",
				map[string]any{"project": deactivated}, ws.ParentRef{})
		}
		h.publishResourceEvent(project.ID, "", "project", project.ID, project.Revision,
			"project.activated", map[string]any{"project": project}, ws.ParentRef{})
	}
	// 切换活动 Project 不再隐式销毁原场景；停止、归档和 Server shutdown
	// 仍走各自的资源清理链路。Store 对未结束 Workflow / Run 的切换检查
	// 保持不变，这里不把单活动 Project 扩展成多 Project 并发调度。
	writeJSON(w, http.StatusOK, map[string]any{"project": project})
}

// HandleGetMemory 处理 GET /api/v1/projects/{id}/memory。
func (h *ProjectsHandler) HandleGetMemory(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.ownedProject(w, r); !ok {
		return
	}
	memory, err := h.st.GetProjectMemory(chi.URLParam(r, "id"))
	if err != nil {
		h.internalError(w, "读取 Project Memory 失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"memory": memory})
}

// HandleSaveMemory 处理 PUT /api/v1/projects/{id}/memory。
func (h *ProjectsHandler) HandleSaveMemory(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxProjectMemoryBytes)
	var request saveMemoryRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest,
			"Memory 必须是合法 JSON，内容不能超过 1MB")
		return
	}
	memory, err := h.st.SaveProjectMemory(auth.UserIDFromContext(r.Context()),
		chi.URLParam(r, "id"), request.Content, request.Revision)
	if h.writeStoreError(w, err) {
		return
	}
	h.publishResourceEvent(memory.ProjectID, "", "project_memory", memory.ProjectID,
		memory.Revision, "memory.updated", map[string]any{"memory": memory}, ws.ParentRef{})
	writeJSON(w, http.StatusOK, map[string]any{"memory": memory})
}

// HandleListConversations 处理 GET /api/v1/projects/{id}/conversations。
func (h *ProjectsHandler) HandleListConversations(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.ownedProject(w, r); !ok {
		return
	}
	includeArchived, _ := strconv.ParseBool(r.URL.Query().Get("include_archived"))
	sessions, err := h.st.ListChatSessionsByProjectIncludingArchived(
		chi.URLParam(r, "id"), includeArchived)
	if err != nil {
		h.internalError(w, "查询 Conversation 失败", err)
		return
	}
	views := make([]sessionView, 0, len(sessions))
	for _, session := range sessions {
		views = append(views, toSessionView(session))
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": views})
}

// HandleArchiveConversation 处理
// DELETE /api/v1/projects/{id}/conversations/{conversation_id}。消息、Run、
// Interaction 与 Trace 都继续保留；列表只隐藏该 Conversation。
func (h *ProjectsHandler) HandleArchiveConversation(w http.ResponseWriter, r *http.Request) {
	project, ok := h.ownedProject(w, r)
	if !ok {
		return
	}
	sessionID := chi.URLParam(r, "conversation_id")
	session, err := h.st.GetChatSession(sessionID)
	if err != nil || session.ProjectID != project.ID || session.UserID != project.OwnerID ||
		session.ArchivedAt != nil {
		writeError(w, http.StatusNotFound, CodeSessionNotFound, "Conversation 不存在")
		return
	}
	archived, err := h.st.ArchiveConversation(project.OwnerID, project.ID, sessionID,
		time.Now().UTC())
	if h.writeStoreError(w, err) {
		return
	}
	if evictor, ok := h.runtime.(SessionEvictor); ok {
		evictCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := evictor.EvictSession(evictCtx, sessionID); err != nil {
			h.logger.WithError(err).Warn("Conversation 已归档但运行态缓存清理失败",
				"conversation_id", sessionID)
		}
	}
	view := toSessionView(archived)
	h.publishResourceEvent(project.ID, sessionID, "conversation", sessionID,
		archived.Revision, "conversation.archived", map[string]any{"conversation": view},
		ws.ParentRef{})
	writeJSON(w, http.StatusOK, map[string]any{"conversation": view})
}

// HandleCreateConversation 处理 POST /api/v1/projects/{id}/conversations。
func (h *ProjectsHandler) HandleCreateConversation(w http.ResponseWriter, r *http.Request) {
	project, ok := h.ownedProject(w, r)
	if !ok {
		return
	}
	if !project.IsActive {
		writeError(w, http.StatusConflict, "PROJECT_INACTIVE", "请先激活 Project")
		return
	}
	var request createSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "请求体不是合法 JSON")
		return
	}
	title := strings.TrimSpace(request.Title)
	if title == "" {
		title = defaultSessionTitle
	}
	now := time.Now().UTC()
	session := store.ChatSession{
		ID: store.NewChatSessionID(), UserID: project.OwnerID, ProjectID: project.ID,
		Title: title, CreatedAt: now, UpdatedAt: now,
	}
	if err := h.st.CreateChatSession(session); err != nil {
		if h.writeStoreError(w, err) {
			return
		}
		return
	}
	if initializer, ok := h.runtime.(SessionInitializer); ok {
		if err := h.st.WithWritableConversation(project.OwnerID, session.ID,
			func(_ store.ChatSession, _ store.Project) error {
				return initializer.InitializeSessionModels(session.ID)
			}); err != nil {
			_ = h.st.DeleteChatSession(session.ID)
			if h.writeStoreError(w, err) {
				return
			}
			h.internalError(w, "初始化 Conversation 模型失败", err)
			return
		}
	}
	saved, err := h.st.GetChatSession(session.ID)
	if err != nil {
		h.internalError(w, "读取新建 Conversation 失败", err)
		return
	}
	view := toSessionView(saved)
	h.publishResourceEvent(saved.ProjectID, saved.ID, "conversation", saved.ID,
		saved.Revision, "conversation.created", map[string]any{"conversation": view},
		ws.ParentRef{})
	writeJSON(w, http.StatusCreated, map[string]any{
		"conversation": view,
	})
}

// HandleListRuns 处理 GET /api/v1/projects/{id}/runs。
func (h *ProjectsHandler) HandleListRuns(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.ownedProject(w, r); !ok {
		return
	}
	status := r.URL.Query().Get("status")
	if status != "" && !store.ValidRunStatus(status) {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "status 参数非法")
		return
	}
	page, pageSize := parsePageLimit(r, maxListPageSize)
	runs, total, err := h.st.ListRunSessions(store.RunFilter{
		ProjectID: chi.URLParam(r, "id"), ChatSessionID: r.URL.Query().Get("conversation_id"),
		Status: status,
	}, pageSize, (page-1)*pageSize)
	if err != nil {
		h.internalError(w, "查询 Run 列表失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"runs": runs, "page": page, "page_size": pageSize, "total": total,
	})
}

// HandleGetRun 处理 GET /api/v1/runs/{id}。
func (h *ProjectsHandler) HandleGetRun(w http.ResponseWriter, r *http.Request) {
	run, err := h.ownedRun(r)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "RUN_NOT_FOUND", "Run 不存在")
		return
	}
	if err != nil {
		h.internalError(w, "查询 Run 失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}

// HandleCancelRun 处理 POST /api/v1/runs/{id}/cancel。
func (h *ProjectsHandler) HandleCancelRun(w http.ResponseWriter, r *http.Request) {
	run, err := h.ownedRun(r)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "RUN_NOT_FOUND", "Run 不存在")
		return
	}
	if err != nil {
		h.internalError(w, "查询 Run 失败", err)
		return
	}
	controller, ok := h.runtime.(RunByIDController)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "RUN_CONTROL_UNAVAILABLE",
			"精确 Run 取消服务尚未装配")
		return
	}
	if err := controller.CancelRunByID(r.Context(),
		auth.UserIDFromContext(r.Context()), run.ProjectID, run.ID); err != nil {
		writeError(w, http.StatusConflict, "RUN_CANCEL_FAILED", err.Error())
		return
	}
	updated, err := h.st.GetRunSession(run.ID)
	if err != nil {
		h.internalError(w, "读取取消后的 Run 失败", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run": updated})
}

// HandleStudioSnapshot 处理 GET /api/v1/projects/{id}/studio/snapshot。
func (h *ProjectsHandler) HandleStudioSnapshot(w http.ResponseWriter, r *http.Request) {
	snapshot, err := h.st.BuildStudioSnapshot(auth.UserIDFromContext(r.Context()),
		chi.URLParam(r, "id"), 100)
	if h.writeStoreError(w, err) {
		return
	}
	conversations := make([]sessionView, 0, len(snapshot.Conversations))
	for _, session := range snapshot.Conversations {
		conversations = append(conversations, toSessionView(session))
	}
	interactions := make([]interactionView, 0, len(snapshot.PendingInteractions))
	for _, value := range snapshot.PendingInteractions {
		interactions = append(interactions, interactionView{
			ID: value.ID, ProjectID: value.ProjectID, SessionID: value.SessionID,
			Revision: value.Revision, Agent: value.Agent,
			Type: value.Type, Status: value.Status,
			WorkflowID: value.WorkflowID, TaskID: value.TaskID, UIKind: value.UIKind,
			SourceRevision: value.SourceRevision, TargetAgentID: value.TargetAgentID,
			ResponseSchema: rawJSON(value.ResponseSchema, "{}"), MapID: value.MapID,
			MapGeneration: value.MapGeneration,
			Payload:       rawJSON(value.Payload, "{}"), Reply: rawJSON(value.Reply, "null"),
			RunID: value.RunID, CreatedAt: value.CreatedAt,
			AnsweredAt: value.AnsweredAt, ExpiredAt: value.ExpiredAt, ExpiresAt: value.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": map[string]any{
		"snapshot_version": 1,
		"project":          snapshot.Project, "conversations": conversations,
		"runs": snapshot.Runs, "pending_interactions": interactions,
		"memory_revision": snapshot.MemoryRevision,
		"event_sequence":  snapshot.EventSequence, "captured_at": snapshot.CapturedAt,
	}})
}

// publishResourceEvent 将 HTTP 已提交的状态变化放入统一事件链。sequence 不在
// Handler 中计算，Aggregator 落库时按 Project 连续分配，避免响应层和 Store
// 各自维护游标。发布失败不会回滚已经提交的 HTTP 写入；客户端发现缺口时会
// 重新读取 Snapshot。
func (h *ProjectsHandler) publishResourceEvent(projectID, sessionID, resourceType,
	resourceID string, revision int64, eventType string, payload any, parent ws.ParentRef) {
	if h.events == nil {
		return
	}
	envelope := ws.NewEnvelope(sessionID, ws.ChannelDialogue, eventType,
		ws.ImportanceNormal, payload)
	envelope.ProjectID = projectID
	envelope.ResourceType = resourceType
	envelope.ResourceID = resourceID
	envelope.Revision = revision
	envelope.Parent = parent
	h.events.Publish(event.TopicAgentEvents, envelope)
}

func (h *ProjectsHandler) ownedProject(w http.ResponseWriter, r *http.Request) (store.Project, bool) {
	project, err := h.st.GetProject(chi.URLParam(r, "id"))
	if err != nil || project.OwnerID != auth.UserIDFromContext(r.Context()) {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "Project 不存在")
		return store.Project{}, false
	}
	if project.ArchivedAt != nil {
		writeError(w, http.StatusGone, "PROJECT_ARCHIVED", "Project 已归档")
		return store.Project{}, false
	}
	return project, true
}

func (h *ProjectsHandler) ownedRun(r *http.Request) (store.RunSession, error) {
	run, err := h.st.GetRunSession(chi.URLParam(r, "id"))
	if err != nil {
		return store.RunSession{}, err
	}
	if err := h.st.ProjectOwnedByUser(auth.UserIDFromContext(r.Context()),
		run.ProjectID); err != nil {
		return store.RunSession{}, store.ErrNotFound
	}
	return run, nil
}

// writeStoreError 写出预期业务错误，返回 true 表示已经处理。
func (h *ProjectsHandler) writeStoreError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "Project 不存在")
	case errors.Is(err, store.ErrRevisionConflict):
		writeError(w, http.StatusConflict, "REVISION_CONFLICT", "内容已更新，请重新读取")
	case errors.Is(err, store.ErrProjectInactive):
		writeError(w, http.StatusConflict, "PROJECT_INACTIVE", "请先激活 Project")
	case errors.Is(err, store.ErrProjectArchived):
		writeError(w, http.StatusGone, "PROJECT_ARCHIVED", "Project 已归档")
	case errors.Is(err, store.ErrDefaultProject):
		writeError(w, http.StatusConflict, "DEFAULT_PROJECT", "Default Project 不能归档")
	case errors.Is(err, store.ErrProjectHasActiveWork):
		writeError(w, http.StatusConflict, "PROJECT_HAS_ACTIVE_WORK",
			"Project 仍有未结束 Run 或待应答 Interaction，请先取消或应答")
	case errors.Is(err, store.ErrConversationHasActiveWork):
		writeError(w, http.StatusConflict, "CONVERSATION_HAS_ACTIVE_WORK",
			"Conversation 仍有未结束 Run 或待应答 Interaction，请先停止或应答")
	case errors.Is(err, store.ErrInvalidState):
		writeError(w, http.StatusConflict, "INVALID_STATE", "当前状态不允许执行此操作")
	default:
		h.internalError(w, "Project 操作失败", err)
	}
	return true
}

func (h *ProjectsHandler) internalError(w http.ResponseWriter, message string, err error) {
	h.logger.WithError(err).Error(message)
	writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
}
