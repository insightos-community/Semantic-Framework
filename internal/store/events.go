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

// ChannelTrace 是可丢不补的 trace 频道取值（与 ws.ChannelTrace 的协议值一致）。
// 为什么复制字面量而不是引用 ws 包：store 是底层包，ws 依赖 store 方向
// 已成形（经 aggregate），反向引用即包循环；协议值变更本就是破坏性升级。
const ChannelTrace = "trace"

// Event 是一条落库的下行事件（envelope 的持久化形态，迁移 v5 events 表）。
// 字段全部是标量/JSON 文本：反序列化回 envelope 由聚合器负责，
// store 不理解协议结构（分层：store 只管存取，不管 schema 演进）。
type Event struct {
	// ID 事件唯一标识（聚合器发放，字典序 = 发放序）。
	ID string

	// ProjectID 目标 Project；Studio 使用它订阅整个工作区。
	ProjectID string

	// SessionID 目标会话 ID；空串表示 Project 级或全局事件。
	SessionID string

	// ResourceType / ResourceID 标识变化的资源，Studio 可按需精确重读。
	ResourceType string
	ResourceID   string

	// Revision 是资源自身版本，Sequence 是 Project 内连续事件序号。
	Revision int64
	Sequence int64

	// Channel 事件分发通道。
	Channel string

	// Type 事件类型。
	Type string

	// Importance 事件重要级别（聚合器分级打标结果）。
	Importance string

	// AgentID 事件来源 Agent ID；无来源时为 "system"。
	AgentID string

	// AgentRole 事件来源 Agent 角色。
	AgentRole string

	// Parent 关联运行上下文的 JSON 文本（ParentRef 序列化）。
	Parent string

	// Payload 事件负载的 JSON 文本（原样保存，回放时原样下发）。
	Payload string

	// Ts 事件发生时间。
	Ts time.Time
}

// InsertEvent 保留原调用面；实际写入由 InsertProjectEvent 统一补齐 Project
// 和连续 sequence。
func (s *Store) InsertEvent(ev Event) error {
	_, err := s.InsertProjectEvent(ev)
	return err
}

// InsertProjectEvent 先确定 Project，再在同一事务中分配连续序号并写入。
// 返回补齐后的事件，聚合器用它向 Project 订阅者实时下发同一 sequence。
func (s *Store) InsertProjectEvent(ev Event) (Event, error) {
	if ev.ProjectID == "" && ev.SessionID != "" {
		var projectID string
		err := s.db.QueryRow(`SELECT project_id FROM chat_sessions WHERE id = ?`,
			ev.SessionID).Scan(&projectID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Event{}, fmt.Errorf("解析事件 Project 失败: %w", err)
		}
		ev.ProjectID = projectID
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Event{}, fmt.Errorf("开启事件写入事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// trace 频道不参与 Studio 断连续传，因此保持 sequence=0。只有会被
	// ListProjectEventsAfter 返回的事件才能占用 Project 连续序号。
	if ev.ProjectID != "" && ev.Sequence == 0 && ev.Channel != ChannelTrace {
		var next int64
		err := tx.QueryRow(`SELECT next_sequence FROM project_event_sequences
			WHERE project_id = ?`, ev.ProjectID).Scan(&next)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			ev.Sequence = 1
			if _, err := tx.Exec(`INSERT INTO project_event_sequences
				(project_id, next_sequence) VALUES (?, 2)`, ev.ProjectID); err != nil {
				return Event{}, fmt.Errorf("初始化 Project 事件序号失败: %w", err)
			}
		case err != nil:
			return Event{}, fmt.Errorf("读取 Project 事件序号失败: %w", err)
		default:
			ev.Sequence = next
			if _, err := tx.Exec(`UPDATE project_event_sequences
				SET next_sequence = ? WHERE project_id = ?`, next+1, ev.ProjectID); err != nil {
				return Event{}, fmt.Errorf("推进 Project 事件序号失败: %w", err)
			}
		}
	}
	if _, err := tx.Exec(`INSERT INTO events (id, project_id, session_id,
		resource_type, resource_id, revision, sequence, channel, type, importance,
		agent_id, agent_role, parent, payload, ts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.ProjectID, ev.SessionID, ev.ResourceType, ev.ResourceID,
		ev.Revision, ev.Sequence, ev.Channel, ev.Type, ev.Importance,
		ev.AgentID, ev.AgentRole, ev.Parent, ev.Payload, ev.Ts); err != nil {
		return Event{}, fmt.Errorf("写入会话 %q 的事件 %q 失败: %w",
			ev.SessionID, ev.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return Event{}, fmt.Errorf("提交事件写入事务失败: %w", err)
	}
	return ev, nil
}

// ListEventsAfter 查询会话中 id 字典序晚于 lastEventID 的事件，按 id 升序
// 返回（id 字典序 = 事件发放序，断连续传的补发顺序）。
// lastEventID 为空串时从会话起点返回（全量回放）；limit <= 0 时不限条数。
// trace 频道恒不返回：协议语义是"trace 可丢不补"（14-frontend-api §7），
// 在查询侧过滤比在调用侧过滤更能防止未来调用方忘记这条契约。
func (s *Store) ListEventsAfter(sessionID, lastEventID string, limit int) ([]Event, error) {
	query := `SELECT id, session_id, channel, type, importance,
		agent_id, agent_role, parent, payload, ts
		FROM events
		WHERE session_id = ? AND id > ? AND channel != ?
		ORDER BY id`
	args := []any{sessionID, lastEventID, ChannelTrace}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询会话 %q 的后续事件失败: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()

	var events []Event
	for rows.Next() {
		var ev Event
		if err := rows.Scan(&ev.ID, &ev.SessionID, &ev.Channel, &ev.Type, &ev.Importance,
			&ev.AgentID, &ev.AgentRole, &ev.Parent, &ev.Payload, &ev.Ts); err != nil {
			return nil, fmt.Errorf("扫描事件失败: %w", err)
		}
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历事件列表失败: %w", err)
	}
	return events, nil
}

// ListProjectEventsAfter 按 Project 连续序号返回增量事件。trace 事件与旧的
// 会话补发一样不回放；Studio 遇到 ErrEventGap 后重新读取 Snapshot。
func (s *Store) ListProjectEventsAfter(projectID string, afterSequence int64, limit int) ([]Event, error) {
	var maxSequence int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(sequence), 0) FROM events
		WHERE project_id = ?`, projectID).Scan(&maxSequence); err != nil {
		return nil, fmt.Errorf("读取 Project 事件位置失败: %w", err)
	}
	if afterSequence > maxSequence {
		return nil, ErrEventGap
	}
	query := `SELECT id, project_id, session_id, resource_type, resource_id,
		revision, sequence, channel, type, importance, agent_id, agent_role,
		parent, payload, ts FROM events
		WHERE project_id = ? AND sequence > ? AND channel != ?
		ORDER BY sequence`
	args := []any{projectID, afterSequence, ChannelTrace}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("读取 Project 增量事件失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]Event, 0)
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.ProjectID, &event.SessionID,
			&event.ResourceType, &event.ResourceID, &event.Revision,
			&event.Sequence, &event.Channel, &event.Type, &event.Importance,
			&event.AgentID, &event.AgentRole, &event.Parent, &event.Payload,
			&event.Ts); err != nil {
			return nil, fmt.Errorf("扫描 Project 事件失败: %w", err)
		}
		result = append(result, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 Project 事件失败: %w", err)
	}
	return result, nil
}

// LatestProjectEventSequence 返回 Snapshot 对应的最后事件位置。
func (s *Store) LatestProjectEventSequence(projectID string) (int64, error) {
	var sequence int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(sequence), 0) FROM events
		WHERE project_id = ?`, projectID).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("读取 Project 最新事件位置失败: %w", err)
	}
	return sequence, nil
}
