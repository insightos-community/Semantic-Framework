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
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
)

// Run 状态与 run_sessions.status 一致。旧库中的 done 与
// awaiting_approval 只允许由 v14 数据库迁移转换，运行时不再接受旧名称。
const (
	RunStatusQueued       = "queued"
	RunStatusRunning      = "running"
	RunStatusWaitingInput = "waiting_input"
	RunStatusCancelling   = "cancelling"
	RunStatusCompleted    = "completed"
	RunStatusFailed       = "failed"
	RunStatusCancelled    = "cancelled"
)

const (
	RunKindConversation  = "conversation"
	RunKindTaskPlanning  = "task_planning"
	RunKindTaskExecution = "task_execution"
)

// ChatSession 是一个用户对话会话（ChatSession，架构文档 04 §7.1 会话模型第一层）。
type ChatSession struct {
	// ID 会话唯一标识（cs- 前缀 + uuid）。
	ID string

	// UserID 会话归属的用户 ID（归属过滤的唯一依据）。
	UserID string

	// ProjectID 会话所属 Project；缺省时由 Store 归入用户 Default Project。
	ProjectID string

	// Title 会话标题（默认取首条消息前 20 字）。
	Title string

	// CreatedAt 创建时间。
	CreatedAt time.Time

	// ArchivedAt 归档时间；归档会话仍保留消息、Run 和 Trace。
	ArchivedAt *time.Time

	// Revision 用于 Studio 条件更新。
	Revision int64

	// UpdatedAt 最近一条消息的时间（会话列表按此倒序）。
	UpdatedAt time.Time
}

// ChatMessage 是持久化消息的领域薄信封。Message 是唯一消息语义，其他
// 字段只负责归属、审计和 Artifact 引用，不再重复保存 role/content。
type ChatMessage struct {
	// ID 消息唯一标识（msg-<纳秒时间戳>-<随机数>，字典序 ≈ 时间序）。
	ID string

	// SessionID 所属会话 ID。
	SessionID string

	// AgentID 生成或发送消息的 Agent；用户消息为空。
	AgentID string

	// RunID 助手消息对应的运行 ID；用户消息为空。
	RunID string

	// TraceID 生成该消息的链路 ID。
	TraceID string

	// Provider 实际模型服务 ID；用户消息为空。
	Provider string

	// Endpoint 实际模型端点 ID；用户消息为空。
	Endpoint string

	// Model 实际模型 ID；用户消息为空。
	Model string

	// Message 是 Eino 的统一消息对象；不能为空。
	Message *schema.Message

	// ArtifactRefs 是消息稳定引用的 Artifact ID，不把二进制内容写入消息 JSON。
	ArtifactRefs []string

	// Metadata 运行活动 JSON；无活动时为 {}。
	Metadata string

	// CreatedAt 写入时间。
	CreatedAt time.Time

	// StartedAt is the associated run's start, read without changing audit timestamps.
	StartedAt *time.Time
}

// RunSession 是一次 Agent 运行的持久记录。
type RunSession struct {
	ID                  string     `json:"id"`
	ProjectID           string     `json:"project_id"`
	ChatSessionID       string     `json:"conversation_id"`
	WorkflowID          string     `json:"workflow_id,omitempty"`
	TaskID              string     `json:"task_id,omitempty"`
	SourceInteractionID string     `json:"-"`
	Kind                string     `json:"kind"`
	ContextID           string     `json:"context_id"`
	AgentID             string     `json:"agent_id"`
	RobotID             string     `json:"robot_id,omitempty"`
	AgentName           string     `json:"agent_name"`
	Provider            string     `json:"provider,omitempty"`
	Endpoint            string     `json:"endpoint,omitempty"`
	Model               string     `json:"model,omitempty"`
	TraceID             string     `json:"trace_id,omitempty"`
	Status              string     `json:"status"`
	Error               string     `json:"error,omitempty"`
	Revision            int64      `json:"revision"`
	StartedAt           time.Time  `json:"started_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	EndedAt             *time.Time `json:"ended_at,omitempty"`
}

// NewChatSessionID 生成会话 ID（cs- 前缀 + uuid）。
func NewChatSessionID() string {
	return "cs-" + uuid.NewString()
}

// NewRunSessionID 生成运行会话 ID（run- 前缀 + uuid）。
func NewRunSessionID() string {
	return "run-" + uuid.NewString()
}

// NewChatMessageID 生成消息 ID：msg-<19 位纳秒时间戳>-<8 字节随机数十六进制>。
// 为什么不用 uuid：消息列表按 id 排序即得时间序（断连续传、历史重放都依赖
// 该顺序），固定宽度的时间戳前缀保证字典序与时间序一致；随机段消解同纳秒冲突。
var lastMessageTimestamp atomic.Int64

func monotonicMessageTimestamp(last *atomic.Int64, now int64) int64 {
	for {
		previous := last.Load()
		next := now
		if next <= previous {
			next = previous + 1
		}
		if last.CompareAndSwap(previous, next) {
			return next
		}
	}
}

func NewChatMessageID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("msg-%019d-%s", monotonicMessageTimestamp(&lastMessageTimestamp, time.Now().UnixNano()), hex.EncodeToString(b))
}

// CreateChatSession 写入一个新会话。普通用户未指定 Project 时使用当前
// 活动 Project；显式指定非活动 Project 会被拒绝，防止后台页面向错误项目写入。
func (s *Store) CreateChatSession(sess ChatSession) error {
	explicitProject := sess.ProjectID != ""
	var project Project
	var err error
	if !explicitProject {
		if sess.UserID == "" {
			project, err = s.EnsureDefaultProject("")
		} else {
			project, err = s.GetActiveProject(sess.UserID)
			if errors.Is(err, ErrNotFound) {
				project, err = s.EnsureDefaultProject(sess.UserID)
			}
		}
	} else {
		project, err = s.GetProject(sess.ProjectID)
	}
	if err != nil {
		return fmt.Errorf("会话 Project 不存在: %w", err)
	}
	if project.OwnerID != sess.UserID {
		return fmt.Errorf("会话 Project %q 不属于用户: %w", project.ID, ErrNotFound)
	}
	if project.ArchivedAt != nil {
		return ErrProjectArchived
	}
	if sess.UserID != "" && !project.IsActive {
		if explicitProject {
			return ErrProjectInactive
		}
		// 旧调用面没有 project_id，会显式切换到该用户的 Default Project；
		// v0.2 Studio 始终传 Project，不走这条兼容路径。
		project, err = s.ActivateProject(sess.UserID, project.ID)
		if err != nil {
			return err
		}
	}
	// 解析和兼容激活完成后再取得门闩，避免递归调用 ActivateProject。门闩内
	// 必须重新读取 Project；若另一个请求在两步之间完成切换或归档，本次创建
	// 会明确失败，不会向已经失效的 Project 插入 Conversation。
	s.projectWriteMu.Lock()
	defer s.projectWriteMu.Unlock()
	project, err = s.GetProject(project.ID)
	if err != nil {
		return fmt.Errorf("会话 Project 不存在: %w", err)
	}
	if project.OwnerID != sess.UserID {
		return fmt.Errorf("会话 Project %q 不属于用户: %w", project.ID, ErrNotFound)
	}
	if project.ArchivedAt != nil {
		return ErrProjectArchived
	}
	if sess.UserID != "" && !project.IsActive {
		return ErrProjectInactive
	}
	sess.ProjectID = project.ID
	if _, err := s.db.Exec(
		`INSERT INTO chat_sessions (id, user_id, project_id, title, revision, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 1, ?, ?)`,
		sess.ID, sess.UserID, sess.ProjectID, sess.Title, sess.CreatedAt, sess.UpdatedAt,
	); err != nil {
		return fmt.Errorf("创建会话 %q 失败: %w", sess.ID, err)
	}
	return nil
}

// GetChatSession 按 ID 查询会话，不存在时返回 ErrNotFound。
func (s *Store) GetChatSession(id string) (ChatSession, error) {
	var sess ChatSession
	err := s.db.QueryRow(
		`SELECT id, user_id, project_id, title, archived_at, revision, created_at, updated_at
		 FROM chat_sessions WHERE id = ?`, id,
	).Scan(&sess.ID, &sess.UserID, &sess.ProjectID, &sess.Title, &sess.ArchivedAt,
		&sess.Revision, &sess.CreatedAt, &sess.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatSession{}, ErrNotFound
	}
	if err != nil {
		return ChatSession{}, fmt.Errorf("查询会话 %q 失败: %w", id, err)
	}
	return sess, nil
}

// requireWritableConversation 在同一次查询中读取 Conversation 与 Project，
// 避免调用方分别查询后把两个时刻的状态拼在一起。用户归属不匹配统一按
// ErrNotFound 返回；归档和非活动状态保留明确错误，供写入口给出可操作提示。
func (s *Store) requireWritableConversation(ownerID, sessionID string) (ChatSession, Project, error) {
	var session ChatSession
	var project Project
	var projectDefault, projectActive int
	err := s.db.QueryRow(`SELECT
		c.id, c.user_id, c.project_id, c.title, c.archived_at, c.revision,
		c.created_at, c.updated_at,
		p.id, p.owner_id, p.name, p.workspace_root, p.is_default, p.mode,
		p.is_active, p.archived_at, p.revision, p.created_at, p.updated_at
		FROM chat_sessions c JOIN projects p ON p.id = c.project_id
		WHERE c.id = ? AND c.user_id = ? AND p.owner_id = ?`,
		sessionID, ownerID, ownerID).Scan(
		&session.ID, &session.UserID, &session.ProjectID, &session.Title,
		&session.ArchivedAt, &session.Revision, &session.CreatedAt,
		&session.UpdatedAt,
		&project.ID, &project.OwnerID, &project.Name, &project.WorkspaceRoot,
		&projectDefault, &project.Mode, &projectActive, &project.ArchivedAt,
		&project.Revision, &project.CreatedAt, &project.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatSession{}, Project{}, ErrNotFound
	}
	if err != nil {
		return ChatSession{}, Project{}, fmt.Errorf("检查 Conversation 写入状态失败: %w", err)
	}
	project.IsDefault = projectDefault != 0
	project.IsActive = projectActive != 0
	if session.ArchivedAt != nil {
		return session, project, ErrConversationArchived
	}
	if project.ArchivedAt != nil {
		return session, project, ErrProjectArchived
	}
	if !project.IsActive {
		return session, project, ErrProjectInactive
	}
	return session, project, nil
}

// RequireWritableConversation 检查一次 Conversation 写操作能否开始。
// 需要在校验后连续写入消息、Run 或配置的调用方应使用
// WithWritableConversation，避免校验后 Project 被切换或归档。
func (s *Store) RequireWritableConversation(ownerID, sessionID string) (ChatSession, Project, error) {
	s.projectWriteMu.Lock()
	defer s.projectWriteMu.Unlock()
	return s.requireWritableConversation(ownerID, sessionID)
}

// WithWritableConversation 在 Project/Conversation 状态保持不变的窗口内
// 执行写操作。回调中不能再次调用本方法或 Project/Conversation 归档、激活
// 方法；这些入口使用同一把锁，递归调用会造成死锁。
func (s *Store) WithWritableConversation(ownerID, sessionID string,
	write func(ChatSession, Project) error) error {
	s.projectWriteMu.Lock()
	defer s.projectWriteMu.Unlock()
	session, project, err := s.requireWritableConversation(ownerID, sessionID)
	if err != nil {
		return err
	}
	return write(session, project)
}

// ListChatSessionsByUser 查询某用户的全部会话，按 updated_at 倒序（最近活跃在前）。
func (s *Store) ListChatSessionsByUser(userID string) ([]ChatSession, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, project_id, title, archived_at, revision, created_at, updated_at
		 FROM chat_sessions WHERE user_id = ? AND archived_at IS NULL ORDER BY updated_at DESC`, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("查询用户 %q 的会话列表失败: %w", userID, err)
	}
	defer func() { _ = rows.Close() }()

	var sessions []ChatSession
	for rows.Next() {
		var sess ChatSession
		if err := rows.Scan(&sess.ID, &sess.UserID, &sess.ProjectID, &sess.Title,
			&sess.ArchivedAt, &sess.Revision, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
			return nil, fmt.Errorf("扫描会话失败: %w", err)
		}
		sessions = append(sessions, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历会话列表失败: %w", err)
	}
	return sessions, nil
}

// ListChatSessionsByProject 返回 Project 中未归档的 Conversation。
func (s *Store) ListChatSessionsByProject(projectID string) ([]ChatSession, error) {
	return s.ListChatSessionsByProjectIncludingArchived(projectID, false)
}

// ListChatSessionsByProjectIncludingArchived 返回 Project 中的 Conversation。
// includeArchived=false 时只返回仍可使用的会话；归档箱显式传 true，避免普通
// Studio Snapshot 把已归档会话重新带回活动列表。
func (s *Store) ListChatSessionsByProjectIncludingArchived(projectID string,
	includeArchived bool) ([]ChatSession, error) {
	query := `SELECT id, user_id, project_id, title, archived_at,
		revision, created_at, updated_at FROM chat_sessions WHERE project_id = ?`
	if !includeArchived {
		query += ` AND archived_at IS NULL`
	}
	query += ` ORDER BY updated_at DESC`
	rows, err := s.db.Query(query, projectID)
	if err != nil {
		return nil, fmt.Errorf("查询 Project %q 的会话失败: %w", projectID, err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]ChatSession, 0)
	for rows.Next() {
		var session ChatSession
		if err := rows.Scan(&session.ID, &session.UserID, &session.ProjectID,
			&session.Title, &session.ArchivedAt, &session.Revision,
			&session.CreatedAt, &session.UpdatedAt); err != nil {
			return nil, fmt.Errorf("扫描 Project 会话失败: %w", err)
		}
		result = append(result, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 Project 会话失败: %w", err)
	}
	return result, nil
}

// ArchiveConversation 只隐藏会话，不删除消息、Run、Interaction 或 Trace。
// 活动工作检查与归档更新在同一事务和 projectWriteMu 临界区内完成，避免检查
// 之后又启动新 Run 的竞态。
func (s *Store) ArchiveConversation(ownerID, projectID, sessionID string,
	archivedAt time.Time) (ChatSession, error) {
	s.projectWriteMu.Lock()
	defer s.projectWriteMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return ChatSession{}, fmt.Errorf("开启 Conversation 归档事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var session ChatSession
	var projectActive int
	var projectArchived *time.Time
	err = tx.QueryRow(`SELECT c.id, c.user_id, c.project_id, c.title, c.archived_at,
		c.revision, c.created_at, c.updated_at, p.is_active, p.archived_at
		FROM chat_sessions c JOIN projects p ON p.id = c.project_id
		WHERE c.id = ? AND c.user_id = ? AND c.project_id = ? AND p.owner_id = ?`,
		sessionID, ownerID, projectID, ownerID).Scan(
		&session.ID, &session.UserID, &session.ProjectID, &session.Title,
		&session.ArchivedAt, &session.Revision, &session.CreatedAt,
		&session.UpdatedAt, &projectActive, &projectArchived)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatSession{}, ErrNotFound
	}
	if err != nil {
		return ChatSession{}, fmt.Errorf("查询待归档 Conversation 失败: %w", err)
	}
	if session.ArchivedAt != nil {
		return session, nil
	}
	if projectArchived != nil {
		return ChatSession{}, ErrProjectArchived
	}
	if projectActive == 0 {
		return ChatSession{}, ErrProjectInactive
	}
	var hasActiveRun, hasPendingInteraction int
	if err := tx.QueryRow(`SELECT
		EXISTS(SELECT 1 FROM run_sessions WHERE chat_session_id = ? AND status IN (?, ?, ?, ?)),
		EXISTS(SELECT 1 FROM interactions WHERE session_id = ? AND status = ?)`,
		sessionID, RunStatusQueued, RunStatusRunning, RunStatusWaitingInput,
		RunStatusCancelling, sessionID, InteractionStatusPending).Scan(
		&hasActiveRun, &hasPendingInteraction); err != nil {
		return ChatSession{}, fmt.Errorf("检查 Conversation 未结束工作失败: %w", err)
	}
	if hasActiveRun != 0 || hasPendingInteraction != 0 {
		return ChatSession{}, ErrConversationHasActiveWork
	}
	result, err := tx.Exec(`UPDATE chat_sessions SET archived_at = ?,
		revision = revision + 1, updated_at = ?
		WHERE id = ? AND user_id = ? AND project_id = ? AND archived_at IS NULL`,
		archivedAt, archivedAt, sessionID, ownerID, projectID)
	if err != nil {
		return ChatSession{}, fmt.Errorf("归档会话 %q 失败: %w", sessionID, err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ChatSession{}, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return ChatSession{}, fmt.Errorf("提交 Conversation 归档事务失败: %w", err)
	}
	return s.GetChatSession(sessionID)
}

// ArchiveChatSession 保留旧 /chat 迁移入口的兼容行为。该入口可能操作不再
// 活动的历史 Project；Studio 必须使用 ArchiveConversation 的严格检查。
func (s *Store) ArchiveChatSession(ownerID, projectID, sessionID string,
	archivedAt time.Time) error {
	s.projectWriteMu.Lock()
	defer s.projectWriteMu.Unlock()
	result, err := s.db.Exec(`UPDATE chat_sessions SET archived_at = ?,
		revision = revision + 1, updated_at = ?
		WHERE id = ? AND user_id = ? AND project_id = ? AND archived_at IS NULL`,
		archivedAt, archivedAt, sessionID, ownerID, projectID)
	if err != nil {
		return fmt.Errorf("归档会话 %q 失败: %w", sessionID, err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteChatSession 删除会话及其全部消息；run_sessions 保留（归档语义，
// 供追踪与复盘查询）。不存在时不报错（删除幂等）。
func (s *Store) DeleteChatSession(id string) error {
	if _, err := s.db.Exec(`DELETE FROM session_execution_policies WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("删除会话 %q 的执行策略失败: %w", id, err)
	}
	if _, err := s.db.Exec(`DELETE FROM session_agent_models WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("删除会话 %q 的 Agent 模型快照失败: %w", id, err)
	}
	if _, err := s.db.Exec(`DELETE FROM chat_messages WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("删除会话 %q 的消息失败: %w", id, err)
	}
	if _, err := s.db.Exec(`DELETE FROM chat_sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("删除会话 %q 失败: %w", id, err)
	}
	return nil
}

// TouchChatSession 把会话的 updated_at 推进到指定时间（每次消息活动后调用）。
func (s *Store) TouchChatSession(id string, ts time.Time) error {
	if _, err := s.db.Exec(`UPDATE chat_sessions SET updated_at = ?,
		revision = revision + 1 WHERE id = ?`, ts, id); err != nil {
		return fmt.Errorf("更新会话 %q 活动时间失败: %w", id, err)
	}
	return nil
}

// UpdateChatSessionTitle 更新会话标题。标题生成属于运行时策略，存储层只负责持久化。
func (s *Store) UpdateChatSessionTitle(id, title string) error {
	if _, err := s.db.Exec(`UPDATE chat_sessions SET title = ?,
		revision = revision + 1, updated_at = ? WHERE id = ?`,
		title, time.Now().UTC(), id); err != nil {
		return fmt.Errorf("更新会话 %q 标题失败: %w", id, err)
	}
	return nil
}

// AppendChatMessage 追加一条会话消息。
func (s *Store) AppendChatMessage(msg ChatMessage) error {
	if msg.Message == nil {
		return errors.New("写入会话消息失败: schema.Message 不能为空")
	}
	if msg.Message.Role == schema.Assistant && msg.Message.Content == "" &&
		len(msg.Message.ToolCalls) == 0 && len(msg.Message.AssistantGenMultiContent) == 0 {
		return errors.New("写入会话消息失败: Assistant 必须包含 content、tool_calls 或多模态输出")
	}
	messageJSON, err := json.Marshal(msg.Message)
	if err != nil {
		return fmt.Errorf("序列化会话消息失败: %w", err)
	}
	artifactJSON, err := json.Marshal(msg.ArtifactRefs)
	if err != nil {
		return fmt.Errorf("序列化消息 ArtifactRef 失败: %w", err)
	}
	if msg.Metadata == "" {
		msg.Metadata = "{}"
	}
	if _, err := s.db.Exec(
		`INSERT INTO chat_messages (id, session_id, agent_id, run_id, trace_id, provider,
		 endpoint, model, message_json, artifact_refs, metadata, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.SessionID, msg.AgentID, msg.RunID, msg.TraceID, msg.Provider,
		msg.Endpoint, msg.Model, string(messageJSON), string(artifactJSON), msg.Metadata, msg.CreatedAt,
	); err != nil {
		return fmt.Errorf("写入会话 %q 的消息失败: %w", msg.SessionID, err)
	}
	return nil
}

// ListChatMessages 按会话查询消息，按 id 升序（即时间序）返回。
// limit <= 0 时返回全部（历史重放路径）；否则按 limit/offset 分页（REST 路径）。
func (s *Store) ListChatMessages(sessionID string, limit, offset int) ([]ChatMessage, error) {
	query := `SELECT m.id, m.session_id, m.agent_id, m.run_id, m.trace_id, m.provider, m.endpoint,
		m.model, m.message_json, m.artifact_refs, m.metadata, m.created_at, r.started_at
		FROM chat_messages m LEFT JOIN run_sessions r ON r.id = m.run_id
		WHERE m.session_id = ? ORDER BY m.id`
	args := []any{sessionID}
	if limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	return s.listChatMessagesQuery(query, args...)
}

// LatestTaskPlanningMessage returns the public message for the latest completed
// planning run of this exact Task, not another concurrently planned Task.
func (s *Store) LatestTaskPlanningMessage(taskID string) (*ChatMessage, error) {
	rows, err := s.listChatMessagesQuery(`SELECT m.id, m.session_id, m.agent_id, m.run_id, m.trace_id, m.provider, m.endpoint,
		m.model, m.message_json, m.artifact_refs, m.metadata, m.created_at, r.started_at
		FROM chat_messages m LEFT JOIN run_sessions r ON r.id = m.run_id
		WHERE m.run_id = (SELECT id FROM run_sessions WHERE task_id = ? AND kind = 'task_planning'
		AND status = 'completed' ORDER BY started_at DESC LIMIT 1) ORDER BY m.created_at DESC LIMIT 1`, taskID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

// listChatMessagesQuery 统一反序列化消息，供完整历史与摘要边界查询复用。
func (s *Store) listChatMessagesQuery(query string, args ...any) ([]ChatMessage, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询会话消息失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var messages []ChatMessage
	for rows.Next() {
		var msg ChatMessage
		var messageJSON, artifactJSON string
		if err := rows.Scan(&msg.ID, &msg.SessionID, &msg.AgentID, &msg.RunID,
			&msg.TraceID, &msg.Provider, &msg.Endpoint, &msg.Model, &messageJSON,
			&artifactJSON, &msg.Metadata, &msg.CreatedAt, &msg.StartedAt); err != nil {
			return nil, fmt.Errorf("扫描会话消息失败: %w", err)
		}
		msg.Message = &schema.Message{}
		if err := json.Unmarshal([]byte(messageJSON), msg.Message); err != nil {
			return nil, fmt.Errorf("解析会话消息 %q 失败: %w", msg.ID, err)
		}
		if err := json.Unmarshal([]byte(artifactJSON), &msg.ArtifactRefs); err != nil {
			return nil, fmt.Errorf("解析消息 %q 的 ArtifactRef 失败: %w", msg.ID, err)
		}
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历会话消息失败: %w", err)
	}
	return messages, nil
}

// CountChatMessages 统计会话的消息总数（分页响应的 total 字段）。
func (s *Store) CountChatMessages(sessionID string) (int, error) {
	var total int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM chat_messages WHERE session_id = ?`, sessionID,
	).Scan(&total); err != nil {
		return 0, fmt.Errorf("统计会话 %q 的消息数失败: %w", sessionID, err)
	}
	return total, nil
}

// RunFilter 是 Run 列表过滤条件；空字段不参与查询。
type RunFilter struct {
	ProjectID     string
	ChatSessionID string
	Status        string
}

// ValidRunStatus 判断状态是否属于 v0.2 Run 状态机。
func ValidRunStatus(status string) bool {
	switch status {
	case RunStatusQueued, RunStatusRunning, RunStatusWaitingInput,
		RunStatusCancelling, RunStatusCompleted, RunStatusFailed, RunStatusCancelled:
		return true
	default:
		return false
	}
}
func terminalRunStatus(status string) bool {
	return status == RunStatusCompleted || status == RunStatusFailed ||
		status == RunStatusCancelled
}

// CreateRunSession 创建 Run，并从 Conversation 补齐 Project 和 Context。
func (s *Store) CreateRunSession(run RunSession) error {
	if !ValidRunStatus(run.Status) {
		return ErrInvalidState
	}
	session, err := s.GetChatSession(run.ChatSessionID)
	if err == nil {
		if run.ProjectID == "" {
			run.ProjectID = session.ProjectID
		}
		if run.ProjectID != session.ProjectID {
			return fmt.Errorf("Run 与 Conversation 的 Project 不一致: %w", ErrInvalidState)
		}
	} else if errors.Is(err, ErrNotFound) && run.ProjectID == "" {
		// 兼容系统诊断和旧测试直接写入的归档 Run；公共 API 不允许这种记录。
		run.ProjectID = SystemDefaultProjectID
	} else if err != nil {
		return fmt.Errorf("创建 Run 的 Conversation 不存在: %w", err)
	}
	if run.ContextID == "" {
		run.ContextID = "leader:" + run.ChatSessionID
	}
	if run.AgentID == "" {
		run.AgentID = run.AgentName
	}
	if run.AgentName == "" {
		run.AgentName = run.AgentID
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
	}
	if run.UpdatedAt.IsZero() {
		run.UpdatedAt = run.StartedAt
	}
	if run.Kind == "" {
		run.Kind = RunKindConversation
	}
	if run.Kind != RunKindConversation && run.Kind != RunKindTaskPlanning && run.Kind != RunKindTaskExecution {
		return ErrInvalidState
	}
	if run.TaskID != "" {
		task, err := s.GetTask(run.TaskID)
		if err != nil || task.WorkflowID != run.WorkflowID || task.ContextID != run.ContextID || task.AssignedAgentID != run.AgentID {
			return fmt.Errorf("Task Run 归属不一致: %w", ErrInvalidState)
		}
		workflow, err := s.GetWorkflow(run.WorkflowID)
		if err != nil || workflow.ProjectID != run.ProjectID || workflow.ConversationID != run.ChatSessionID {
			return fmt.Errorf("Task Run 的 Workflow 归属不一致: %w", ErrInvalidState)
		}
		if run.Kind != RunKindTaskPlanning && run.Kind != RunKindTaskExecution {
			return fmt.Errorf("Task Run kind 非法: %w", ErrInvalidState)
		}
	} else if run.Kind == RunKindTaskPlanning || run.Kind == RunKindTaskExecution {
		return fmt.Errorf("Task Run 必须关联 task_id: %w", ErrInvalidState)
	}
	_, err = s.db.Exec(`INSERT INTO run_sessions
		(id, project_id, chat_session_id, workflow_id, task_id, source_interaction_id, kind, context_id, agent_id, agent_name,
		 provider, endpoint, model, trace_id, status, error, revision,
		 started_at, updated_at, ended_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
		run.ID, run.ProjectID, run.ChatSessionID, run.WorkflowID, run.TaskID, run.SourceInteractionID, run.Kind, run.ContextID, run.AgentID,
		run.AgentName, run.Provider, run.Endpoint, run.Model, run.TraceID,
		run.Status, run.Error, run.StartedAt, run.UpdatedAt, run.EndedAt)
	if err != nil {
		return fmt.Errorf("创建 run session %q 失败: %w", run.ID, err)
	}
	return nil
}

type runScanner interface {
	Scan(dest ...any) error
}

func scanRun(scanner runScanner) (RunSession, error) {
	var run RunSession
	err := scanner.Scan(&run.ID, &run.ProjectID, &run.ChatSessionID,
		&run.WorkflowID, &run.TaskID, &run.SourceInteractionID, &run.Kind, &run.ContextID, &run.AgentID, &run.RobotID, &run.AgentName, &run.Provider,
		&run.Endpoint, &run.Model, &run.TraceID, &run.Status, &run.Error,
		&run.Revision, &run.StartedAt, &run.UpdatedAt, &run.EndedAt)
	return run, err
}

const runSelectColumns = `id, project_id, chat_session_id, workflow_id, task_id, source_interaction_id, kind, context_id, agent_id,
	robot_id, agent_name, provider, endpoint, model, trace_id, status, error, revision,
	started_at, updated_at, ended_at`

// GetRunSession 按 ID 查询 Run。
func (s *Store) GetRunSession(id string) (RunSession, error) {
	run, err := scanRun(s.db.QueryRow(`SELECT `+runSelectColumns+
		` FROM run_sessions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return RunSession{}, ErrNotFound
	}
	if err != nil {
		return RunSession{}, fmt.Errorf("查询 run session %q 失败: %w", id, err)
	}
	return run, nil
}

func (s *Store) GetRunSessionBySourceInteraction(interactionID string) (RunSession, error) {
	if strings.TrimSpace(interactionID) == "" {
		return RunSession{}, ErrNotFound
	}
	run, err := scanRun(s.db.QueryRow(`SELECT `+runSelectColumns+
		` FROM run_sessions WHERE source_interaction_id=?
		 ORDER BY CASE WHEN status IN ('queued','running','waiting_input','cancelling') THEN 0 ELSE 1 END,
		 started_at DESC,id DESC LIMIT 1`, interactionID))
	if errors.Is(err, sql.ErrNoRows) {
		return RunSession{}, ErrNotFound
	}
	return run, err
}

// GetWorkflowSummaryRun 返回 Workflow 终态对应的唯一 Leader 总结 Run。
// 该查询复用既有 RunSession 字段，不为一次展示性动作增加新的领域对象。
func (s *Store) GetWorkflowSummaryRun(workflowID string) (RunSession, error) {
	if strings.TrimSpace(workflowID) == "" {
		return RunSession{}, ErrNotFound
	}
	run, err := scanRun(s.db.QueryRow(`SELECT `+runSelectColumns+
		` FROM run_sessions WHERE workflow_id=? AND task_id='' AND kind='conversation'
		 ORDER BY started_at DESC,id DESC LIMIT 1`, workflowID))
	if errors.Is(err, sql.ErrNoRows) {
		return RunSession{}, ErrNotFound
	}
	return run, err
}

// UpdateRunExecutionMetadata 让 Runtime 接管 Workflow Service 预留的 queued/
// running Run。它只补齐实际模型与 Trace，不创建第二条 Run。
func (s *Store) UpdateRunExecutionMetadata(id, provider, endpoint, model, traceID string,
	updatedAt time.Time) (RunSession, error) {
	if strings.TrimSpace(traceID) == "" {
		return RunSession{}, ErrInvalidState
	}
	result, err := s.db.Exec(`UPDATE run_sessions SET provider=?,endpoint=?,model=?,trace_id=?,
		revision=revision+1,updated_at=? WHERE id=? AND status IN (?,?)`, provider, endpoint,
		model, traceID, updatedAt, id, RunStatusQueued, RunStatusRunning)
	if err != nil {
		return RunSession{}, fmt.Errorf("更新 Run 执行信息失败: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return RunSession{}, ErrInvalidState
	}
	return s.GetRunSession(id)
}

// GetRunSessionByTraceID 返回精确关联该 Trace 的 Run。v0.2 每个 Agent
// Run 有独立 Trace，因此多行表示数据损坏，查询只取最近一行用于诊断。
func (s *Store) GetRunSessionByTraceID(traceID string) (RunSession, error) {
	run, err := scanRun(s.db.QueryRow(`SELECT `+runSelectColumns+
		` FROM run_sessions WHERE trace_id = ?
		 ORDER BY started_at DESC LIMIT 1`, traceID))
	if errors.Is(err, sql.ErrNoRows) {
		return RunSession{}, ErrNotFound
	}
	if err != nil {
		return RunSession{}, fmt.Errorf("按 Trace 查询 Run 失败: %w", err)
	}
	return run, nil
}

// ListRunSessions 分页返回 Run，按开始时间倒序。
func (s *Store) ListRunSessions(filter RunFilter, limit, offset int) ([]RunSession, int, error) {
	where := make([]string, 0, 3)
	args := make([]any, 0, 5)
	if filter.ProjectID != "" {
		where = append(where, "project_id = ?")
		args = append(args, filter.ProjectID)
	}
	if filter.ChatSessionID != "" {
		where = append(where, "chat_session_id = ?")
		args = append(args, filter.ChatSessionID)
	}
	if filter.Status != "" {
		if !ValidRunStatus(filter.Status) {
			return nil, 0, ErrInvalidState
		}
		where = append(where, "status = ?")
		args = append(args, filter.Status)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM run_sessions`+clause,
		args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计 Run 失败: %w", err)
	}
	query := `SELECT ` + runSelectColumns + ` FROM run_sessions` + clause +
		` ORDER BY started_at DESC, id DESC`
	if limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("查询 Run 列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]RunSession, 0)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("扫描 Run 失败: %w", err)
		}
		result = append(result, run)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("遍历 Run 失败: %w", err)
	}
	return result, total, nil
}

func statusPlaceholders(statuses []string) (string, []any, error) {
	if len(statuses) == 0 {
		statuses = []string{RunStatusQueued, RunStatusRunning,
			RunStatusWaitingInput, RunStatusCancelling}
	}
	placeholders := make([]string, 0, len(statuses))
	args := make([]any, 0, len(statuses))
	for _, status := range statuses {
		if !ValidRunStatus(status) {
			return "", nil, ErrInvalidState
		}
		placeholders = append(placeholders, "?")
		args = append(args, status)
	}
	return strings.Join(placeholders, ","), args, nil
}

// TransitionRunStatus 只从给定状态迁移，供精确取消和等待输入使用。
func (s *Store) TransitionRunStatus(id string, from []string, to string, updatedAt time.Time) (RunSession, error) {
	if !ValidRunStatus(to) || terminalRunStatus(to) {
		return RunSession{}, ErrInvalidState
	}
	placeholders, args, err := statusPlaceholders(from)
	if err != nil {
		return RunSession{}, err
	}
	queryArgs := []any{to, updatedAt, id}
	queryArgs = append(queryArgs, args...)
	result, err := s.db.Exec(`UPDATE run_sessions SET status = ?,
		revision = revision + 1, updated_at = ? WHERE id = ? AND status IN (`+
		placeholders+`)`, queryArgs...)
	if err != nil {
		return RunSession{}, fmt.Errorf("迁移 Run %q 状态失败: %w", id, err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		if _, getErr := s.GetRunSession(id); getErr != nil {
			return RunSession{}, getErr
		}
		return RunSession{}, ErrInvalidState
	}
	return s.GetRunSession(id)
}

// FinishRunSession 从指定非终态结束 Run，写入错误和结束时间。
func (s *Store) FinishRunSession(id string, from []string, status, errorText string, endedAt time.Time) (RunSession, error) {
	if !ValidRunStatus(status) || !terminalRunStatus(status) {
		return RunSession{}, ErrInvalidState
	}
	placeholders, args, err := statusPlaceholders(from)
	if err != nil {
		return RunSession{}, err
	}
	queryArgs := []any{status, errorText, endedAt, endedAt, id}
	queryArgs = append(queryArgs, args...)
	result, err := s.db.Exec(`UPDATE run_sessions SET status = ?, error = ?,
		ended_at = ?, updated_at = ?, revision = revision + 1
		WHERE id = ? AND status IN (`+placeholders+`)`, queryArgs...)
	if err != nil {
		return RunSession{}, fmt.Errorf("结束 Run %q 失败: %w", id, err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		if _, getErr := s.GetRunSession(id); getErr != nil {
			return RunSession{}, getErr
		}
		return RunSession{}, ErrInvalidState
	}
	return s.GetRunSession(id)
}

// EndRunSession 保留旧 Runtime 的调用面；状态已经统一为 v0.2 名称。
func (s *Store) EndRunSession(id, status string, endedAt time.Time) error {
	_, err := s.FinishRunSession(id, nil, status, "", endedAt)
	return err
}

// UpdateRunSessionStatus 保留旧 Runtime 的中间态调用面。
func (s *Store) UpdateRunSessionStatus(id, status string) error {
	_, err := s.TransitionRunStatus(id, nil, status, time.Now().UTC())
	return err
}

// MarkInterruptedRuns 在 Server 启动后收敛无法继续的旧进程 Run。
func (s *Store) MarkInterruptedRuns(now time.Time) (failed, cancelled int64, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("开启 Run 恢复事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	failedResult, err := tx.Exec(`UPDATE run_sessions SET status = ?, error = ?,
		ended_at = ?, updated_at = ?, revision = revision + 1
		WHERE status IN (?, ?, ?)`, RunStatusFailed, "Server 重启，原运行已结束",
		now, now, RunStatusQueued, RunStatusRunning, RunStatusWaitingInput)
	if err != nil {
		return 0, 0, fmt.Errorf("收敛中断 Run 失败: %w", err)
	}
	cancelledResult, err := tx.Exec(`UPDATE run_sessions SET status = ?,
		ended_at = ?, updated_at = ?, revision = revision + 1
		WHERE status = ?`, RunStatusCancelled, now, now, RunStatusCancelling)
	if err != nil {
		return 0, 0, fmt.Errorf("收敛取消中 Run 失败: %w", err)
	}
	failed, _ = failedResult.RowsAffected()
	cancelled, _ = cancelledResult.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("提交 Run 恢复事务失败: %w", err)
	}
	return failed, cancelled, nil
}

// SaveRunCheckpoint 保存 Run 的内核断点。checkpoint 不属于公共 Run 视图，
// 因此不能推进 revision/updated_at；否则 Studio 会看到没有对应资源事件的
// revision 缺口，并把一次内部存档误判为业务状态变化。
func (s *Store) SaveRunCheckpoint(id string, checkpoint []byte) error {
	res, err := s.db.Exec(`UPDATE run_sessions SET checkpoint = ? WHERE id = ?`,
		checkpoint, id)
	if err != nil {
		return fmt.Errorf("保存 run session %q 的断点失败: %w", id, err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return fmt.Errorf("保存 run session %q 的断点失败: %w", id, ErrNotFound)
	}
	return nil
}

// GetRunCheckpoint 读取 run 的内核断点；第二个返回值表示断点是否存在。
func (s *Store) GetRunCheckpoint(id string) ([]byte, bool, error) {
	var checkpoint []byte
	err := s.db.QueryRow(`SELECT checkpoint FROM run_sessions WHERE id = ?`, id).Scan(&checkpoint)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("读取 run session %q 的断点失败: %w", id, err)
	}
	if checkpoint == nil {
		return nil, false, nil
	}
	return checkpoint, true, nil
}
