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

package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	// ModelSourceSystemDefault 表示会话创建时继承系统 Default。
	ModelSourceSystemDefault = "system_default"

	// ModelSourceAgentProfile 表示会话创建时继承 Agent Profile。
	ModelSourceAgentProfile = "agent_profile"

	// ModelSourceSessionOverride 表示用户显式覆盖当前会话中的 Agent 模型。
	ModelSourceSessionOverride = "session_override"
)

// SessionAgentModel 是一个 Agent 在一个对话会话中的模型快照。
type SessionAgentModel struct {
	SessionID       string    `json:"session_id"`
	AgentID         string    `json:"agent_id"`
	EndpointID      string    `json:"endpoint_id"`
	ReasoningEffort string    `json:"reasoning_effort"`
	Source          string    `json:"source"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// EnsureSessionAgentModel 只在快照不存在时创建，保证设置页和 Agent Profile
// 的后续变更不会悄悄影响已有会话。
func (s *Store) EnsureSessionAgentModel(snapshot SessionAgentModel) (SessionAgentModel, error) {
	if snapshot.SessionID == "" || snapshot.AgentID == "" || snapshot.EndpointID == "" {
		return SessionAgentModel{}, errors.New("会话 Agent 模型快照缺少 session_id、agent_id 或 endpoint_id")
	}
	if snapshot.ReasoningEffort == "" {
		snapshot.ReasoningEffort = "auto"
	}
	if snapshot.Source == "" {
		snapshot.Source = ModelSourceSystemDefault
	}
	now := time.Now().UTC()
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO session_agent_models
		(session_id, agent_id, endpoint_id, reasoning_effort, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, snapshot.SessionID, snapshot.AgentID,
		snapshot.EndpointID, snapshot.ReasoningEffort, snapshot.Source, now, now); err != nil {
		return SessionAgentModel{}, fmt.Errorf("创建会话 Agent 模型快照失败: %w", err)
	}
	return s.GetSessionAgentModel(snapshot.SessionID, snapshot.AgentID)
}

// SetSessionAgentModel 写入用户对当前会话 Agent 的显式覆盖。
func (s *Store) SetSessionAgentModel(sessionID, agentID, endpointID, effort string) (SessionAgentModel, error) {
	if sessionID == "" || agentID == "" || endpointID == "" {
		return SessionAgentModel{}, errors.New("会话 Agent 模型覆盖缺少必要字段")
	}
	if effort == "" {
		effort = "auto"
	}
	now := time.Now().UTC()
	if _, err := s.db.Exec(`INSERT INTO session_agent_models
		(session_id, agent_id, endpoint_id, reasoning_effort, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, agent_id) DO UPDATE SET
		endpoint_id = excluded.endpoint_id,
		reasoning_effort = excluded.reasoning_effort,
		source = excluded.source,
		updated_at = excluded.updated_at`, sessionID, agentID, endpointID, effort,
		ModelSourceSessionOverride, now, now); err != nil {
		return SessionAgentModel{}, fmt.Errorf("更新会话 Agent 模型失败: %w", err)
	}
	return s.GetSessionAgentModel(sessionID, agentID)
}

// GetSessionAgentModel 查询一个会话 Agent 模型快照。
func (s *Store) GetSessionAgentModel(sessionID, agentID string) (SessionAgentModel, error) {
	var snapshot SessionAgentModel
	err := s.db.QueryRow(`SELECT session_id, agent_id, endpoint_id, reasoning_effort,
		source, created_at, updated_at FROM session_agent_models
		WHERE session_id = ? AND agent_id = ?`, sessionID, agentID).Scan(
		&snapshot.SessionID, &snapshot.AgentID, &snapshot.EndpointID,
		&snapshot.ReasoningEffort, &snapshot.Source, &snapshot.CreatedAt, &snapshot.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionAgentModel{}, ErrNotFound
	}
	if err != nil {
		return SessionAgentModel{}, fmt.Errorf("查询会话 Agent 模型失败: %w", err)
	}
	return snapshot, nil
}

// ListSessionAgentModels 返回会话中全部 Agent 模型快照，按 Agent ID 排序。
func (s *Store) ListSessionAgentModels(sessionID string) ([]SessionAgentModel, error) {
	rows, err := s.db.Query(`SELECT session_id, agent_id, endpoint_id, reasoning_effort,
		source, created_at, updated_at FROM session_agent_models
		WHERE session_id = ? ORDER BY agent_id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("查询会话 Agent 模型列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []SessionAgentModel
	for rows.Next() {
		var snapshot SessionAgentModel
		if err := rows.Scan(&snapshot.SessionID, &snapshot.AgentID, &snapshot.EndpointID,
			&snapshot.ReasoningEffort, &snapshot.Source, &snapshot.CreatedAt,
			&snapshot.UpdatedAt); err != nil {
			return nil, fmt.Errorf("扫描会话 Agent 模型失败: %w", err)
		}
		result = append(result, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历会话 Agent 模型失败: %w", err)
	}
	return result, nil
}
