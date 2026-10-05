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

// HandleStudioSnapshotV030 在 v0.2 快照上增加当前 Workflow 与两张地图摘要。
// Layout 仍由浏览器保存，不进入业务快照。
func (h *ProjectsHandler) HandleStudioSnapshotV030(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "id")
	snapshot, err := h.st.BuildStudioSnapshot(auth.UserIDFromContext(r.Context()), projectID, 100)
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
			Revision: value.Revision, Agent: value.Agent, Type: value.Type, Status: value.Status,
			WorkflowID: value.WorkflowID, TaskID: value.TaskID, UIKind: value.UIKind,
			SourceRevision: value.SourceRevision, TargetAgentID: value.TargetAgentID,
			ResponseSchema: rawJSON(value.ResponseSchema, "{}"), MapID: value.MapID,
			MapGeneration: value.MapGeneration,
			Payload:       rawJSON(value.Payload, "{}"), Reply: rawJSON(value.Reply, "null"),
			RunID: value.RunID, CreatedAt: value.CreatedAt, AnsweredAt: value.AnsweredAt,
			ExpiredAt: value.ExpiredAt, ExpiresAt: value.ExpiresAt,
		})
	}
	var workflowView any
	var planProposal any
	proposal, proposalErr := h.st.GetActivePlanProposal(projectID)
	if proposalErr == nil {
		planProposal = proposal
	} else if proposalErr != store.ErrNotFound {
		h.internalError(w, "读取 Snapshot Plan Proposal 失败", proposalErr)
		return
	}
	allWorkflows, err := h.st.ListWorkflows(projectID, true)
	if err != nil {
		h.internalError(w, "读取 Snapshot Workflow 失败", err)
		return
	}
	workflows := make([]store.Workflow, 0)
	var latestEnded *store.Workflow
	// 保留活动工作及最近一条终态摘要，刷新后仍可按原 ID 读取结果。
	// ListWorkflows 按 updated_at 倒序；其余历史与 Task 不进入 Snapshot。
	for _, workflow := range allWorkflows {
		switch workflow.Status {
		case store.WorkflowStatusCompleted, store.WorkflowStatusFailed, store.WorkflowStatusStopped:
			if latestEnded == nil {
				ended := workflow
				latestEnded = &ended
			}
		default:
			workflows = append(workflows, workflow)
		}
	}
	// 详细 view 仍只为活动工作提供；最近终态不是“当前活动 Workflow”。
	if len(workflows) > 0 {
		view, getErr := h.st.GetWorkflowView(workflows[0].ID)
		if getErr != nil {
			h.internalError(w, "读取 Snapshot Workflow 详情失败", getErr)
			return
		}
		workflowView = view
	}
	if latestEnded != nil {
		workflows = append(workflows, *latestEnded)
	}
	mapSummaries := make([]map[string]any, 0, 2)
	maps, err := h.st.ListSemanticMaps(projectID)
	if err != nil {
		h.internalError(w, "读取 Snapshot Semantic Map 失败", err)
		return
	}
	for _, semanticMap := range maps {
		mapSnapshot, getErr := h.st.GetMapSnapshot(projectID, semanticMap.Slot, 0)
		if getErr != nil {
			h.internalError(w, "读取 Snapshot 地图摘要失败", getErr)
			return
		}
		mapSummaries = append(mapSummaries, map[string]any{
			"map_id": semanticMap.Slot, "generation": semanticMap.Generation,
			"revision": semanticMap.Revision, "entity_count": len(mapSnapshot.Entities),
			"relation_count": len(mapSnapshot.Relations),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": map[string]any{
		"snapshot_version": 3, "project": snapshot.Project,
		"conversations": conversations, "runs": snapshot.Runs,
		"pending_interactions": interactions, "memory_revision": snapshot.MemoryRevision,
		"event_sequence": snapshot.EventSequence, "captured_at": snapshot.CapturedAt,
		"plan_proposal": planProposal, "workflow_view": workflowView,
		"workflows":     workflows,
		"map_summaries": mapSummaries, "robot_executions": snapshot.RobotExecutions,
	}})
}
