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
	"encoding/json"
	"net/http"
	"time"

	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/log"
)

// InteractionsHandler 是结构化交互记录（架构文档 12 §3）的只读 REST 处理器：
// 交互列表供前端审批卡恢复（status=pending）与历史查看。
type InteractionsHandler struct {
	// st 元数据存储。
	st *store.Store

	// logger 结构化日志器。
	logger *log.Logger
}

// NewInteractionsHandler 创建交互记录处理器。
func NewInteractionsHandler(st *store.Store, logger *log.Logger) *InteractionsHandler {
	return &InteractionsHandler{st: st, logger: logger}
}

// interactionView 是交互记录的响应视图（payload/reply 以 JSON 对象下发；
// checkpoint_id 属内核断点衔接字段，不下发）。
type interactionView struct {
	// ID 交互唯一标识。
	ID string `json:"id"`

	// ProjectID / SessionID 是交互的工作区归属。
	ProjectID string `json:"project_id"`
	SessionID string `json:"session_id"`

	// Revision 用于 Studio 忽略旧事件。
	Revision       int64           `json:"revision"`
	WorkflowID     string          `json:"workflow_id,omitempty"`
	TaskID         string          `json:"task_id,omitempty"`
	UIKind         string          `json:"ui_kind,omitempty"`
	SourceRevision int64           `json:"source_revision,omitempty"`
	TargetAgentID  string          `json:"target_agent_id,omitempty"`
	ResponseSchema json.RawMessage `json:"response_schema,omitempty"`
	MapID          string          `json:"map_id,omitempty"`
	MapGeneration  int64           `json:"map_generation,omitempty"`

	// Agent 发起交互的 Agent 名。
	Agent string `json:"agent"`

	// Type 交互类型（v1 仅 confirm）。
	Type string `json:"type"`

	// Status 交互状态（pending/answered/expired/cancelled）。
	Status string `json:"status"`

	// Payload 请求负载（question/risk/timeout_ts 等）。
	Payload json.RawMessage `json:"payload"`

	// Reply 应答负载；未应答为 null。
	Reply json.RawMessage `json:"reply"`

	// RunID 关联的 run session ID。
	RunID string `json:"run_id"`

	// CreatedAt 创建时间。
	CreatedAt time.Time `json:"created_at"`

	// AnsweredAt 应答时间；未应答为 null。
	AnsweredAt *time.Time `json:"answered_at"`

	// ExpiredAt 超时/取消时间；未发生为 null。
	ExpiredAt *time.Time `json:"expired_at"`

	// ExpiresAt 是结构化输入声明的截止时间，浏览器断开不会清除它。
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// HandleListInteractions 处理 GET /api/v1/interactions：分页查询交互记录
// （按创建时间倒序；session_id/status 精确过滤，status=pending 供审批卡恢复）。
func (h *InteractionsHandler) HandleListInteractions(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status != "" && !validInteractionStatus(status) {
		writeError(w, http.StatusBadRequest, CodeBadRequest,
			"status 参数非法（取值：pending/answered/expired/cancelled）")
		return
	}
	page, pageSize := parsePageLimit(r, maxListPageSize)
	projectID := r.URL.Query().Get("project_id")
	if projectID != "" {
		if err := h.st.ProjectOwnedByUser(auth.UserIDFromContext(r.Context()),
			projectID); err != nil {
			writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "Project 不存在")
			return
		}
	}

	items, total, err := h.st.ListInteractions(store.InteractionFilter{
		OwnerID:   auth.UserIDFromContext(r.Context()),
		ProjectID: projectID, SessionID: r.URL.Query().Get("session_id"),
		Status: status,
	}, pageSize, (page-1)*pageSize)
	if err != nil {
		h.logger.WithError(err).Error("查询交互记录失败")
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	views := make([]interactionView, 0, len(items))
	for _, it := range items {
		views = append(views, interactionView{
			ID: it.ID, ProjectID: it.ProjectID, SessionID: it.SessionID,
			Revision: it.Revision, Agent: it.Agent, Type: it.Type,
			WorkflowID: it.WorkflowID, TaskID: it.TaskID, UIKind: it.UIKind,
			SourceRevision: it.SourceRevision, TargetAgentID: it.TargetAgentID,
			ResponseSchema: rawJSON(it.ResponseSchema, "{}"), MapID: it.MapID,
			MapGeneration: it.MapGeneration,
			Status:        it.Status, Payload: rawJSON(it.Payload, "{}"), Reply: rawJSON(it.Reply, "null"),
			RunID: it.RunID, CreatedAt: it.CreatedAt, AnsweredAt: it.AnsweredAt,
			ExpiredAt: it.ExpiredAt, ExpiresAt: it.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"interactions": views, "page": page, "page_size": pageSize, "total": total,
	})
}

// validInteractionStatus 校验 status 查询参数是否合法状态机取值。
func validInteractionStatus(status string) bool {
	switch status {
	case store.InteractionStatusPending, store.InteractionStatusAnswered,
		store.InteractionStatusExpired, store.InteractionStatusCancelled:
		return true
	}
	return false
}
