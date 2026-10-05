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
	"strings"
	"time"

	"github.com/google/uuid"
)

// 交互类型取值，与 interactions.type 一致（架构文档 12 §3.1，v1 仅落地 confirm）。
const (
	InteractionTypeConfirm = "confirm"
	InteractionTypeInput   = "input"
)

const (
	InteractionUIForm         = "form"
	InteractionUISingleSelect = "single_select"
	InteractionUIMultiSelect  = "multi_select"
	InteractionUIParameter    = "parameter"
	InteractionUIImageSelect  = "image_select"
	InteractionUIMapSelect    = "map_select"
	InteractionUIFileSelect   = "file_select"
)

// 交互状态机取值，与 interactions.status 一致（架构文档 12 §3.3）。
const (
	// InteractionStatusPending 待应答（已发起，等待用户）。
	InteractionStatusPending = "pending"

	// InteractionStatusAnswered 已应答。
	InteractionStatusAnswered = "answered"

	// InteractionStatusExpired 已超时（on_timeout 策略生效）。
	InteractionStatusExpired = "expired"

	// InteractionStatusCancelled 已取消（任务取消/会话删除）。
	InteractionStatusCancelled = "cancelled"
)

// ErrNotPending 表示交互不在待应答状态（重复应答、已过期、已取消），
// 调用方应以 errors.Is 判断。
var ErrNotPending = errors.New("交互不在待应答状态")

// Interaction 是一条结构化交互请求（架构文档 12 §3）。
type Interaction struct {
	// ID 交互唯一标识（int- 前缀 + uuid）。
	ID string

	// ProjectID 是交互所属 Project，由 Conversation 自动补齐。
	ProjectID string

	// SessionID 目标会话 ID（交互路由到该会话的属主用户）。
	SessionID string

	// Revision 用于 Studio 忽略旧增量。
	Revision int64

	WorkflowID     string
	TaskID         string
	UIKind         string
	SourceRevision int64
	MapID          string
	MapGeneration  int64
	TargetAgentID  string
	ResponseSchema string

	// Agent 发起交互的 Agent 名。
	Agent string

	// Type 交互类型（v1 仅 confirm）。
	Type string

	// Status 交互状态（pending/answered/expired/cancelled）。
	Status string

	// Payload 请求负载 JSON（question/risk/timeout_ts 等，schema 按类型定）。
	Payload string

	// Reply 应答负载 JSON（如 {"approved":true}）；未应答时为空。
	Reply string

	// RunID 关联的 run session ID（审批恢复执行的归属 run）。
	RunID string

	// CheckpointID 关联的内核断点 ID（interrupt/resume 衔接用）。
	CheckpointID string

	// CreatedAt 创建时间。
	CreatedAt time.Time

	// AnsweredAt 应答时间；未应答为 nil。
	AnsweredAt *time.Time

	// ExpiredAt 超时时间；未发生为 nil。
	ExpiredAt     *time.Time
	ExpiresAt     *time.Time
	CancelledAt   *time.Time
	HandledAt     *time.Time
	HandlingError string
}

// NewInteractionID 生成交互 ID（int- 前缀 + uuid）。
func NewInteractionID() string {
	return "int-" + uuid.NewString()
}

const interactionSelectColumns = `id, project_id, session_id, revision, agent,
	type, status, payload, reply, run_id, checkpoint_id, workflow_id, task_id,
	ui_kind, source_revision, map_id, map_generation, target_agent_id, response_schema,
	created_at, answered_at, expired_at, expires_at, cancelled_at, handled_at, handling_error`

type interactionScanner interface {
	Scan(dest ...any) error
}

func scanInteraction(scanner interactionScanner) (Interaction, error) {
	var value Interaction
	err := scanner.Scan(&value.ID, &value.ProjectID, &value.SessionID,
		&value.Revision, &value.Agent, &value.Type, &value.Status,
		&value.Payload, &value.Reply, &value.RunID, &value.CheckpointID,
		&value.WorkflowID, &value.TaskID, &value.UIKind, &value.SourceRevision,
		&value.MapID, &value.MapGeneration, &value.TargetAgentID, &value.ResponseSchema,
		&value.CreatedAt, &value.AnsweredAt, &value.ExpiredAt, &value.ExpiresAt,
		&value.CancelledAt, &value.HandledAt, &value.HandlingError)
	return value, err
}

// CreateInteraction 写入一条待应答交互，并从 Conversation 补齐 Project。
func (s *Store) CreateInteraction(it Interaction) error {
	session, err := s.GetChatSession(it.SessionID)
	implicitSystemRecord := false
	if err == nil {
		if it.ProjectID == "" {
			it.ProjectID = session.ProjectID
		}
		if it.ProjectID != session.ProjectID {
			return ErrInvalidState
		}
	} else if errors.Is(err, ErrNotFound) && it.ProjectID == "" {
		// 兼容系统诊断和旧测试直接写入的交互；公共 API 始终先校验 Conversation。
		it.ProjectID = SystemDefaultProjectID
		implicitSystemRecord = true
	} else if err != nil {
		return fmt.Errorf("交互 Conversation 不存在: %w", err)
	}
	if !implicitSystemRecord && session.UserID != "" {
		project, projectErr := s.GetProject(it.ProjectID)
		if projectErr != nil || project.ArchivedAt != nil || !project.IsActive {
			return ErrProjectInactive
		}
	}
	if it.RunID != "" && !implicitSystemRecord {
		run, err := s.GetRunSession(it.RunID)
		if err != nil || run.ProjectID != it.ProjectID || run.ChatSessionID != it.SessionID ||
			(it.WorkflowID != "" && run.WorkflowID != it.WorkflowID) ||
			(it.TaskID != "" && run.TaskID != it.TaskID) {
			return fmt.Errorf("交互 Run 归属不一致: %w", ErrInvalidState)
		}
	}
	if it.WorkflowID != "" {
		workflow, getErr := s.GetWorkflow(it.WorkflowID)
		if getErr != nil || workflow.ProjectID != it.ProjectID || workflow.ConversationID != it.SessionID {
			return fmt.Errorf("交互 Workflow 归属不一致: %w", ErrInvalidState)
		}
		if it.TaskID == "" && it.SourceRevision > 0 && workflow.Revision != it.SourceRevision {
			return ErrRevisionConflict
		}
	}
	if it.TaskID != "" {
		task, getErr := s.GetTask(it.TaskID)
		if getErr != nil || task.WorkflowID != it.WorkflowID {
			return fmt.Errorf("交互 Task 归属不一致: %w", ErrInvalidState)
		}
		if it.SourceRevision > 0 && task.Revision != it.SourceRevision {
			return ErrRevisionConflict
		}
	}
	if it.WorkflowID == "" && it.TaskID == "" && it.SourceRevision > 0 && session.Revision != it.SourceRevision {
		return ErrRevisionConflict
	}
	if it.MapID != "" {
		semanticMap, getErr := s.GetSemanticMap(it.ProjectID, it.MapID)
		if getErr != nil {
			return fmt.Errorf("交互 Map 归属不一致: %w", ErrInvalidState)
		}
		if semanticMap.Generation != it.MapGeneration {
			return ErrStaleMapGeneration
		}
	}
	if _, err := s.db.Exec(
		`INSERT INTO interactions (id, project_id, session_id, revision, agent,
			type, status, payload, reply, run_id, checkpoint_id, workflow_id, task_id,
			ui_kind, source_revision, map_id, map_generation, target_agent_id,
			response_schema, created_at, expires_at)
		 VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		it.ID, it.ProjectID, it.SessionID, it.Agent, it.Type, it.Status,
		it.Payload, it.Reply, it.RunID, it.CheckpointID, it.WorkflowID, it.TaskID,
		it.UIKind, it.SourceRevision, it.MapID, it.MapGeneration, it.TargetAgentID,
		it.ResponseSchema, it.CreatedAt, it.ExpiresAt,
	); err != nil {
		return fmt.Errorf("创建交互 %q 失败: %w", it.ID, err)
	}
	return nil
}

// GetInteraction 按 ID 查询交互，不存在时返回 ErrNotFound。
func (s *Store) GetInteraction(id string) (Interaction, error) {
	value, err := scanInteraction(s.db.QueryRow(`SELECT `+
		interactionSelectColumns+` FROM interactions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Interaction{}, ErrNotFound
	}
	if err != nil {
		return Interaction{}, fmt.Errorf("查询交互 %q 失败: %w", id, err)
	}
	return value, nil
}

// GetPendingInteractionByRunID 查询某次 Agent Run 已经创建的待回答问题。
// interaction.ask 成功后模型请求无需继续阻塞；Task Runtime 在 Run 收尾时用
// 这条持久事实判断 waiting_input，而不是再解析一份自然语言或 JSON 协议。
func (s *Store) GetPendingInteractionByRunID(runID string) (Interaction, error) {
	value, err := scanInteraction(s.db.QueryRow(`SELECT `+interactionSelectColumns+
		` FROM interactions WHERE run_id=? AND status=? ORDER BY created_at DESC LIMIT 1`,
		runID, InteractionStatusPending))
	if errors.Is(err, sql.ErrNoRows) {
		return Interaction{}, ErrNotFound
	}
	if err != nil {
		return Interaction{}, fmt.Errorf("查询 Run %q 的待回答交互失败: %w", runID, err)
	}
	return value, nil
}

// AnswerInteraction 把待应答交互迁移到 answered：写入应答负载与应答时间。
// 条件更新（WHERE status='pending'）：记录不存在或已终结时返回 ErrNotPending，
// 防重复应答在数据库层原子保证（并发应答只有一个成功）。
func (s *Store) AnswerInteraction(id, reply string, answeredAt time.Time) error {
	return s.AnswerInteractionAtRevision(id, reply, 0, answeredAt)
}

func (s *Store) AnswerInteractionAtRevision(id, reply string, sourceRevision int64, answeredAt time.Time) error {
	query := `UPDATE interactions SET status=?, reply=?, answered_at=?, revision=revision+1
		WHERE id=? AND status=? AND source_revision=?`
	args := []any{InteractionStatusAnswered, reply, answeredAt, id, InteractionStatusPending, sourceRevision}
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("应答交互失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotPending
	}
	return nil
}

// ExpireInteraction 把待应答交互迁移到 expired（超时路径）。
// 已应答的交互不再过期：条件更新同样保证只从 pending 迁移。
func (s *Store) ExpireInteraction(id string, expiredAt time.Time) error {
	return s.finalizeInteraction(id, InteractionStatusExpired, "", nil, &expiredAt)
}

// CancelInteraction 把待应答交互迁移到 cancelled（调用方取消路径）。
func (s *Store) CancelInteraction(id string, cancelledAt time.Time) error {
	if err := s.finalizeInteraction(id, InteractionStatusCancelled, "", nil, &cancelledAt); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE interactions SET cancelled_at=? WHERE id=?`, cancelledAt, id)
	return err
}

// finalizeInteraction 是 pending → 终态的条件更新（应答写 answered_at，
// 超时写 expired_at）；0 行命中说明已终结或不存在，返回 ErrNotPending。
func (s *Store) finalizeInteraction(id, status, reply string, answeredAt, expiredAt *time.Time) error {
	res, err := s.db.Exec(
		`UPDATE interactions SET status = ?, reply = ?, answered_at = ?, expired_at = ?,
		 revision = revision + 1 WHERE id = ? AND status = ?`,
		status, reply, answeredAt, expiredAt, id, InteractionStatusPending,
	)
	if err != nil {
		return fmt.Errorf("终结交互 %q（→%s）失败: %w", id, status, err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return ErrNotPending
	}
	return nil
}

// CancelPendingInteractions 把会话的全部待应答交互批量迁移到 cancelled
// （任务取消/会话删除路径），返回取消条数。
func (s *Store) CancelPendingInteractions(sessionID string, cancelledAt time.Time) (int64, error) {
	res, err := s.db.Exec(
		`UPDATE interactions SET status = ?, expired_at = ?,
		 revision = revision + 1 WHERE session_id = ? AND status = ?`,
		InteractionStatusCancelled, cancelledAt, sessionID, InteractionStatusPending,
	)
	if err != nil {
		return 0, fmt.Errorf("取消会话 %q 的待应答交互失败: %w", sessionID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取取消交互影响行数失败: %w", err)
	}
	return affected, nil
}

// ListPendingInteractions 查询会话的待应答交互（按创建时间升序）。
func (s *Store) ListPendingInteractions(sessionID string) ([]Interaction, error) {
	rows, err := s.db.Query(`SELECT `+interactionSelectColumns+
		` FROM interactions WHERE session_id = ? AND status = ? ORDER BY created_at`,
		sessionID, InteractionStatusPending)
	if err != nil {
		return nil, fmt.Errorf("查询会话 %q 的待应答交互失败: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()

	var interactions []Interaction
	for rows.Next() {
		value, err := scanInteraction(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描交互记录失败: %w", err)
		}
		interactions = append(interactions, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历交互列表失败: %w", err)
	}
	return interactions, nil
}

// InteractionFilter 是交互记录查询过滤器，零值字段不参与过滤。
type InteractionFilter struct {
	// OwnerID 按 Project 所有者过滤。HTTP 等用户入口必须设置此字段，避免
	// 未指定 Project 时返回其他用户的交互记录；内部维护任务可留空。
	OwnerID string

	// ProjectID 按 Project 精确过滤。
	ProjectID string

	// SessionID 按会话 ID 精确过滤。
	SessionID string

	// Status 按状态精确过滤（pending/answered/expired/cancelled）。
	Status string
}

// ListInteractions 按过滤器分页查询交互记录（按创建时间倒序，同刻按 ID 倒序），
// 返回本页与总条数。供审批卡恢复（status=pending）与交互历史视图使用。
func (s *Store) ListInteractions(f InteractionFilter, limit, offset int) ([]Interaction, int, error) {
	var where []string
	var args []any
	if f.OwnerID != "" {
		where = append(where, "project_id IN (SELECT id FROM projects WHERE owner_id = ?)")
		args = append(args, f.OwnerID)
	}
	if f.ProjectID != "" {
		where = append(where, "project_id = ?")
		args = append(args, f.ProjectID)
	}
	if f.SessionID != "" {
		where = append(where, "session_id = ?")
		args = append(args, f.SessionID)
	}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	whereClause := ""
	if len(where) > 0 {
		whereClause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM interactions`+whereClause, args...,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计交互记录失败: %w", err)
	}

	rows, err := s.db.Query(`SELECT `+interactionSelectColumns+
		` FROM interactions`+whereClause+` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("查询交互记录失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var interactions []Interaction
	for rows.Next() {
		value, err := scanInteraction(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("扫描交互记录失败: %w", err)
		}
		interactions = append(interactions, value)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("遍历交互记录失败: %w", err)
	}
	return interactions, total, nil
}

// MarkInteractionHandled 标记已回答或已取消Interaction的后续路由结果，
// 供重启时只处理一次。取消同样必须恢复原Agent，只是没有用户答案。
func (s *Store) MarkInteractionHandled(id string, handledAt time.Time, handlingError string) error {
	res, err := s.db.Exec(`UPDATE interactions SET handled_at=?,handling_error=?
		WHERE id=? AND status IN (?,?) AND handled_at IS NULL`, handledAt, handlingError,
		id, InteractionStatusAnswered, InteractionStatusCancelled)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrInvalidState
	}
	return nil
}

func (s *Store) RecordInteractionHandlingError(id, handlingError string) error {
	res, err := s.db.Exec(`UPDATE interactions SET handling_error=?
		WHERE id=? AND status IN (?,?) AND handled_at IS NULL`, handlingError, id,
		InteractionStatusAnswered, InteractionStatusCancelled)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrInvalidState
	}
	return nil
}

func (s *Store) ListUnhandledAnsweredInteractions() ([]Interaction, error) {
	rows, err := s.db.Query(`SELECT `+interactionSelectColumns+` FROM interactions
		WHERE status IN (?,?) AND handled_at IS NULL
		ORDER BY COALESCE(answered_at,cancelled_at),id`, InteractionStatusAnswered,
		InteractionStatusCancelled)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Interaction, 0)
	for rows.Next() {
		v, e := scanInteraction(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

func ValidInteractionUIKind(kind string) bool {
	switch kind {
	case InteractionUIForm, InteractionUISingleSelect, InteractionUIMultiSelect, InteractionUIParameter, InteractionUIImageSelect, InteractionUIMapSelect, InteractionUIFileSelect:
		return true
	}
	return false
}
