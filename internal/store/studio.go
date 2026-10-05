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
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// StudioSnapshot 是 Project 工作台恢复业务状态的最小完整快照。布局保存在
// 浏览器，不进入此结构；Workflow、Map、Robot 等后续资源也不在 v0.2 伪造。
type StudioSnapshot struct {
	Project             Project          `json:"project"`
	Conversations       []ChatSession    `json:"conversations"`
	Runs                []RunSession     `json:"runs"`
	PendingInteractions []Interaction    `json:"pending_interactions"`
	RobotExecutions     []RobotExecution `json:"robot_executions"`
	MemoryRevision      int64            `json:"memory_revision"`
	EventSequence       int64            `json:"event_sequence"`
	CapturedAt          time.Time        `json:"captured_at"`
}

// BuildStudioSnapshot 在同一个 SQLite 读事务内读取资源和事件位置。这样发生
// 在快照之后的修改一定拥有更大的 sequence，前端不会在“先读资源、后读游标”
// 的竞态窗口里漏掉一次更新。
func (s *Store) BuildStudioSnapshot(ownerID, projectID string, runLimit int) (StudioSnapshot, error) {
	if runLimit <= 0 {
		runLimit = 100
	}
	tx, err := s.db.Begin()
	if err != nil {
		return StudioSnapshot{}, fmt.Errorf("开启 Studio Snapshot 事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	project, err := scanProject(tx.QueryRow(`SELECT `+projectSelectColumns+
		` FROM projects WHERE id = ? AND owner_id = ?`, projectID, ownerID))
	if errors.Is(err, sql.ErrNoRows) {
		return StudioSnapshot{}, ErrNotFound
	}
	if err != nil {
		return StudioSnapshot{}, fmt.Errorf("读取 Snapshot Project 失败: %w", err)
	}
	if project.ArchivedAt != nil {
		return StudioSnapshot{}, ErrProjectArchived
	}

	conversations := make([]ChatSession, 0)
	rows, err := tx.Query(`SELECT id, user_id, project_id, title, archived_at,
		revision, created_at, updated_at FROM chat_sessions
		WHERE project_id = ? AND archived_at IS NULL ORDER BY updated_at DESC`,
		projectID)
	if err != nil {
		return StudioSnapshot{}, fmt.Errorf("读取 Snapshot Conversation 失败: %w", err)
	}
	for rows.Next() {
		var session ChatSession
		if err := rows.Scan(&session.ID, &session.UserID, &session.ProjectID,
			&session.Title, &session.ArchivedAt, &session.Revision,
			&session.CreatedAt, &session.UpdatedAt); err != nil {
			_ = rows.Close()
			return StudioSnapshot{}, fmt.Errorf("扫描 Snapshot Conversation 失败: %w", err)
		}
		conversations = append(conversations, session)
	}
	if err := rows.Close(); err != nil {
		return StudioSnapshot{}, fmt.Errorf("关闭 Snapshot Conversation 查询失败: %w", err)
	}

	runs := make([]RunSession, 0)
	rows, err = tx.Query(`SELECT `+runSelectColumns+` FROM run_sessions
		WHERE project_id = ? ORDER BY started_at DESC, id DESC LIMIT ?`,
		projectID, runLimit)
	if err != nil {
		return StudioSnapshot{}, fmt.Errorf("读取 Snapshot Run 失败: %w", err)
	}
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			_ = rows.Close()
			return StudioSnapshot{}, fmt.Errorf("扫描 Snapshot Run 失败: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Close(); err != nil {
		return StudioSnapshot{}, fmt.Errorf("关闭 Snapshot Run 查询失败: %w", err)
	}

	robotExecutions := make([]RobotExecution, 0)
	rows, err = tx.Query(`SELECT body FROM robot_executions WHERE project_id=? ORDER BY updated_at DESC LIMIT ?`,
		projectID, runLimit)
	if err != nil {
		return StudioSnapshot{}, fmt.Errorf("读取 Snapshot Robot Execution 失败: %w", err)
	}
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			_ = rows.Close()
			return StudioSnapshot{}, fmt.Errorf("扫描 Snapshot Robot Execution 失败: %w", err)
		}
		var execution RobotExecution
		if err := json.Unmarshal(body, &execution); err != nil {
			_ = rows.Close()
			return StudioSnapshot{}, fmt.Errorf("解析 Snapshot Robot Execution 失败: %w", err)
		}
		robotExecutions = append(robotExecutions, execution)
	}
	if err := rows.Close(); err != nil {
		return StudioSnapshot{}, fmt.Errorf("关闭 Snapshot Robot Execution 查询失败: %w", err)
	}

	pending := make([]Interaction, 0)
	rows, err = tx.Query(`SELECT `+interactionSelectColumns+
		` FROM interactions WHERE project_id = ? AND status = ?
		ORDER BY created_at`, projectID, InteractionStatusPending)
	if err != nil {
		return StudioSnapshot{}, fmt.Errorf("读取 Snapshot Interaction 失败: %w", err)
	}
	for rows.Next() {
		value, err := scanInteraction(rows)
		if err != nil {
			_ = rows.Close()
			return StudioSnapshot{}, fmt.Errorf("扫描 Snapshot Interaction 失败: %w", err)
		}
		pending = append(pending, value)
	}
	if err := rows.Close(); err != nil {
		return StudioSnapshot{}, fmt.Errorf("关闭 Snapshot Interaction 查询失败: %w", err)
	}

	var memoryRevision, eventSequence int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(revision), 0)
		FROM project_memories WHERE project_id = ?`, projectID).Scan(&memoryRevision); err != nil {
		return StudioSnapshot{}, fmt.Errorf("读取 Snapshot Memory 版本失败: %w", err)
	}
	if err := tx.QueryRow(`SELECT COALESCE(MAX(sequence), 0)
		FROM events WHERE project_id = ?`, projectID).Scan(&eventSequence); err != nil {
		return StudioSnapshot{}, fmt.Errorf("读取 Snapshot 事件位置失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return StudioSnapshot{}, fmt.Errorf("提交 Studio Snapshot 事务失败: %w", err)
	}
	return StudioSnapshot{
		Project: project, Conversations: conversations, Runs: runs,
		PendingInteractions: pending, RobotExecutions: robotExecutions, MemoryRevision: memoryRevision,
		EventSequence: eventSequence, CapturedAt: time.Now().UTC(),
	}, nil
}

// ProjectOwnedByUser 是 Studio WS 握手使用的只读归属检查。
func (s *Store) ProjectOwnedByUser(userID, projectID string) error {
	project, err := s.GetProject(projectID)
	if err != nil || project.OwnerID != userID {
		return ErrNotFound
	}
	if project.ArchivedAt != nil {
		return ErrProjectArchived
	}
	return nil
}

// ProjectWritableByUser 检查 Project 是否允许产生新业务状态。
func (s *Store) ProjectWritableByUser(userID, projectID string) error {
	project, err := s.GetProject(projectID)
	if err != nil || project.OwnerID != userID {
		return ErrNotFound
	}
	if project.ArchivedAt != nil {
		return ErrProjectArchived
	}
	if !project.IsActive {
		return ErrProjectInactive
	}
	return nil
}

// ConversationBelongsToProject 防止 Project WS 向其他 Project 的 Conversation
// 发送消息。
func (s *Store) ConversationBelongsToProject(userID, projectID, sessionID string) error {
	session, project, err := s.RequireWritableConversation(userID, sessionID)
	if err != nil || session.ProjectID != projectID || project.ID != projectID {
		return ErrNotFound
	}
	return nil
}

// RunBelongsToProject 是精确取消和 Run 详情的归属检查。
func (s *Store) RunBelongsToProject(userID, projectID, runID string) error {
	run, err := s.GetRunSession(runID)
	if err != nil || run.ProjectID != projectID {
		return ErrNotFound
	}
	return s.ProjectOwnedByUser(userID, projectID)
}

// InteractionBelongsToProject 是 Studio 交互应答前的归属检查。
func (s *Store) InteractionBelongsToProject(userID, projectID, interactionID string) error {
	value, err := s.GetInteraction(interactionID)
	if err != nil || value.ProjectID != projectID {
		return ErrNotFound
	}
	return s.ProjectOwnedByUser(userID, projectID)
}
