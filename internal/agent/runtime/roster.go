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
	"sort"
	"sync"
)

// AgentStatus 是 roster 中 Agent 的当前运行状态。
type AgentStatus string

const (
	// AgentStatusStarting 正在装配。
	AgentStatusStarting AgentStatus = "starting"

	// AgentStatusIdle 待命：无在办工作（leader 等消息；service 等接入）。
	AgentStatusIdle AgentStatus = "idle"

	// AgentStatusRunning 正在处理一次 Run。
	AgentStatusRunning AgentStatus = "running"

	// AgentStatusStopped 已停止（优雅退出后）。
	AgentStatusStopped AgentStatus = "stopped"

	// AgentStatusOffline 表示逻辑 Agent 仍存在，但关联的 Pilot 当前不可达。
	AgentStatusOffline AgentStatus = "offline"
)

// AgentInfo 是 Agent 目录（roster）条目：成员身份与实时状态，
// 供 Agent 目录 API（GET /api/v1/agents）序列化。
type AgentInfo struct {
	// ID 成员实例标识（Team 定义中的 id，如 query-1）。
	ID string `json:"id"`

	// Role 角色名（profile 目录名，如 query）。
	Role string `json:"role"`

	// Source 标识目录项来源。Team 成员为空；Pilot 注册后派生的 Robot Agent
	// 为 pilot。它只用于控制面展示，不参与模型选择。
	Source string `json:"source,omitempty"`

	// 以下字段仅用于动态 Robot Agent。Agent 的逻辑身份是
	// robot:<robot_id>，模型 Run 仍按任务按需创建。
	RobotID         string `json:"robot_id,omitempty"`
	PilotInstanceID string `json:"pilot_instance_id,omitempty"`
	RobotModel      string `json:"robot_model,omitempty"`
	Backend         string `json:"backend,omitempty"`

	// Mode 交互模式（coordinator/worker/service）。
	Mode string `json:"mode"`

	// Status 生命周期状态。
	Status AgentStatus `json:"status"`

	// Model 角色主模型（profile 声明的端点名）。
	Model string `json:"model"`

	// DefaultInherited 表示 Profile 未固定模型，当前展示值继承系统 Default。
	DefaultInherited bool `json:"default_inherited"`

	// ReasoningEffort 当前端点的推理强度：auto/low/medium/high。
	ReasoningEffort string `json:"reasoning_effort"`

	// ReasoningVisibility 服务商推理内容展示策略：auto/show/hide。
	ReasoningVisibility string `json:"reasoning_visibility"`

	// Activity 当前活动；无活动时为空。
	Activity string `json:"activity,omitempty"`

	// Description 角色职责摘要。
	Description string `json:"description,omitempty"`

	// ToolNamespaces Profile 授权的工具命名空间。
	ToolNamespaces []string `json:"tool_namespaces"`

	// PinnedTools ToolSearch 开启时仍常驻可见的工具。
	PinnedTools []string `json:"pinned_tools"`

	// ToolSearch 是否对非 pinned 工具启用动态检索。
	ToolSearch bool `json:"tool_search"`

	// ApprovalRequired 需要人工审批的工具命名空间。
	ApprovalRequired []string `json:"approval_required"`

	// SkillNames 是 Agent Profile 允许且当前已安装的 Skill；具体会话还可能
	// 被 Project Skill 绑定进一步收窄。
	SkillNames      []string `json:"skill_names"`
	AgentSkillNames []string `json:"agent_skill_names"`
	RobotSkillNames []string `json:"robot_skill_names,omitempty"`

	// MaxTurns 单轮 ReAct 最大轮次。
	MaxTurns int `json:"max_turns"`

	// ContextTokens 上下文预算。
	ContextTokens int `json:"context_tokens"`

	// LongTermMemory 是否启用长期记忆。
	LongTermMemory bool `json:"long_term_memory"`
}

// roster 是进程内 Agent 目录：Team 组建时注册，状态随生命周期迁移。
// 所有方法并发安全。
type roster struct {
	// mu 保护 entries。
	mu sync.Mutex

	// entries 成员 ID → 目录条目。
	entries map[string]AgentInfo
}

// register 登记一个成员（重复登记覆盖，装配重试幂等）。
func (r *roster) register(info AgentInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[info.ID] = info
}

// setStatus 迁移成员状态与当前活动；未登记的 ID 静默忽略
// （未组建 Team 时 leader 的状态迁移天然无登记对象）。
func (r *roster) setStatus(id string, status AgentStatus, activity string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if info, ok := r.entries[id]; ok {
		info.Status = status
		info.Activity = activity
		r.entries[id] = info
	}
}

func (r *roster) get(id string) (AgentInfo, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	info, ok := r.entries[id]
	return info, ok
}

func (r *roster) updateRoleModels(role, model string, defaultInherited bool,
	effort, visibility string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, info := range r.entries {
		if info.Role != role {
			continue
		}
		info.Model, info.ReasoningEffort, info.ReasoningVisibility = model, effort, visibility
		info.DefaultInherited = defaultInherited
		r.entries[id] = info
	}
}

// list 返回全部目录条目（按 ID 升序，API 输出确定性）。
func (r *roster) list() []AgentInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := make([]AgentInfo, 0, len(r.entries))
	for _, info := range r.entries {
		list = append(list, info)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list
}
