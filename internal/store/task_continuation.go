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

var ErrContinuationInterrupted = errors.New("Interaction continuation 已中断，需要用户显式恢复")

// StartTaskContinuation 把“paused Task 校验、source Interaction 幂等 Run 预留、
// Task→running”放在同一事务。allowInterruptedRetry 只由用户显式 Resume 路径
// 使用：旧 Run 保留关联，新建一次 active attempt，但不会重放旧 Run。
func (s *Store) StartTaskContinuation(taskID string, expectedRevision int64,
	run RunSession, allowInterruptedRetry bool, now time.Time) (RunSession, Task, bool, error) {
	if run.SourceInteractionID == "" || run.TaskID != taskID || run.Status != RunStatusQueued ||
		run.Kind != RunKindTaskExecution {
		return RunSession{}, Task{}, false, ErrInvalidState
	}
	tx, err := s.db.Begin()
	if err != nil {
		return RunSession{}, Task{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	var revision int64
	var workflowID, contextID, agentID string
	err = tx.QueryRow(`SELECT workflow_id,context_id,assigned_agent_id,status,revision FROM tasks WHERE id=?`,
		taskID).Scan(&workflowID, &contextID, &agentID, &status, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return RunSession{}, Task{}, false, ErrNotFound
	}
	if err != nil {
		return RunSession{}, Task{}, false, err
	}
	var workflowStatus, projectID, conversationID string
	if err := tx.QueryRow(`SELECT status,project_id,conversation_id FROM workflows WHERE id=?`,
		workflowID).Scan(&workflowStatus, &projectID, &conversationID); err != nil {
		return RunSession{}, Task{}, false, err
	}
	if workflowStatus != WorkflowStatusRunning {
		return RunSession{}, Task{}, false, ErrInvalidState
	}
	if run.WorkflowID != workflowID || run.ContextID != contextID || run.AgentID != agentID ||
		run.ProjectID != projectID || run.ChatSessionID != conversationID {
		return RunSession{}, Task{}, false, ErrInvalidState
	}
	var interactionProjectID, interactionWorkflowID, interactionTaskID, interactionStatus string
	var interactionSourceRevision int64
	if err := tx.QueryRow(`SELECT project_id,workflow_id,task_id,status,source_revision
		FROM interactions WHERE id=?`, run.SourceInteractionID).Scan(&interactionProjectID,
		&interactionWorkflowID, &interactionTaskID, &interactionStatus,
		&interactionSourceRevision); errors.Is(err, sql.ErrNoRows) {
		return RunSession{}, Task{}, false, ErrNotFound
	} else if err != nil {
		return RunSession{}, Task{}, false, err
	}
	if interactionProjectID != projectID || interactionWorkflowID != workflowID ||
		interactionTaskID != taskID ||
		(interactionStatus != InteractionStatusAnswered && interactionStatus != InteractionStatusCancelled) {
		return RunSession{}, Task{}, false, ErrInvalidState
	}
	// Interaction 绑定的是提问时的 Task revision；回答落库前，证据同步等
	// 无关更新可以合法推进当前 revision。真正的并发边界由下方“当前 Task
	// 仍是 paused + expectedRevision”保证，不能把旧 source revision 当成
	// 当前行锁，否则用户已经回答也会永久留在 waiting_input。
	if !allowInterruptedRetry && (interactionSourceRevision <= 0 ||
		interactionSourceRevision > expectedRevision) {
		return RunSession{}, Task{}, false, ErrRevisionConflict
	}

	existing, existingErr := scanRun(tx.QueryRow(`SELECT `+runSelectColumns+
		` FROM run_sessions WHERE source_interaction_id=?
		 ORDER BY CASE WHEN status IN ('queued','running','waiting_input','cancelling') THEN 0 ELSE 1 END,
		 started_at DESC,id DESC LIMIT 1`, run.SourceInteractionID))
	if existingErr == nil {
		if (existing.Status == RunStatusQueued || existing.Status == RunStatusRunning ||
			existing.Status == RunStatusWaitingInput) && status == TaskStatusRunning {
			_ = tx.Rollback()
			task, getErr := s.GetTask(taskID)
			return existing, task, false, getErr
		}
		if !allowInterruptedRetry {
			return RunSession{}, Task{}, false, ErrContinuationInterrupted
		}
		if !terminalRunStatus(existing.Status) || status != TaskStatusPaused || revision != expectedRevision {
			return RunSession{}, Task{}, false, ErrRevisionConflict
		}
	} else if !errors.Is(existingErr, sql.ErrNoRows) {
		return RunSession{}, Task{}, false, existingErr
	}
	if status != TaskStatusPaused || revision != expectedRevision {
		return RunSession{}, Task{}, false, ErrRevisionConflict
	}
	if run.ID == "" {
		run.ID = NewRunSessionID()
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = now
	}
	run.UpdatedAt = now
	_, err = tx.Exec(`INSERT INTO run_sessions
		(id,project_id,chat_session_id,workflow_id,task_id,source_interaction_id,kind,context_id,
		 agent_id,agent_name,provider,endpoint,model,trace_id,status,error,revision,started_at,updated_at,ended_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?,NULL)`, run.ID, run.ProjectID,
		run.ChatSessionID, run.WorkflowID, run.TaskID, run.SourceInteractionID, run.Kind,
		run.ContextID, run.AgentID, run.AgentName, run.Provider, run.Endpoint, run.Model,
		run.TraceID, RunStatusQueued, "", run.StartedAt, now)
	if err != nil {
		return RunSession{}, Task{}, false, fmt.Errorf("预留 Interaction continuation Run 失败: %w", err)
	}
	result, err := tx.Exec(`UPDATE tasks SET status=?,waiting_reason='',revision=revision+1,updated_at=?
		WHERE id=? AND status=? AND revision=?`, TaskStatusRunning, now, taskID,
		TaskStatusPaused, expectedRevision)
	if err != nil {
		return RunSession{}, Task{}, false, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return RunSession{}, Task{}, false, ErrRevisionConflict
	}
	if err := tx.Commit(); err != nil {
		return RunSession{}, Task{}, false, err
	}
	created, err := s.GetRunSession(run.ID)
	if err != nil {
		return RunSession{}, Task{}, false, err
	}
	task, err := s.GetTask(taskID)
	return created, task, true, err
}

func (s *Store) GetUnhandledAnsweredInteractionForTask(taskID string) (Interaction, error) {
	value, err := scanInteraction(s.db.QueryRow(`SELECT `+interactionSelectColumns+
		` FROM interactions WHERE task_id=? AND status=? AND handled_at IS NULL
		ORDER BY answered_at DESC,id DESC LIMIT 1`, taskID, InteractionStatusAnswered))
	if errors.Is(err, sql.ErrNoRows) {
		return Interaction{}, ErrNotFound
	}
	return value, err
}
