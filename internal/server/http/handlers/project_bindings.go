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

	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
)

type replaceProjectBindingsRequest struct {
	AgentIDs   []string `json:"agent_ids"`
	SkillNames []string `json:"skill_names"`
}

// HandleGetProjectBindings 返回 Project 显式启用的 Agent 与只读 Agent Skill。
// Robot Skill 属于设备 desired/actual 状态，不进入这份 Project 上下文绑定。
func (h *ProjectsHandler) HandleGetProjectBindings(w http.ResponseWriter, r *http.Request) {
	project, ok := h.ownedProject(w, r)
	if !ok {
		return
	}
	bindings, err := h.st.GetProjectBindings(project.ID)
	if h.writeStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bindings": bindings})
}

// HandleReplaceProjectBindings 复用现有 Project 绑定表原子替换上下文资源。
// 运行中的 Project 不允许中途改变 Agent 可见 Skill，避免同一 Workflow 的
// 前后 Run 获得不同规划规则；这里没有引入第二套 Skill 安装或挂载模型。
func (h *ProjectsHandler) HandleReplaceProjectBindings(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "id")
	if !h.writableProject(w, r, projectID) {
		return
	}
	var request replaceProjectBindingsRequest
	if !decodeV030JSON(w, r, &request) {
		return
	}
	bindings := store.ProjectBindings{AgentIDs: request.AgentIDs, SkillNames: request.SkillNames}
	if err := h.st.ReplaceProjectBindings(r.Context(), projectID, bindings); h.writeStoreError(w, err) {
		return
	}
	bindings, err := h.st.GetProjectBindings(projectID)
	if h.writeStoreError(w, err) {
		return
	}
	project, err := h.st.GetProject(projectID)
	if h.writeStoreError(w, err) {
		return
	}
	h.publishResourceEvent(project.ID, "", "project", project.ID, project.Revision,
		"project.bindings.updated", map[string]any{"project": project, "bindings": bindings}, ws.ParentRef{})
	writeJSON(w, http.StatusOK, map[string]any{"project": project, "bindings": bindings})
}
