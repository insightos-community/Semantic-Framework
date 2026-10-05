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
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/agent/runtime"
	"insightos.cn/semantic-framework/internal/server/auth"
)

// AgentsHandler 是 Agent 目录的 REST 处理器（架构文档 04 §4 Team 目录）：
// 只读暴露 roster（Team 组建时登记的成员清单与实时状态）。
type AgentsHandler struct {
	// rt Agent 运行时（roster 的持有方）。
	rt *runtime.Service
}

type updateAgentModelsRequest struct {
	Model               string `json:"model"`
	ReasoningEffort     string `json:"reasoning_effort"`
	ReasoningVisibility string `json:"reasoning_visibility"`
}

// updateSessionAgentModelRequest 是会话级 Agent 模型覆盖请求。endpoint_id
// 指向模型端点而不是 Provider model ID，避免多个端点使用同一 model 时歧义。
type updateSessionAgentModelRequest struct {
	EndpointID      string `json:"endpoint_id"`
	ReasoningEffort string `json:"reasoning_effort"`
}

// NewAgentsHandler 创建 Agent 目录处理器。
func NewAgentsHandler(rt *runtime.Service) *AgentsHandler {
	return &AgentsHandler{rt: rt}
}

// agentsResponse 是 GET /agents 的响应体。
type agentsResponse struct {
	// Agents 全部成员（按 ID 升序）。
	Agents []runtime.AgentInfo `json:"agents"`
}

// HandleListAgents 处理 GET /api/v1/agents：返回 Team 成员目录
// （id/role/mode/状态/模型/当前活动）。
func (h *AgentsHandler) HandleListAgents(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, agentsResponse{Agents: h.rt.Roster()})
}

// HandleUpdateAgentModels 更新 Agent 所属角色的模型策略。
func (h *AgentsHandler) HandleUpdateAgentModels(w http.ResponseWriter, r *http.Request) {
	var req updateAgentModelsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "请求体不是合法 JSON")
		return
	}
	if err := h.rt.UpdateAgentModels(chi.URLParam(r, "id"), req.Model,
		req.ReasoningEffort, req.ReasoningVisibility); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// HandleListSessionAgents 返回当前会话中每个 Agent 的模型快照。接口同时
// 返回实际 Provider/model 和配置来源，前端不需要依赖模型自述判断身份。
func (h *AgentsHandler) HandleListSessionAgents(w http.ResponseWriter, r *http.Request) {
	models, err := h.rt.ListSessionAgentModels(auth.UserIDFromContext(r.Context()),
		chi.URLParam(r, "id"))
	if errors.Is(err, runtime.ErrSessionNotFound) {
		writeError(w, http.StatusNotFound, CodeSessionNotFound, "会话不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	recipients, err := h.rt.ListConversationRecipients(auth.UserIDFromContext(r.Context()), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": models, "recipients": recipients})
}

// HandleListSessionAgentTools 返回某个会话 Agent 的实际有效工具集。该接口
// 与全局 GET /tools 的安装目录分离，按 Profile、Project、SubAgent、
// ToolSearch 及会话宿主权限计算，供前端准确解释模型为什么能或不能调用工具。
func (h *AgentsHandler) HandleListSessionAgentTools(w http.ResponseWriter, r *http.Request) {
	view, err := h.rt.ListSessionAgentTools(r.Context(),
		auth.UserIDFromContext(r.Context()), chi.URLParam(r, "id"),
		chi.URLParam(r, "agent_id"))
	if errors.Is(err, runtime.ErrSessionNotFound) {
		writeError(w, http.StatusNotFound, CodeSessionNotFound, "会话不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// HandleUpdateSessionAgentModel 在会话空闲时设置一个 Agent 的会话覆盖。
// 活动 Run 存在时返回稳定的 409 错误码，调用方可在本轮结束后重试。
func (h *AgentsHandler) HandleUpdateSessionAgentModel(w http.ResponseWriter, r *http.Request) {
	var req updateSessionAgentModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.EndpointID == "" {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "endpoint_id 不能为空")
		return
	}
	model, err := h.rt.SetSessionAgentModel(auth.UserIDFromContext(r.Context()),
		chi.URLParam(r, "id"), chi.URLParam(r, "agent_id"), req.EndpointID,
		req.ReasoningEffort)
	if writeConversationWriteError(w, err) {
		return
	}
	switch {
	case errors.Is(err, runtime.ErrSessionBusy):
		writeError(w, http.StatusConflict, CodeSessionBusy, "会话正在运行，模型将在空闲后才能切换")
	case err != nil:
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"agent": model})
	}
}
