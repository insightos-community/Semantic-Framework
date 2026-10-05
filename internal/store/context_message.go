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

	"github.com/cloudwego/eino/schema"
)

// ContextMessage 是 Task Context 的独立 transcript。它沿用 Eino Message，
// 但不写入用户 Conversation 的 chat_messages。
type ContextMessage struct {
	ID           string          `json:"id"`
	ContextID    string          `json:"context_id"`
	ProjectID    string          `json:"project_id"`
	SessionID    string          `json:"conversation_id"`
	TaskID       string          `json:"task_id"`
	AgentID      string          `json:"agent_id"`
	RunID        string          `json:"run_id,omitempty"`
	TraceID      string          `json:"trace_id,omitempty"`
	Message      *schema.Message `json:"message"`
	ArtifactRefs []string        `json:"artifact_refs"`
	Metadata     string          `json:"metadata"`
	CreatedAt    time.Time       `json:"created_at"`
}

func (s *Store) AppendContextMessage(value ContextMessage) error {
	if value.Message == nil || value.ContextID == "" || value.TaskID == "" {
		return ErrInvalidState
	}
	task, err := s.GetTask(value.TaskID)
	if err != nil {
		return err
	}
	workflow, err := s.GetWorkflow(task.WorkflowID)
	if err != nil {
		return err
	}
	if task.ContextID != value.ContextID || workflow.ProjectID != value.ProjectID || workflow.ConversationID != value.SessionID {
		return ErrInvalidState
	}
	messageJSON, err := json.Marshal(value.Message)
	if err != nil {
		return err
	}
	refsJSON, err := json.Marshal(value.ArtifactRefs)
	if err != nil {
		return err
	}
	if value.Metadata == "" {
		value.Metadata = "{}"
	}
	if value.ID == "" {
		value.ID = NewChatMessageID()
	}
	if value.CreatedAt.IsZero() {
		value.CreatedAt = time.Now().UTC()
	}
	_, err = s.db.Exec(`INSERT INTO context_messages (id,context_id,project_id,session_id,task_id,agent_id,run_id,trace_id,message_json,artifact_refs,metadata,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, value.ID, value.ContextID, value.ProjectID, value.SessionID, value.TaskID, value.AgentID, value.RunID, value.TraceID, string(messageJSON), string(refsJSON), value.Metadata, value.CreatedAt)
	if err != nil {
		return fmt.Errorf("写入 Task Context 消息失败: %w", err)
	}
	return nil
}

func (s *Store) ListContextMessagesAfter(contextID, afterID string, limit int) ([]ContextMessage, error) {
	query := `SELECT id,context_id,project_id,session_id,task_id,agent_id,run_id,trace_id,message_json,artifact_refs,metadata,created_at FROM context_messages WHERE context_id=? AND id>? ORDER BY id`
	args := []any{contextID, afterID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ContextMessage, 0)
	for rows.Next() {
		var v ContextMessage
		var messageJSON, refsJSON string
		if err := rows.Scan(&v.ID, &v.ContextID, &v.ProjectID, &v.SessionID, &v.TaskID, &v.AgentID, &v.RunID, &v.TraceID, &messageJSON, &refsJSON, &v.Metadata, &v.CreatedAt); err != nil {
			return nil, err
		}
		v.Message = &schema.Message{}
		if err := json.Unmarshal([]byte(messageJSON), v.Message); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(refsJSON), &v.ArtifactRefs); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

func (s *Store) ContextMessageExists(contextID, messageID string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM context_messages WHERE context_id=? AND id=?`, contextID, messageID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
