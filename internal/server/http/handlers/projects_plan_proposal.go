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
	"net/http"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/store"
)

type planProposalActionRequest struct {
	Revision         int64 `json:"revision"`
	ExpectedRevision int64 `json:"expected_revision"`
}

func (r planProposalActionRequest) revision() int64 {
	if r.ExpectedRevision != 0 {
		return r.ExpectedRevision
	}
	return r.Revision
}

// HandleGetActivePlanProposal 返回对话中尚未结束的当前 Proposal；不存在时
// 返回 null，前端无需用 404 区分“还没有计划”和网络错误。
func (h *ProjectsHandler) HandleGetActivePlanProposal(w http.ResponseWriter, r *http.Request) {
	project, ok := h.ownedProject(w, r)
	if !ok {
		return
	}
	proposal, err := h.st.GetActivePlanProposal(project.ID)
	if err == store.ErrNotFound {
		writeJSON(w, http.StatusOK, map[string]any{"plan_proposal": nil})
		return
	}
	if h.writeV030Error(w, err, "读取 Plan Proposal 失败") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plan_proposal": proposal})
}

func (h *ProjectsHandler) HandleGetPlanProposal(w http.ResponseWriter, r *http.Request) {
	project, ok := h.ownedProject(w, r)
	if !ok {
		return
	}
	proposal, err := h.st.GetPlanProposal(chi.URLParam(r, "proposal_id"))
	if err == nil && proposal.ProjectID != project.ID {
		err = store.ErrNotFound
	}
	if h.writeV030Error(w, err, "读取 Plan Proposal 失败") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plan_proposal": proposal})
}

func (h *ProjectsHandler) HandlePlanProposalAction(w http.ResponseWriter, r *http.Request) {
	project, ok := h.ownedProject(w, r)
	if !ok {
		return
	}
	if !h.writableProject(w, r, project.ID) {
		return
	}
	var request planProposalActionRequest
	if !decodeV030JSON(w, r, &request) {
		return
	}
	revision := request.revision()
	if revision <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "INVALID_REVISION",
			"revision 必须大于 0")
		return
	}
	application := h.workflowApp
	if application == nil {
		writeError(w, http.StatusServiceUnavailable, "PLAN_SERVICE_UNAVAILABLE",
			"Plan 服务尚未装配")
		return
	}
	proposalID := chi.URLParam(r, "proposal_id")
	userID := auth.UserIDFromContext(r.Context())
	switch chi.URLParam(r, "action") {
	case "approve":
		view, err := application.ApprovePlanProposal(r.Context(), userID, project.ID,
			proposalID, revision)
		if h.writeV030Error(w, err, "批准 Plan Proposal 失败") {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"workflow_view": view})
	case "discard":
		proposal, err := application.DiscardPlanProposal(userID, project.ID,
			proposalID, revision)
		if h.writeV030Error(w, err, "丢弃 Plan Proposal 失败") {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"plan_proposal": proposal})
	default:
		writeError(w, http.StatusNotFound, "ACTION_NOT_FOUND",
			"Plan Proposal 操作不存在")
	}
}
