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

// ContextSummary 保存 Conversation 的压缩结果及其覆盖边界。Transcript 仍在
// chat_messages 中完整保留，Summary 只决定下一轮模型需要装配哪些消息。
type ContextSummary struct {
	ContextID               string    `json:"context_id"`
	ProjectID               string    `json:"project_id"`
	SessionID               string    `json:"conversation_id"`
	TaskID                  string    `json:"task_id,omitempty"`
	Summary                 string    `json:"summary"`
	CoveredThroughMessageID string    `json:"covered_through_message_id"`
	Revision                int64     `json:"revision"`
	UpdatedAt               time.Time `json:"updated_at"`
}

// GetContextSummary 返回指定 Context 的最后一份有效摘要。
func (s *Store) GetContextSummary(contextID string) (ContextSummary, error) {
	var summary ContextSummary
	err := s.db.QueryRow(`SELECT context_id, project_id, session_id, task_id, summary,
		covered_through_message_id, revision, updated_at
		FROM context_summaries WHERE context_id = ?`, contextID).Scan(
		&summary.ContextID, &summary.ProjectID, &summary.SessionID, &summary.TaskID,
		&summary.Summary, &summary.CoveredThroughMessageID,
		&summary.Revision, &summary.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ContextSummary{}, ErrNotFound
	}
	if err != nil {
		return ContextSummary{}, fmt.Errorf("查询 Context 摘要失败: %w", err)
	}
	return summary, nil
}

// GetContextSummaryBySession 是 Leader Conversation 的便捷读取入口。
func (s *Store) GetContextSummaryBySession(sessionID string) (ContextSummary, error) {
	var summary ContextSummary
	err := s.db.QueryRow(`SELECT context_id, project_id, session_id, task_id, summary,
		covered_through_message_id, revision, updated_at
		FROM context_summaries WHERE context_id = ?`, "leader:"+sessionID).Scan(
		&summary.ContextID, &summary.ProjectID, &summary.SessionID, &summary.TaskID,
		&summary.Summary, &summary.CoveredThroughMessageID,
		&summary.Revision, &summary.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ContextSummary{}, ErrNotFound
	}
	if err != nil {
		return ContextSummary{}, fmt.Errorf("查询 Conversation 摘要失败: %w", err)
	}
	return summary, nil
}

// SaveContextSummary 使用 expectedRevision 条件保存摘要。首次保存传 0；更新
// 必须传当前 revision，摘要生成较慢时不会覆盖另一轮刚保存的新边界。
func (s *Store) SaveContextSummary(value ContextSummary, expectedRevision int64) (ContextSummary, error) {
	session, err := s.GetChatSession(value.SessionID)
	if err != nil {
		return ContextSummary{}, err
	}
	if value.ProjectID == "" {
		value.ProjectID = session.ProjectID
	}
	if value.ProjectID != session.ProjectID {
		return ContextSummary{}, ErrInvalidState
	}
	if value.ContextID == "" {
		value.ContextID = "leader:" + value.SessionID
	}
	if value.TaskID != "" {
		task, taskErr := s.GetTask(value.TaskID)
		if taskErr != nil || task.ContextID != value.ContextID {
			return ContextSummary{}, ErrInvalidState
		}
	}
	if value.CoveredThroughMessageID != "" {
		var exists bool
		if value.TaskID != "" {
			exists, err = s.ContextMessageExists(value.ContextID, value.CoveredThroughMessageID)
		} else {
			var one int
			err = s.db.QueryRow(`SELECT 1 FROM chat_messages WHERE id = ? AND session_id = ?`,
				value.CoveredThroughMessageID, value.SessionID).Scan(&one)
			exists = err == nil
		}
		if errors.Is(err, sql.ErrNoRows) || !exists {
			return ContextSummary{}, fmt.Errorf("摘要边界消息不存在: %w", ErrInvalidState)
		}
		if err != nil {
			return ContextSummary{}, fmt.Errorf("检查摘要边界失败: %w", err)
		}
	}
	now := time.Now().UTC()
	if expectedRevision == 0 {
		_, err = s.db.Exec(`INSERT INTO context_summaries
			(context_id, project_id, session_id, task_id, summary,
			 covered_through_message_id, revision, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 1, ?)`,
			value.ContextID, value.ProjectID, value.SessionID, value.TaskID, value.Summary,
			value.CoveredThroughMessageID, now)
		if err != nil {
			return ContextSummary{}, fmt.Errorf("首次保存 Context 摘要失败: %w",
				ErrRevisionConflict)
		}
		return s.GetContextSummary(value.ContextID)
	}
	result, err := s.db.Exec(`UPDATE context_summaries SET summary = ?,
		covered_through_message_id = ?, revision = revision + 1, updated_at = ?
		WHERE context_id = ? AND project_id = ? AND session_id = ?
		  AND revision = ?`, value.Summary, value.CoveredThroughMessageID, now,
		value.ContextID, value.ProjectID, value.SessionID, expectedRevision)
	if err != nil {
		return ContextSummary{}, fmt.Errorf("更新 Context 摘要失败: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ContextSummary{}, ErrRevisionConflict
	}
	return s.GetContextSummary(value.ContextID)
}

// ListChatMessagesAfter 只读取摘要边界之后的消息，按时间升序返回。
func (s *Store) ListChatMessagesAfter(sessionID, afterMessageID string, limit int) ([]ChatMessage, error) {
	query := `SELECT m.id, m.session_id, m.agent_id, m.run_id, m.trace_id, m.provider, m.endpoint,
		m.model, m.message_json, m.artifact_refs, m.metadata, m.created_at, r.started_at
		FROM chat_messages m LEFT JOIN run_sessions r ON r.id = m.run_id
		WHERE m.session_id = ? AND m.id > ? ORDER BY m.id`
	args := []any{sessionID, afterMessageID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	return s.listChatMessagesQuery(query, args...)
}

// LatestChatMessageID 返回 Conversation 当前最后一条消息；空会话返回空字符串。
func (s *Store) LatestChatMessageID(sessionID string) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM chat_messages
		WHERE session_id = ? ORDER BY id DESC LIMIT 1`, sessionID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("查询最后一条消息失败: %w", err)
	}
	return id, nil
}

// ProjectMemory 是用户明确维护的一份 Project Markdown 文档。
type ProjectMemory struct {
	ProjectID string    `json:"project_id"`
	Content   string    `json:"content"`
	Revision  int64     `json:"revision"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// GetProjectMemory 返回 Project Memory；尚未创建时返回空内容和 revision=0。
func (s *Store) GetProjectMemory(projectID string) (ProjectMemory, error) {
	if _, err := s.GetProject(projectID); err != nil {
		return ProjectMemory{}, err
	}
	var memory ProjectMemory
	err := s.db.QueryRow(`SELECT project_id, content, revision, updated_at
		FROM project_memories WHERE project_id = ?`, projectID).Scan(
		&memory.ProjectID, &memory.Content, &memory.Revision, &memory.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectMemory{ProjectID: projectID}, nil
	}
	if err != nil {
		return ProjectMemory{}, fmt.Errorf("读取 Project Memory 失败: %w", err)
	}
	return memory, nil
}

// SaveProjectMemory 条件保存活动 Project 的 Markdown Memory。expectedRevision
// 为 0 表示首次创建，其他值必须等于当前 revision。
func (s *Store) SaveProjectMemory(ownerID, projectID, content string, expectedRevision int64) (ProjectMemory, error) {
	project, err := s.GetProject(projectID)
	if err != nil || project.OwnerID != ownerID {
		return ProjectMemory{}, ErrNotFound
	}
	if project.ArchivedAt != nil {
		return ProjectMemory{}, ErrProjectArchived
	}
	if !project.IsActive {
		return ProjectMemory{}, ErrProjectInactive
	}
	now := time.Now().UTC()
	if expectedRevision == 0 {
		if _, err := s.db.Exec(`INSERT INTO project_memories
			(project_id, content, revision, updated_at) VALUES (?, ?, 1, ?)`,
			projectID, content, now); err != nil {
			return ProjectMemory{}, ErrRevisionConflict
		}
		return s.GetProjectMemory(projectID)
	}
	result, err := s.db.Exec(`UPDATE project_memories SET content = ?,
		revision = revision + 1, updated_at = ?
		WHERE project_id = ? AND revision = ?`,
		content, now, projectID, expectedRevision)
	if err != nil {
		return ProjectMemory{}, fmt.Errorf("保存 Project Memory 失败: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ProjectMemory{}, ErrRevisionConflict
	}
	return s.GetProjectMemory(projectID)
}
