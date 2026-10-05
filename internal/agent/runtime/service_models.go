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

package runtime

import (
	"errors"
	"fmt"
	"strings"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/llm"
)

var (
	// ErrSessionBusy 表示会话仍有活动 Run，当前不能修改任一 Agent 模型。
	ErrSessionBusy = errors.New("SESSION_BUSY")
)

// SessionAgentModelView 是会话模型控制面的完整只读视图。
type SessionAgentModelView struct {
	AgentID           string `json:"agent_id"`
	Role              string `json:"role"`
	EndpointID        string `json:"endpoint_id"`
	ReasoningEffort   string `json:"reasoning_effort"`
	Source            string `json:"source"`
	DefaultInherited  bool   `json:"default_inherited"`
	Provider          string `json:"provider"`
	Model             string `json:"model"`
	SupportsReasoning bool   `json:"supports_reasoning_effort"`
	Busy              bool   `json:"busy"`
}

// resolveSessionModelEntry 按 session_id + agent_id 解析模型。快照不存在时
// 只创建一次；之后 Profile 或系统 Default 变化不会影响该会话。
func (s *Service) resolveSessionModelEntry(sessionID, agentID string,
	prof *profile.Profile) (llm.Provider, store.SessionAgentModel, error) {
	snapshot, err := s.ensureSessionAgentModel(sessionID, agentID, prof)
	if err != nil {
		return llm.Provider{}, store.SessionAgentModel{}, err
	}
	entry, _, err := s.resolveNamedEntry(snapshot.EndpointID)
	if err != nil {
		return llm.Provider{}, store.SessionAgentModel{}, err
	}
	entry = withReasoningEffort(entry, effectiveReasoningEffort(snapshot.ReasoningEffort))
	return entry, snapshot, nil
}

// ensureSessionAgentModel 使用“系统 Default → Agent Profile”生成初始快照。
// 会话覆盖由 SetSessionAgentModel 单独写入，优先级高于该初始值。
func (s *Service) ensureSessionAgentModel(sessionID, agentID string,
	prof *profile.Profile) (store.SessionAgentModel, error) {
	endpoint, source := prof.Model, store.ModelSourceAgentProfile
	if endpoint == "" {
		endpoint, source = s.llmReg.Default().Name, store.ModelSourceSystemDefault
	}
	effort := prof.ReasoningEffort
	if effort == "" {
		effort = "auto"
	}
	return s.st.EnsureSessionAgentModel(store.SessionAgentModel{
		SessionID: sessionID, AgentID: agentID, EndpointID: endpoint,
		ReasoningEffort: effort, Source: source,
	})
}

// InitializeSessionModels 为会话创建时已存在的 Agent 固化模型快照。首次在
// 会话中加入的新 Agent 仍会由 resolveSessionModelEntry 懒创建一次。
func (s *Service) InitializeSessionModels(sessionID string) error {
	infos := s.roster.list()
	if len(infos) == 0 {
		infos = []AgentInfo{{ID: agentRoleLeader, Role: agentRoleLeader}}
	}
	for _, info := range infos {
		prof, err := s.profiles.Load(info.Role)
		if err != nil {
			return err
		}
		if _, err := s.ensureSessionAgentModel(sessionID, info.ID, prof); err != nil {
			return err
		}
	}
	return nil
}

// ListSessionAgentModels 返回用户所属会话的模型快照与实际端点信息。
func (s *Service) ListSessionAgentModels(userID, sessionID string) ([]SessionAgentModelView, error) {
	if err := s.requireOwnedSession(userID, sessionID); err != nil {
		return nil, err
	}
	if err := s.InitializeSessionModels(sessionID); err != nil {
		return nil, err
	}
	snapshots, err := s.st.ListSessionAgentModels(sessionID)
	if err != nil {
		return nil, err
	}
	busy := s.sessionBusy(sessionID)
	views := make([]SessionAgentModelView, 0, len(snapshots))
	for _, snapshot := range snapshots {
		entry, err := s.llmReg.Get(snapshot.EndpointID)
		if err != nil {
			return nil, err
		}
		role := snapshot.AgentID
		if strings.HasPrefix(snapshot.AgentID, "robot:") {
			role = "robot"
		} else if info, ok := s.roster.get(snapshot.AgentID); ok {
			role = info.Role
		}
		views = append(views, SessionAgentModelView{
			AgentID: snapshot.AgentID, Role: role, EndpointID: snapshot.EndpointID,
			ReasoningEffort: snapshot.ReasoningEffort, Source: snapshot.Source,
			DefaultInherited: snapshot.Source == store.ModelSourceSystemDefault,
			Provider:         entry.Service, Model: entry.Model,
			SupportsReasoning: hasCapability(entry.Capabilities, "reasoning_effort"), Busy: busy,
		})
	}
	return views, nil
}

// SetSessionAgentModel 在会话空闲时更新一个 Agent 的模型覆盖。更新后淘汰
// 会话 Runner，使下一轮按新快照重建；历史消息本身保持不变。
func (s *Service) SetSessionAgentModel(userID, sessionID, agentID, endpointID,
	effort string) (SessionAgentModelView, error) {
	if _, _, err := s.requireWritableSession(userID, sessionID); err != nil {
		return SessionAgentModelView{}, err
	}
	role := agentID
	if strings.HasPrefix(agentID, "robot:") {
		if _, err := s.conversationRecipient(userID, sessionID, agentID); err != nil {
			return SessionAgentModelView{}, err
		}
		role = "robot"
	} else if info, ok := s.roster.get(agentID); ok {
		role = info.Role
	} else if agentID != agentRoleLeader {
		if s.subAgents == nil {
			return SessionAgentModelView{}, fmt.Errorf("Agent %q 不存在", agentID)
		}
		def, ok := s.subAgents.Get(agentID)
		if !ok {
			return SessionAgentModelView{}, fmt.Errorf("Agent %q 不存在", agentID)
		}
		role = def.Role
	}
	if _, err := s.profiles.Load(role); err != nil {
		return SessionAgentModelView{}, err
	}
	entry, err := s.llmReg.Get(endpointID)
	if err != nil {
		return SessionAgentModelView{}, err
	}
	if effort == "" {
		effort = "auto"
	}
	if err := validateReasoningEffort(effort); err != nil {
		return SessionAgentModelView{}, err
	}
	if effort != "auto" && !hasCapability(entry.Capabilities, "reasoning_effort") {
		effort = "auto"
	}
	// 与消息主循环使用同一把会话锁。TryLock 失败包含模型调用、工具调用及
	// 消息正在进入运行态的窗口，统一返回 409，不等待并暗中延迟切换。
	gate := s.sessionLock(sessionID)
	if !gate.TryLock() {
		return SessionAgentModelView{}, ErrSessionBusy
	}
	defer gate.Unlock()
	if s.sessionBusy(sessionID) {
		return SessionAgentModelView{}, ErrSessionBusy
	}
	var snapshot store.SessionAgentModel
	err = s.st.WithWritableConversation(userID, sessionID,
		func(_ store.ChatSession, _ store.Project) error {
			var writeErr error
			snapshot, writeErr = s.st.SetSessionAgentModel(sessionID, agentID,
				endpointID, effort)
			return writeErr
		})
	if err != nil {
		return SessionAgentModelView{}, mapWritableSessionError(err)
	}
	s.mu.Lock()
	delete(s.sessions, sessionID)
	delete(s.staleSessions, sessionID)
	s.mu.Unlock()
	return SessionAgentModelView{
		AgentID: agentID, Role: role, EndpointID: endpointID,
		ReasoningEffort: effort, Source: snapshot.Source,
		Provider: entry.Service, Model: entry.Model,
		SupportsReasoning: hasCapability(entry.Capabilities, "reasoning_effort"), Busy: false,
	}, nil
}

// requireOwnedSession 隐藏会话是否属于其他用户，统一返回 ErrSessionNotFound。
func (s *Service) requireOwnedSession(userID, sessionID string) error {
	session, err := s.st.GetChatSession(sessionID)
	if err != nil || session.UserID != userID {
		return ErrSessionNotFound
	}
	return nil
}

// sessionBusy 检查会话是否存在活动 Run。
func (s *Service) sessionBusy(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, active := range s.activeRuns {
		if active.sessionID == sessionID {
			return true
		}
	}
	return false
}

func validateReasoningEffort(effort string) error {
	if effort != "auto" && effort != "low" && effort != "medium" && effort != "high" {
		return fmt.Errorf("reasoning_effort 仅支持 auto/low/medium/high")
	}
	return nil
}

// resolveNamedEntry 解析指定端点并校验凭据。模型故障或凭据缺失必须显式
// 返回给用户处理，不能静默切换到另一个模型破坏会话行为的一致性。
func (s *Service) resolveNamedEntry(name string) (llm.Provider, string, error) {
	entry, err := s.llmReg.Get(name)
	if err != nil {
		return llm.Provider{}, "", err
	}
	if kernel.RequiresAPIKey(entry.Component) && s.llmReg.APIKey(name) == "" {
		return llm.Provider{}, "", fmt.Errorf("模型端点 %q 未配置 API Token", name)
	}
	return entry, name, nil
}

// effectiveReasoningEffort 把配置层的 auto 转换为“不发送固定档位”。推理
// 复杂度由当前模型自行决定，不再通过另一个 Depth Model 路由。
func effectiveReasoningEffort(effort string) string {
	if effort == "" || effort == "auto" {
		return ""
	}
	return effort
}

// robotExecutionReasoningEffort 只收敛执行期参数组装。Leader与Task Planning仍
// 使用会话配置；用户显式选择low/medium/high时也继续优先于这个默认值。
func robotExecutionReasoningEffort(configured string) string {
	if effort := effectiveReasoningEffort(configured); effort != "" {
		return effort
	}
	return "low"
}

func withReasoningEffort(entry llm.Provider, effort string) llm.Provider {
	options := make(map[string]any, len(entry.Options)+1)
	for key, value := range entry.Options {
		options[key] = value
	}
	// 覆盖为 auto 时必须移除端点默认档位，才能真正做到“不发送固定档位”。
	delete(options, "reasoning_effort")
	if effort != "" {
		options["reasoning_effort"] = effort
	}
	entry.Options = options
	return entry
}

// InvalidateModelRuntimes 使全部会话级 Runner 的模型装配失效。LLM 注册表
// 热更新后调用：空闲 Runner 立即删除，运行中 Runner 完成本轮后删除。
func (s *Service) InvalidateModelRuntimes() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidateModelRuntimesLocked("")
}

// invalidateModelRuntimesLocked 按角色淘汰会话 Runner；role 为空表示全部角色。
// 调用方必须持有 s.mu。
func (s *Service) invalidateModelRuntimesLocked(role string) {
	s.runtimeGeneration++
	for sessionID, rt := range s.sessions {
		if role != "" && rt.profile.Name != role {
			continue
		}
		if s.sessionBusyLocked(sessionID) {
			s.staleSessions[sessionID] = struct{}{}
			continue
		}
		delete(s.sessions, sessionID)
		delete(s.staleSessions, sessionID)
	}
}

func (s *Service) sessionBusyLocked(sessionID string) bool {
	for _, active := range s.activeRuns {
		if active.sessionID == sessionID {
			return true
		}
	}
	return false
}

// UpdateAgentModels 更新成员所属角色的共享模型配置。
func (s *Service) UpdateAgentModels(agentID, model, effort, visibility string) error {
	info, ok := s.roster.get(agentID)
	if !ok {
		return fmt.Errorf("Agent %q 不存在", agentID)
	}
	if model != "" {
		if _, err := s.llmReg.Get(model); err != nil {
			return err
		}
	}
	if effort == "" {
		effort = "auto"
	}
	if err := validateReasoningEffort(effort); err != nil {
		return err
	}
	if visibility == "" {
		visibility = "auto"
	}
	if visibility != "auto" && visibility != "show" && visibility != "hide" {
		return fmt.Errorf("reasoning_visibility 仅支持 auto/show/hide")
	}
	if err := s.profiles.UpdateModels(info.Role, model, effort, visibility); err != nil {
		return err
	}
	effectiveModel, defaultInherited := model, false
	if effectiveModel == "" {
		effectiveModel, defaultInherited = s.llmReg.Default().Name, true
	}
	s.roster.updateRoleModels(info.Role, effectiveModel, defaultInherited, effort, visibility)
	// Profile 更新只影响后续新会话；已有会话使用创建时固化的模型快照。
	return nil
}
