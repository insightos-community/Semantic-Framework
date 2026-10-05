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
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/store"
)

// WorkflowApplication 是 HTTP 层进入 Plan、修订计划以及控制执行所需的最小能力。
// 这些操作会启动 Planning Run、Scheduler 或取消活动 Run，Handler 不能只改 Store。
type WorkflowApplication interface {
	ApprovePlanProposal(ctx context.Context, userID, projectID, proposalID string,
		expectedRevision int64) (store.WorkflowView, error)
	DiscardPlanProposal(userID, projectID, proposalID string,
		expectedRevision int64) (store.PlanProposal, error)
	PauseWorkflow(ctx context.Context, userID, projectID, workflowID string,
		expectedRevision int64) (store.WorkflowView, error)
	ResumeWorkflow(ctx context.Context, userID, projectID, workflowID string,
		expectedRevision int64) (store.WorkflowView, error)
	RetryRobotAgentDecision(ctx context.Context, userID, projectID, workflowID string,
		expectedRevision int64) (store.WorkflowView, error)
	StopWorkflow(ctx context.Context, userID, projectID, workflowID string,
		expectedRevision int64) (store.WorkflowView, error)
	ConfirmWorkflowStop(ctx context.Context, userID, projectID, workflowID string,
		expectedRevision int64, physicalStateConfirmed bool, reason string) (store.WorkflowView, error)
}

type workflowRevisionRequest struct {
	Revision               int64  `json:"revision"`
	ExpectedRevision       int64  `json:"expected_revision"`
	PhysicalStateConfirmed bool   `json:"physical_state_confirmed"`
	Reason                 string `json:"reason"`
}

func (r workflowRevisionRequest) revision() int64 {
	if r.ExpectedRevision != 0 {
		return r.ExpectedRevision
	}
	return r.Revision
}

// HandleListWorkflows 处理 Project 内 Workflow 列表。
func (h *ProjectsHandler) HandleListWorkflows(w http.ResponseWriter, r *http.Request) {
	project, ok := h.ownedProject(w, r)
	if !ok {
		return
	}
	includeEnded, _ := strconv.ParseBool(r.URL.Query().Get("include_ended"))
	items, err := h.st.ListWorkflows(project.ID, includeEnded)
	if h.writeV030Error(w, err, "读取 Workflow 失败") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": items})
}

// HandleGetActiveWorkflow 返回 Project 当前未结束 Workflow；没有时返回 null。
func (h *ProjectsHandler) HandleGetActiveWorkflow(w http.ResponseWriter, r *http.Request) {
	project, ok := h.ownedProject(w, r)
	if !ok {
		return
	}
	items, err := h.st.ListWorkflows(project.ID, false)
	if h.writeV030Error(w, err, "读取当前 Workflow 失败") {
		return
	}
	if len(items) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"workflow_view": nil})
		return
	}
	view, err := h.st.GetWorkflowView(items[0].ID)
	if h.writeV030Error(w, err, "读取当前 Workflow 失败") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflow_view": view})
}

// HandleGetWorkflowView 返回计划、Task、SubTask 与依赖的完整只读视图。
func (h *ProjectsHandler) HandleGetWorkflowView(w http.ResponseWriter, r *http.Request) {
	project, ok := h.ownedProject(w, r)
	if !ok {
		return
	}
	workflow, ok := h.ownedWorkflow(w, project.ID, chi.URLParam(r, "workflow_id"))
	if !ok {
		return
	}
	view, err := h.st.GetWorkflowView(workflow.ID)
	if h.writeV030Error(w, err, "读取 Workflow 详情失败") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflow_view": view})
}

// HandleWorkflowAction 处理 confirm/discard/pause/resume/stop。
func (h *ProjectsHandler) HandleWorkflowAction(w http.ResponseWriter, r *http.Request) {
	project, ok := h.ownedProject(w, r)
	if !ok {
		return
	}
	if !h.writableProject(w, r, project.ID) {
		return
	}
	workflow, ok := h.ownedWorkflow(w, project.ID, chi.URLParam(r, "workflow_id"))
	if !ok {
		return
	}
	var request workflowRevisionRequest
	if !decodeV030JSON(w, r, &request) {
		return
	}
	revision := request.revision()
	if revision <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "INVALID_REVISION", "revision 必须大于 0")
		return
	}
	action := chi.URLParam(r, "action")
	application := h.workflowApp
	if application == nil {
		writeError(w, http.StatusServiceUnavailable, "WORKFLOW_SERVICE_UNAVAILABLE", "Workflow 服务尚未装配")
		return
	}
	var (
		view store.WorkflowView
		err  error
	)
	userID := auth.UserIDFromContext(r.Context())
	switch action {
	case "pause":
		view, err = application.PauseWorkflow(r.Context(), userID, project.ID, workflow.ID, revision)
	case "resume":
		view, err = application.ResumeWorkflow(r.Context(), userID, project.ID, workflow.ID, revision)
	case "retry-decision":
		view, err = application.RetryRobotAgentDecision(r.Context(), userID, project.ID, workflow.ID, revision)
	case "stop":
		view, err = application.StopWorkflow(r.Context(), userID, project.ID, workflow.ID, revision)
	case "confirm-stop":
		if !request.PhysicalStateConfirmed || strings.TrimSpace(request.Reason) == "" {
			writeError(w, http.StatusUnprocessableEntity, "PHYSICAL_CONFIRMATION_REQUIRED",
				"必须确认现场物理状态安全并填写原因")
			return
		}
		view, err = application.ConfirmWorkflowStop(
			r.Context(), userID, project.ID, workflow.ID, revision,
			request.PhysicalStateConfirmed, request.Reason,
		)
	default:
		writeError(w, http.StatusNotFound, "ACTION_NOT_FOUND", "Workflow 操作不存在")
		return
	}
	if h.writeV030Error(w, err, "Workflow 状态更新失败") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflow_view": view})
}

func (h *ProjectsHandler) writableProject(w http.ResponseWriter, r *http.Request, projectID string) bool {
	if err := h.st.ProjectWritableByUser(auth.UserIDFromContext(r.Context()), projectID); err != nil {
		h.writeV030Error(w, err, "Project 当前不可写")
		return false
	}
	return true
}

func (h *ProjectsHandler) ownedWorkflow(w http.ResponseWriter, projectID, workflowID string) (store.Workflow, bool) {
	workflow, err := h.st.GetWorkflow(workflowID)
	if err != nil || workflow.ProjectID != projectID {
		writeError(w, http.StatusNotFound, "WORKFLOW_NOT_FOUND", "Workflow 不存在")
		return store.Workflow{}, false
	}
	return workflow, true
}

func decodeV030JSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "请求体不是合法 JSON")
		return false
	}
	return true
}

func (h *ProjectsHandler) writeV030Error(w http.ResponseWriter, err error, message string) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "资源不存在")
	case errors.Is(err, store.ErrRevisionConflict):
		writeError(w, http.StatusConflict, "REVISION_CONFLICT", "内容已更新，请重新读取")
	case errors.Is(err, store.ErrStaleMapGeneration):
		writeError(w, http.StatusConflict, "MAP_GENERATION_CONFLICT", "地图 generation 已变化，请重新选择")
	case errors.Is(err, store.ErrWorkflowExists):
		writeError(w, http.StatusConflict, "ACTIVE_WORKFLOW_EXISTS", "当前 Project 已有未结束 Workflow")
	case errors.Is(err, store.ErrProjectInactive):
		writeError(w, http.StatusConflict, "PROJECT_INACTIVE", "请先激活 Project")
	case errors.Is(err, store.ErrProjectArchived):
		writeError(w, http.StatusGone, "PROJECT_ARCHIVED", "Project 已归档")
	case errors.Is(err, store.ErrInvalidState):
		writeError(w, http.StatusUnprocessableEntity, "INVALID_STATE", "请求内容或当前状态不允许此操作")
	case errors.Is(err, store.ErrOperatorConfirmationRequired):
		writeError(w, http.StatusConflict, "OPERATOR_CONFIRMATION_REQUIRED",
			"执行物理状态未知，请确认现场机器人已安全保持后再终结 Workflow")
	default:
		h.internalError(w, message, err)
	}
	return true
}
