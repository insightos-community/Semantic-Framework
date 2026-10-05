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
	"strings"
)

type robotReservationReader interface {
	QueryRow(string, ...any) *sql.Row
}

// robotReservation reads existing work inside the same transaction as admission.
// The Run binding covers the gaps between Skills, including model/approval waits.
func robotReservation(q robotReservationReader, robotID, taskID, runID string) error {
	var owner string
	err := q.QueryRow(`SELECT 'Task '||id FROM tasks WHERE assigned_robot_id=? AND id<>?
		AND status IN ('pending','running','paused','stopping')
		UNION ALL SELECT 'Run '||id FROM run_sessions WHERE robot_id=? AND id<>?
		AND status IN ('queued','running','waiting_input','cancelling') LIMIT 1`,
		robotID, taskID, robotID, runID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: %s", ErrRobotReserved, owner)
}

func robotExecutionReservation(q robotReservationReader, robotID, taskID string) error {
	var owner string
	err := q.QueryRow(`SELECT id FROM robot_executions WHERE robot_id=?
		AND status IN ('queued','starting','running','waiting_agent','stopping')
		AND (?='' OR COALESCE(json_extract(body,'$.task_id'),'')<>?) LIMIT 1`,
		robotID, taskID, taskID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: Execution %s", ErrRobotReserved, owner)
}

func robotPhysicalReservation(q robotReservationReader, robotID string) error {
	pilot, err := scanRobotPilot(q.QueryRow(`SELECT body FROM robot_pilots
		WHERE robot_id=? ORDER BY last_seen_at DESC LIMIT 1`, robotID))
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if pilot.CurrentExecutionID != "" || pilot.RobotStatus == "busy" ||
		pilot.RobotStatus == "stopping" || pilot.RobotStatus == "interrupted" {
		return fmt.Errorf("%w: Pilot %s / Execution %s", ErrRobotReserved, pilot.PilotInstanceID, pilot.CurrentExecutionID)
	}
	return nil
}

// CheckRobotAdmission is a fast preflight only; AdmitRobotExecution repeats it
// transactionally after input validation to close all check/write races.
func (s *Store) CheckRobotAdmission(robotID, taskID, runID string) error {
	if err := robotReservation(s.db, robotID, taskID, runID); err != nil {
		return err
	}
	return robotExecutionReservation(s.db, robotID, "")
}

// GetRobotConversationRun exposes current ownership even between physical Skills.
func (s *Store) GetRobotConversationRun(robotID string) (RunSession, error) {
	run, err := scanRun(s.db.QueryRow(`SELECT `+runSelectColumns+` FROM run_sessions
		WHERE robot_id=? AND status IN ('queued','running','waiting_input','cancelling') LIMIT 1`, robotID))
	if errors.Is(err, sql.ErrNoRows) {
		return RunSession{}, ErrNotFound
	}
	return run, err
}

func (s *Store) GetRobotTask(robotID string) (Task, error) {
	task, err := scanTask(s.db.QueryRow(`SELECT `+taskColumns+` FROM tasks
		WHERE assigned_robot_id=? AND status IN ('pending','running','paused','stopping') LIMIT 1`, robotID))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	return task, err
}

// AdmitRobotExecution atomically reserves work and persists its first execution
// fact. No Pilot command may be sent before this transaction succeeds.
func (s *Store) AdmitRobotExecution(item RobotExecution, directAgentID string) (RobotExecution, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return RobotExecution{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	existing, err := scanRobotExecution(tx.QueryRow(`SELECT body FROM robot_executions
		WHERE project_id=? AND request_key=?`, item.ProjectID, item.RequestKey))
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return RobotExecution{}, false, err
	}
	direct := item.TaskID == "" && item.RunID != ""
	excludedRun := ""
	if direct {
		run, err := scanRun(tx.QueryRow(`SELECT `+runSelectColumns+` FROM run_sessions WHERE id=?`, item.RunID))
		if err != nil || item.WorkflowID != "" || item.SubtaskID != "" || run.ProjectID != item.ProjectID || run.Kind != RunKindConversation ||
			run.TaskID != "" || run.WorkflowID != "" || run.Status != RunStatusRunning ||
			run.AgentID != directAgentID || directAgentID != "robot:"+item.RobotID ||
			(run.RobotID != "" && run.RobotID != item.RobotID) {
			return RobotExecution{}, false, ErrInvalidState
		}
		excludedRun = run.ID
	}
	if item.TaskID != "" {
		if err := validateRobotTaskAdmission(tx, item); err != nil {
			return RobotExecution{}, false, err
		}
	}
	if err := robotReservation(tx, item.RobotID, item.TaskID, excludedRun); err != nil {
		return RobotExecution{}, false, err
	}
	if err := robotExecutionReservation(tx, item.RobotID, ""); err != nil {
		return RobotExecution{}, false, err
	}
	pilot, err := scanRobotPilot(tx.QueryRow(`SELECT body FROM robot_pilots WHERE pilot_instance_id=?`, item.PilotInstanceID))
	if err != nil {
		return RobotExecution{}, false, err
	}
	if pilot.CurrentExecutionID != "" || pilot.RobotStatus == "busy" ||
		pilot.RobotStatus == "stopping" || pilot.RobotStatus == "interrupted" {
		return RobotExecution{}, false, fmt.Errorf("%w: Pilot %s / Execution %s", ErrRobotReserved, pilot.PilotInstanceID, pilot.CurrentExecutionID)
	}
	var runtimeProject string
	err = tx.QueryRow(`SELECT COALESCE(json_extract(body,'$.project_id'),'') FROM robot_runtime_instances
		WHERE robot_id=? ORDER BY updated_at DESC LIMIT 1`, item.RobotID).Scan(&runtimeProject)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return RobotExecution{}, false, err
	}
	if runtimeProject != "" && runtimeProject != item.ProjectID {
		return RobotExecution{}, false, ErrInvalidState
	}
	if direct {
		if _, err := tx.Exec(`UPDATE run_sessions SET robot_id=?,revision=revision+1,updated_at=?
			WHERE id=? AND robot_id=''`, item.RobotID, item.CreatedAt, item.RunID); err != nil {
			return RobotExecution{}, false, err
		}
	}
	body, err := json.Marshal(item)
	if err != nil {
		return RobotExecution{}, false, err
	}
	if _, err := tx.Exec(`INSERT INTO robot_executions
		(id,project_id,robot_id,request_key,status,revision,updated_at,body,subtask_id)
		VALUES(?,?,?,?,?,?,?,?,?)`, item.ID, item.ProjectID, item.RobotID, item.RequestKey,
		item.Status, item.Revision, item.UpdatedAt, body, item.SubtaskID); err != nil {
		return RobotExecution{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return RobotExecution{}, false, err
	}
	return item, true, nil
}

// Approval and binding are rechecked in the insertion transaction, because a
// Task can be stopped while Pilot is validating its Skill input.
func validateRobotTaskAdmission(tx *sql.Tx, item RobotExecution) error {
	w, err := scanWorkflow(tx.QueryRow(`SELECT `+workflowColumns+` FROM workflows WHERE id=?`, item.WorkflowID))
	if err != nil || w.ProjectID != item.ProjectID || w.Status != WorkflowStatusRunning || w.ConfirmedRevision != w.Revision {
		return ErrInvalidState
	}
	task, err := scanTask(tx.QueryRow(`SELECT `+taskColumns+` FROM tasks WHERE id=?`, item.TaskID))
	if err != nil || task.WorkflowID != w.ID || task.AssignedRobotID != item.RobotID || task.RequiredRole != "robot" || task.Status != TaskStatusRunning {
		return ErrInvalidState
	}
	subtask, err := scanSubTask(tx.QueryRow(`SELECT `+subTaskColumns+` FROM subtasks WHERE id=?`, item.SubtaskID))
	if err != nil || subtask.TaskID != task.ID || subtask.Status != TaskStatusRunning || subtask.Kind != "robot_skill" {
		return ErrInvalidState
	}
	var spec struct {
		SkillName    string `json:"skill_name"`
		SkillVersion string `json:"skill_version"`
	}
	if json.Unmarshal(subtask.Spec, &spec) != nil || strings.TrimSpace(spec.SkillName) != strings.TrimSpace(item.SkillName) ||
		(strings.TrimSpace(spec.SkillVersion) != "" && strings.TrimSpace(spec.SkillVersion) != strings.TrimSpace(item.SkillVersion)) {
		return ErrInvalidState
	}
	var scope struct {
		RobotIDs      []string `json:"robot_ids"`
		AllowedSkills []string `json:"allowed_skills"`
	}
	if len(w.ApprovedScope) > 0 && json.Unmarshal(w.ApprovedScope, &scope) != nil {
		return ErrInvalidState
	}
	allowed := func(values []string, value string) bool {
		if len(values) == 0 {
			return true
		}
		for _, candidate := range values {
			if strings.TrimSpace(candidate) == strings.TrimSpace(value) {
				return true
			}
		}
		return false
	}
	if !allowed(scope.RobotIDs, item.RobotID) || !allowed(scope.AllowedSkills, item.SkillName) {
		return ErrInvalidState
	}
	return nil
}

func (s *Store) ListRobotExecutionsByRun(runID string) ([]RobotExecution, error) {
	rows, err := s.db.Query(`SELECT body FROM robot_executions
		WHERE json_extract(body,'$.run_id')=? ORDER BY updated_at DESC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []RobotExecution
	for rows.Next() {
		item, err := scanRobotExecution(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range result {
		result[index], err = s.withRobotExecutionArtifacts(result[index])
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}
