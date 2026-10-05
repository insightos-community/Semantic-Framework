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
	"time"

	"github.com/google/uuid"
)

const (
	WorkflowStatusPending   = "pending"
	WorkflowStatusRunning   = "running"
	WorkflowStatusPaused    = "paused"
	WorkflowStatusStopping  = "stopping"
	WorkflowStatusCompleted = "completed"
	WorkflowStatusFailed    = "failed"
	WorkflowStatusStopped   = "stopped"

	TaskStatusPending   = "pending"
	TaskStatusRunning   = "running"
	TaskStatusPaused    = "paused"
	TaskStatusStopping  = "stopping"
	TaskStatusCompleted = "completed"
	TaskStatusFailed    = "failed"
	TaskStatusStopped   = "stopped"
)

var ErrWorkflowExists = errors.New("Project 已有未结束 Workflow")

type Workflow struct {
	ID                  string          `json:"id"`
	ProjectID           string          `json:"project_id"`
	ConversationID      string          `json:"conversation_id"`
	Goal                string          `json:"goal"`
	ApprovedScope       json.RawMessage `json:"approved_scope"`
	Constraints         json.RawMessage `json:"constraints"`
	CompletionCriteria  json.RawMessage `json:"completion_criteria"`
	MapScope            json.RawMessage `json:"map_scope"`
	Status              string          `json:"status"`
	Reason              string          `json:"reason,omitempty"`
	Revision            int64           `json:"revision"`
	ConfirmedRevision   int64           `json:"confirmed_revision"`
	SourceInteractionID string          `json:"-"`
	CreatedAt           time.Time       `json:"created_at"`
	StartedAt           *time.Time      `json:"started_at,omitempty"`
	UpdatedAt           time.Time       `json:"updated_at"`
	EndedAt             *time.Time      `json:"ended_at,omitempty"`
}

type Task struct {
	ID                   string          `json:"id"`
	WorkflowID           string          `json:"workflow_id"`
	Position             int             `json:"position"`
	RequiredRole         string          `json:"required_role"`
	RequiredCapabilities json.RawMessage `json:"required_capabilities"`
	ResourceRequirements json.RawMessage `json:"resource_requirements"`
	AssignedAgentID      string          `json:"assigned_agent_id,omitempty"`
	AssignedRobotID      string          `json:"assigned_robot_id,omitempty"`
	AssignmentRevision   int64           `json:"assignment_revision"`
	Goal                 string          `json:"goal"`
	Input                json.RawMessage `json:"input"`
	CompletionCriteria   json.RawMessage `json:"completion_criteria"`
	Status               string          `json:"status"`
	WaitingReason        string          `json:"reason,omitempty"`
	ResultSummary        string          `json:"result_summary,omitempty"`
	Evidence             json.RawMessage `json:"evidence"`
	ContextID            string          `json:"context_id"`
	Revision             int64           `json:"revision"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
	EndedAt              *time.Time      `json:"ended_at,omitempty"`
}

type SubTask struct {
	ID                 string          `json:"id"`
	TaskID             string          `json:"task_id"`
	Position           int             `json:"position"`
	Kind               string          `json:"kind"`
	Goal               string          `json:"goal"`
	Spec               json.RawMessage `json:"spec"`
	CompletionCriteria json.RawMessage `json:"completion_criteria"`
	Evidence           json.RawMessage `json:"evidence"`
	ExecutionRef       string          `json:"execution_ref,omitempty"`
	Status             string          `json:"status"`
	WaitingReason      string          `json:"reason,omitempty"`
	Result             json.RawMessage `json:"result"`
	Revision           int64           `json:"revision"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	EndedAt            *time.Time      `json:"ended_at,omitempty"`
}

type TaskDependency struct {
	TaskID          string `json:"task_id"`
	DependsOnTaskID string `json:"depends_on_task_id"`
}

type SubTaskDependency struct {
	TaskID             string `json:"task_id"`
	SubTaskID          string `json:"subtask_id"`
	DependsOnSubTaskID string `json:"depends_on_subtask_id"`
}

type WorkflowView struct {
	Workflow            Workflow            `json:"workflow"`
	Tasks               []Task              `json:"tasks"`
	SubTasks            []SubTask           `json:"subtasks"`
	Dependencies        []TaskDependency    `json:"dependencies"`
	SubTaskDependencies []SubTaskDependency `json:"subtask_dependencies"`
}

type TaskDraft struct {
	ID                   string          `json:"id,omitempty"`
	RequiredRole         string          `json:"required_role,omitempty"`
	RequiredCapabilities json.RawMessage `json:"required_capabilities,omitempty"`
	ResourceRequirements json.RawMessage `json:"resource_requirements,omitempty"`
	Goal                 string          `json:"goal"`
	Input                json.RawMessage `json:"input,omitempty"`
	CompletionCriteria   json.RawMessage `json:"completion_criteria,omitempty"`
	SubTasks             []SubTaskDraft  `json:"subtasks,omitempty"`
}

type SubTaskDraft struct {
	ID                 string          `json:"id,omitempty"`
	Kind               string          `json:"kind,omitempty"`
	Goal               string          `json:"goal"`
	Spec               json.RawMessage `json:"spec,omitempty"`
	CompletionCriteria json.RawMessage `json:"completion_criteria,omitempty"`
	DependsOn          []string        `json:"depends_on,omitempty"`
}

type WorkflowDraft struct {
	Goal               string           `json:"goal"`
	Constraints        json.RawMessage  `json:"constraints,omitempty"`
	CompletionCriteria json.RawMessage  `json:"completion_criteria,omitempty"`
	MapScope           json.RawMessage  `json:"map_scope,omitempty"`
	Tasks              []TaskDraft      `json:"tasks,omitempty"`
	Dependencies       []TaskDependency `json:"dependencies,omitempty"`
}

func NewWorkflowID() string { return "wf-" + uuid.NewString() }
func NewTaskID() string     { return "task-" + uuid.NewString() }
func NewSubTaskID() string  { return "subtask-" + uuid.NewString() }

func ValidSubTaskKind(kind string) bool {
	return kind == "robot_skill" || kind == "agent_step"
}

func ValidWorkflowStatus(status string) bool {
	switch status {
	case WorkflowStatusPending, WorkflowStatusRunning, WorkflowStatusPaused,
		WorkflowStatusStopping, WorkflowStatusCompleted, WorkflowStatusFailed,
		WorkflowStatusStopped:
		return true
	default:
		return false
	}
}

func ValidTaskStatus(status string) bool { return ValidWorkflowStatus(status) }
func terminalWorkStatus(status string) bool {
	return status == WorkflowStatusCompleted || status == WorkflowStatusFailed || status == WorkflowStatusStopped
}

func jsonOr(raw json.RawMessage, fallback string) string {
	if len(raw) == 0 {
		return fallback
	}
	return string(raw)
}

func validateDraft(draft WorkflowDraft) error {
	if strings.TrimSpace(draft.Goal) == "" {
		return fmt.Errorf("goal 不能为空: %w", ErrInvalidState)
	}
	ids := make(map[string]struct{}, len(draft.Tasks))
	for i := range draft.Tasks {
		task := &draft.Tasks[i]
		if strings.TrimSpace(task.Goal) == "" {
			return fmt.Errorf("Task goal 不能为空: %w", ErrInvalidState)
		}
		// 计划只声明职责和资源约束，实际 Agent/Robot 必须等 Task ready 后绑定。
		task.RequiredRole = strings.TrimSpace(task.RequiredRole)
		if task.RequiredRole == "" {
			return fmt.Errorf("Task required_role 不能为空: %w", ErrInvalidState)
		}
		if task.ID == "" {
			task.ID = NewTaskID()
		}
		if _, exists := ids[task.ID]; exists {
			return fmt.Errorf("Task ID 重复: %w", ErrInvalidState)
		}
		ids[task.ID] = struct{}{}
		subIDs := make(map[string]struct{}, len(task.SubTasks))
		for j := range task.SubTasks {
			sub := &task.SubTasks[j]
			if strings.TrimSpace(sub.Goal) == "" {
				return fmt.Errorf("SubTask goal 不能为空: %w", ErrInvalidState)
			}
			if !ValidSubTaskKind(sub.Kind) {
				return fmt.Errorf("SubTask kind 只能是 robot_skill 或 agent_step: %w", ErrInvalidState)
			}
			if sub.ID == "" {
				sub.ID = NewSubTaskID()
			}
			if _, exists := subIDs[sub.ID]; exists {
				return fmt.Errorf("SubTask ID 重复: %w", ErrInvalidState)
			}
			subIDs[sub.ID] = struct{}{}
		}
		for _, sub := range task.SubTasks {
			for _, dependency := range sub.DependsOn {
				if dependency == sub.ID {
					return fmt.Errorf("SubTask 不能依赖自身: %w", ErrInvalidState)
				}
				if _, ok := subIDs[dependency]; !ok {
					return fmt.Errorf("SubTask 前置依赖不存在: %w", ErrInvalidState)
				}
			}
		}
	}
	adj := make(map[string][]string, len(ids))
	for _, dep := range draft.Dependencies {
		if dep.TaskID == dep.DependsOnTaskID {
			return fmt.Errorf("Task 不能依赖自身: %w", ErrInvalidState)
		}
		if _, ok := ids[dep.TaskID]; !ok {
			return fmt.Errorf("依赖目标 Task 不存在: %w", ErrInvalidState)
		}
		if _, ok := ids[dep.DependsOnTaskID]; !ok {
			return fmt.Errorf("前置 Task 不存在: %w", ErrInvalidState)
		}
		adj[dep.DependsOnTaskID] = append(adj[dep.DependsOnTaskID], dep.TaskID)
	}
	indegree := make(map[string]int, len(ids))
	for id := range ids {
		indegree[id] = 0
	}
	for _, next := range adj {
		for _, id := range next {
			indegree[id]++
		}
	}
	queue := make([]string, 0, len(ids))
	for id, degree := range indegree {
		if degree == 0 {
			queue = append(queue, id)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		delete(indegree, id)
		for _, next := range adj[id] {
			indegree[next]--
			if indegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if len(indegree) != 0 {
		return fmt.Errorf("Task 依赖存在环: %w", ErrInvalidState)
	}
	return nil
}

const workflowColumns = `id, project_id, conversation_id, goal, approved_scope_json, constraints_json,
	criteria_json, map_scope_json, status, reason, revision, confirmed_revision, source_interaction_id,
	created_at, started_at, updated_at, ended_at`

type workflowScanner interface{ Scan(...any) error }

func scanWorkflow(row workflowScanner) (Workflow, error) {
	var value Workflow
	var approvedScope, constraints, criteria, scope string
	err := row.Scan(&value.ID, &value.ProjectID, &value.ConversationID, &value.Goal,
		&approvedScope, &constraints, &criteria, &scope, &value.Status, &value.Reason, &value.Revision,
		&value.ConfirmedRevision, &value.SourceInteractionID, &value.CreatedAt, &value.StartedAt, &value.UpdatedAt, &value.EndedAt)
	value.ApprovedScope = json.RawMessage(approvedScope)
	value.Constraints, value.CompletionCriteria, value.MapScope = json.RawMessage(constraints), json.RawMessage(criteria), json.RawMessage(scope)
	return value, err
}

func replacePlanTx(tx *sql.Tx, workflow Workflow, draft WorkflowDraft, now time.Time) error {
	if _, err := tx.Exec(`DELETE FROM task_dependencies WHERE workflow_id = ?`, workflow.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM subtask_dependencies
		WHERE task_id IN (SELECT id FROM tasks WHERE workflow_id = ?)`, workflow.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM subtasks WHERE task_id IN (SELECT id FROM tasks WHERE workflow_id = ?)`, workflow.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM tasks WHERE workflow_id = ?`, workflow.ID); err != nil {
		return err
	}
	// Proposal 中的 Task/SubTask ID 只是在计划内部表达依赖的局部标识。
	// 它们由模型生成，不能承担数据库全局主键职责；同一计划在另一个
	// Project 再次执行时很容易产生相同 ID。批准事务在这里一次性生成正式
	// ID 并同步改写依赖，既保留 DAG 语义，也不把 UUID 生成责任推给模型。
	taskIDs := make(map[string]string, len(draft.Tasks))
	for _, item := range draft.Tasks {
		taskID := item.ID
		var conflicts int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM tasks WHERE id=? OR context_id=?`,
			taskID, "task:"+taskID).Scan(&conflicts); err != nil {
			return err
		}
		if conflicts != 0 {
			taskID = NewTaskID()
		}
		taskIDs[item.ID] = taskID
	}
	for position, item := range draft.Tasks {
		taskID := taskIDs[item.ID]
		_, err := tx.Exec(`INSERT INTO tasks (id, workflow_id, position, required_role,
			required_capabilities_json, resource_requirements_json, goal, input_json,
			criteria_json, status, context_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, taskID, workflow.ID, position,
			item.RequiredRole, jsonOr(item.RequiredCapabilities, "[]"),
			jsonOr(item.ResourceRequirements, "{}"), strings.TrimSpace(item.Goal),
			jsonOr(item.Input, "{}"), jsonOr(item.CompletionCriteria, "[]"),
			TaskStatusPending, "task:"+taskID, now, now)
		if err != nil {
			return fmt.Errorf("创建 Task 失败: %w", err)
		}
		subTaskIDs := make(map[string]string, len(item.SubTasks))
		for _, sub := range item.SubTasks {
			subTaskID := sub.ID
			var conflicts int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM subtasks WHERE id=?`,
				subTaskID).Scan(&conflicts); err != nil {
				return err
			}
			if conflicts != 0 {
				subTaskID = NewSubTaskID()
			}
			subTaskIDs[sub.ID] = subTaskID
		}
		for subPosition, sub := range item.SubTasks {
			subTaskID := subTaskIDs[sub.ID]
			_, err = tx.Exec(`INSERT INTO subtasks (id, task_id, position, kind, goal,
				spec_json, criteria_json, status, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, subTaskID, taskID, subPosition,
				sub.Kind, strings.TrimSpace(sub.Goal), jsonOr(sub.Spec, "{}"),
				jsonOr(sub.CompletionCriteria, "[]"), TaskStatusPending, now, now)
			if err != nil {
				return fmt.Errorf("创建 SubTask 失败: %w", err)
			}
			for _, dependency := range sub.DependsOn {
				if _, err = tx.Exec(`INSERT INTO subtask_dependencies
					(task_id, subtask_id, depends_on_subtask_id) VALUES (?, ?, ?)`,
					taskID, subTaskID, subTaskIDs[dependency]); err != nil {
					return fmt.Errorf("创建 SubTask 依赖失败: %w", err)
				}
			}
		}
	}
	for _, dep := range draft.Dependencies {
		if _, err := tx.Exec(`INSERT INTO task_dependencies (workflow_id, task_id, depends_on_task_id)
			VALUES (?, ?, ?)`, workflow.ID, taskIDs[dep.TaskID], taskIDs[dep.DependsOnTaskID]); err != nil {
			return err
		}
	}
	return nil
}

func saveWorkflowRevisionTx(tx *sql.Tx, workflowID string, revision int64, now time.Time) error {
	view, err := getWorkflowViewQuery(tx, workflowID)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO workflow_revisions (workflow_id, revision, snapshot_json, created_at)
		VALUES (?, ?, ?, ?)`, workflowID, revision, string(encoded), now)
	return err
}

func (s *Store) GetWorkflow(id string) (Workflow, error) {
	value, err := scanWorkflow(s.db.QueryRow(`SELECT `+workflowColumns+` FROM workflows WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Workflow{}, ErrNotFound
	}
	if err != nil {
		return Workflow{}, fmt.Errorf("读取 Workflow 失败: %w", err)
	}
	return value, nil
}

func (s *Store) ListWorkflows(projectID string, includeEnded bool) ([]Workflow, error) {
	query := `SELECT ` + workflowColumns + ` FROM workflows WHERE project_id=?`
	if !includeEnded {
		query += ` AND status IN ('pending','running','paused','stopping')`
	}
	query += ` ORDER BY updated_at DESC, id DESC`
	rows, err := s.db.Query(query, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Workflow, 0)
	for rows.Next() {
		value, err := scanWorkflow(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

// ListUnfinishedWorkflows 返回 Server 启动恢复时需要收敛的全部 Workflow。
// 该方法只供应用服务使用；公共接口仍必须按 Project 过滤。
func (s *Store) ListUnfinishedWorkflows() ([]Workflow, error) {
	rows, err := s.db.Query(`SELECT ` + workflowColumns + ` FROM workflows
		WHERE status IN ('running','paused','stopping')
		ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Workflow, 0)
	for rows.Next() {
		value, scanErr := scanWorkflow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) GetWorkflowView(id string) (WorkflowView, error) {
	return getWorkflowViewQuery(s.db, id)
}

type workflowQueryer interface {
	QueryRow(string, ...any) *sql.Row
	Query(string, ...any) (*sql.Rows, error)
}

func getWorkflowViewQuery(q workflowQueryer, id string) (WorkflowView, error) {
	workflow, err := scanWorkflow(q.QueryRow(`SELECT `+workflowColumns+` FROM workflows WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowView{}, ErrNotFound
	}
	if err != nil {
		return WorkflowView{}, err
	}
	tasks, err := listTasksQuery(q, id)
	if err != nil {
		return WorkflowView{}, err
	}
	subs := make([]SubTask, 0)
	for _, task := range tasks {
		values, e := listSubTasksQuery(q, task.ID)
		if e != nil {
			return WorkflowView{}, e
		}
		subs = append(subs, values...)
	}
	rows, err := q.Query(`SELECT task_id, depends_on_task_id FROM task_dependencies WHERE workflow_id=? ORDER BY task_id,depends_on_task_id`, id)
	if err != nil {
		return WorkflowView{}, err
	}
	deps := make([]TaskDependency, 0)
	for rows.Next() {
		var d TaskDependency
		if err := rows.Scan(&d.TaskID, &d.DependsOnTaskID); err != nil {
			_ = rows.Close()
			return WorkflowView{}, err
		}
		deps = append(deps, d)
	}
	if err := rows.Close(); err != nil {
		return WorkflowView{}, err
	}
	subRows, err := q.Query(`SELECT task_id,subtask_id,depends_on_subtask_id
		FROM subtask_dependencies WHERE task_id IN
		(SELECT id FROM tasks WHERE workflow_id=?)
		ORDER BY task_id,subtask_id,depends_on_subtask_id`, id)
	if err != nil {
		return WorkflowView{}, err
	}
	defer subRows.Close()
	subDeps := make([]SubTaskDependency, 0)
	for subRows.Next() {
		var dependency SubTaskDependency
		if err := subRows.Scan(&dependency.TaskID, &dependency.SubTaskID,
			&dependency.DependsOnSubTaskID); err != nil {
			return WorkflowView{}, err
		}
		subDeps = append(subDeps, dependency)
	}
	return WorkflowView{Workflow: workflow, Tasks: tasks, SubTasks: subs,
		Dependencies: deps, SubTaskDependencies: subDeps}, subRows.Err()
}

const taskColumns = `id, workflow_id, position, required_role,
	required_capabilities_json, resource_requirements_json, assigned_agent_id,
	assigned_robot_id, assignment_revision, goal, input_json, criteria_json,
	status, waiting_reason, result_summary, evidence_json, context_id, revision,
	created_at, updated_at, ended_at`

func scanTask(row workflowScanner) (Task, error) {
	var v Task
	var input, criteria, evidence, capabilities, resources string
	err := row.Scan(&v.ID, &v.WorkflowID, &v.Position, &v.RequiredRole, &capabilities, &resources, &v.AssignedAgentID,
		&v.AssignedRobotID, &v.AssignmentRevision, &v.Goal, &input, &criteria,
		&v.Status, &v.WaitingReason, &v.ResultSummary, &evidence, &v.ContextID,
		&v.Revision, &v.CreatedAt, &v.UpdatedAt, &v.EndedAt)
	v.Input = json.RawMessage(input)
	v.CompletionCriteria = json.RawMessage(criteria)
	v.Evidence = json.RawMessage(evidence)
	v.RequiredCapabilities = json.RawMessage(capabilities)
	v.ResourceRequirements = json.RawMessage(resources)
	return v, err
}
func listTasksQuery(q workflowQueryer, workflowID string) ([]Task, error) {
	rows, err := q.Query(`SELECT `+taskColumns+` FROM tasks WHERE workflow_id=? ORDER BY position,id`, workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Task, 0)
	for rows.Next() {
		v, e := scanTask(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
func (s *Store) GetTask(id string) (Task, error) {
	v, err := scanTask(s.db.QueryRow(`SELECT `+taskColumns+` FROM tasks WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	return v, err
}

// ListTasks 返回 Workflow 的稳定 Task 顺序。
func (s *Store) ListTasks(workflowID string) ([]Task, error) {
	return listTasksQuery(s.db, workflowID)
}

const subTaskColumns = `id, task_id, position, kind, goal, spec_json, criteria_json,
	evidence_json, execution_ref, status, waiting_reason, result_json,
	revision, created_at, updated_at, ended_at`

func scanSubTask(row workflowScanner) (SubTask, error) {
	var v SubTask
	var result, spec, criteria, evidence string
	err := row.Scan(&v.ID, &v.TaskID, &v.Position, &v.Kind, &v.Goal, &spec,
		&criteria, &evidence, &v.ExecutionRef, &v.Status, &v.WaitingReason,
		&result, &v.Revision, &v.CreatedAt, &v.UpdatedAt, &v.EndedAt)
	v.Result = json.RawMessage(result)
	v.Spec = json.RawMessage(spec)
	v.CompletionCriteria = json.RawMessage(criteria)
	v.Evidence = json.RawMessage(evidence)
	return v, err
}
func listSubTasksQuery(q workflowQueryer, taskID string) ([]SubTask, error) {
	rows, err := q.Query(`SELECT `+subTaskColumns+` FROM subtasks WHERE task_id=? ORDER BY position,id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]SubTask, 0)
	for rows.Next() {
		v, e := scanSubTask(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

var allowedWorkflowTransitions = map[string]map[string]bool{
	WorkflowStatusRunning:  {WorkflowStatusPaused: true, WorkflowStatusStopping: true, WorkflowStatusCompleted: true, WorkflowStatusFailed: true},
	WorkflowStatusPaused:   {WorkflowStatusRunning: true, WorkflowStatusStopping: true},
	WorkflowStatusStopping: {WorkflowStatusStopped: true, WorkflowStatusPaused: true},
}

var allowedTaskTransitions = map[string]map[string]bool{
	TaskStatusPending: {TaskStatusRunning: true, TaskStatusPaused: true, TaskStatusStopping: true, TaskStatusFailed: true, TaskStatusStopped: true},
	TaskStatusRunning: {TaskStatusPaused: true, TaskStatusStopping: true, TaskStatusCompleted: true, TaskStatusFailed: true},
	TaskStatusPaused:  {TaskStatusRunning: true, TaskStatusStopping: true, TaskStatusFailed: true},
	// stopping → paused 只用于物理停止无法确认的 Robot SubTask。它保留
	// execution_ref 和 Robot 锁，等待对账或人工处理，不能自动重放。
	TaskStatusStopping: {TaskStatusStopped: true, TaskStatusPaused: true},
}

func (s *Store) TransitionWorkflow(id string, expectedRevision int64, to string, now time.Time) (Workflow, error) {
	return s.TransitionWorkflowWithReason(id, expectedRevision, to, "", now)
}

func (s *Store) TransitionWorkflowWithReason(id string, expectedRevision int64, to, reason string, now time.Time) (Workflow, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Workflow{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanWorkflow(tx.QueryRow(`SELECT `+workflowColumns+` FROM workflows WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Workflow{}, ErrNotFound
	}
	if err != nil {
		return Workflow{}, err
	}
	if current.Revision != expectedRevision {
		return Workflow{}, ErrRevisionConflict
	}
	if !allowedWorkflowTransitions[current.Status][to] {
		return Workflow{}, ErrInvalidState
	}
	if to == WorkflowStatusRunning && current.ConfirmedRevision != current.Revision {
		return Workflow{}, ErrRevisionConflict
	}
	nextConfirmedRevision := current.ConfirmedRevision
	if current.ConfirmedRevision == current.Revision {
		nextConfirmedRevision = expectedRevision + 1
	}
	var ended any
	if terminalWorkStatus(to) {
		ended = now
	}
	res, err := tx.Exec(`UPDATE workflows SET status=?,reason=?,revision=revision+1,confirmed_revision=?,updated_at=?,ended_at=?
		WHERE id=? AND revision=?`, to, reason, nextConfirmedRevision, now, ended, id, expectedRevision)
	if err != nil {
		return Workflow{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Workflow{}, ErrRevisionConflict
	}
	if terminalWorkStatus(to) {
		if _, err := tx.Exec(`UPDATE projects SET mode=?,revision=revision+1,updated_at=? WHERE id=?`,
			ProjectModeDevelopment, now, current.ProjectID); err != nil {
			return Workflow{}, err
		}
	}
	if err := saveWorkflowRevisionTx(tx, id, expectedRevision+1, now); err != nil {
		return Workflow{}, err
	}
	if err := tx.Commit(); err != nil {
		return Workflow{}, err
	}
	return s.GetWorkflow(id)
}

func (s *Store) TransitionTask(id string, expectedRevision int64, to, reason, summary string, evidence json.RawMessage, now time.Time) (Task, error) {
	current, err := s.GetTask(id)
	if err != nil {
		return Task{}, err
	}
	if current.Revision != expectedRevision {
		return Task{}, ErrRevisionConflict
	}
	if !allowedTaskTransitions[current.Status][to] {
		return Task{}, ErrInvalidState
	}
	var ended any
	if terminalWorkStatus(to) {
		ended = now
	}
	res, err := s.db.Exec(`UPDATE tasks SET status=?,waiting_reason=?,result_summary=?,evidence_json=?,revision=revision+1,updated_at=?,ended_at=? WHERE id=? AND revision=?`, to, reason, summary, jsonOr(evidence, "[]"), now, ended, id, expectedRevision)
	if err != nil {
		return Task{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Task{}, ErrRevisionConflict
	}
	return s.GetTask(id)
}

// UpdatePausedRobotWaitReason 只改变同一个失败Robot步骤的“当前等待责任方”。
// paused不是可由Scheduler通用恢复的状态，Recovery开始、向用户提问和回答后
// 继续分析都需要在不伪造running的前提下更新reason；Task与SubTask必须在同一
// 事务中前进，避免前端看到一个等待用户、另一个仍显示Recovery运行。
func (s *Store) UpdatePausedRobotWaitReason(taskID string, expectedTaskRevision int64,
	subTaskID string, expectedSubTaskRevision int64, fromReason, toReason string,
	now time.Time) (Task, SubTask, error) {
	fromReason = strings.TrimSpace(fromReason)
	toReason = strings.TrimSpace(toReason)
	if taskID == "" || subTaskID == "" || fromReason == "" || toReason == "" {
		return Task{}, SubTask{}, ErrInvalidState
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Task{}, SubTask{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var taskStatus, taskReason string
	if err := tx.QueryRow(`SELECT status,waiting_reason FROM tasks WHERE id=? AND revision=?`,
		taskID, expectedTaskRevision).Scan(&taskStatus, &taskReason); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Task{}, SubTask{}, ErrRevisionConflict
		}
		return Task{}, SubTask{}, err
	}
	var subTaskStatus, subTaskReason, executionRef string
	if err := tx.QueryRow(`SELECT status,waiting_reason,execution_ref FROM subtasks
		WHERE id=? AND task_id=? AND revision=?`, subTaskID, taskID,
		expectedSubTaskRevision).Scan(&subTaskStatus, &subTaskReason, &executionRef); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Task{}, SubTask{}, ErrRevisionConflict
		}
		return Task{}, SubTask{}, err
	}
	if taskStatus != TaskStatusPaused || subTaskStatus != TaskStatusPaused ||
		taskReason != fromReason || subTaskReason != fromReason || executionRef == "" {
		return Task{}, SubTask{}, ErrInvalidState
	}
	taskResult, err := tx.Exec(`UPDATE tasks SET waiting_reason=?,revision=revision+1,updated_at=?
		WHERE id=? AND revision=? AND status=? AND waiting_reason=?`, toReason, now,
		taskID, expectedTaskRevision, TaskStatusPaused, fromReason)
	if err != nil {
		return Task{}, SubTask{}, err
	}
	if affected, _ := taskResult.RowsAffected(); affected != 1 {
		return Task{}, SubTask{}, ErrRevisionConflict
	}
	subTaskResult, err := tx.Exec(`UPDATE subtasks SET waiting_reason=?,revision=revision+1,updated_at=?
		WHERE id=? AND revision=? AND status=? AND waiting_reason=?`, toReason, now,
		subTaskID, expectedSubTaskRevision, TaskStatusPaused, fromReason)
	if err != nil {
		return Task{}, SubTask{}, err
	}
	if affected, _ := subTaskResult.RowsAffected(); affected != 1 {
		return Task{}, SubTask{}, ErrRevisionConflict
	}
	if err := tx.Commit(); err != nil {
		return Task{}, SubTask{}, err
	}
	task, err := s.GetTask(taskID)
	if err != nil {
		return Task{}, SubTask{}, err
	}
	subTask, err := s.GetSubTask(subTaskID)
	return task, subTask, err
}

// SetPendingTaskReason 记录 pending Task 暂不能启动的原因，不伪造新的主状态。
func (s *Store) SetPendingTaskReason(id string, expectedRevision int64, reason string, now time.Time) (Task, error) {
	result, err := s.db.Exec(`UPDATE tasks SET waiting_reason=?,revision=revision+1,updated_at=?
		WHERE id=? AND revision=? AND status=?`, strings.TrimSpace(reason), now, id,
		expectedRevision, TaskStatusPending)
	if err != nil {
		return Task{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return Task{}, ErrRevisionConflict
	}
	return s.GetTask(id)
}

// ResumePausedTaskPlanning 只恢复尚未生成任何SubTask的Planning等待。它把
// paused/waiting_input还原为pending/planning_subtasks，让同一Robot Agent在
// 新Run中结合用户答案继续规划；不能用于恢复已有物理Execution的Task。
func (s *Store) ResumePausedTaskPlanning(id string, expectedRevision int64, now time.Time) (Task, error) {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM subtasks WHERE task_id=?`, id).Scan(&count); err != nil {
		return Task{}, err
	}
	if count != 0 {
		return Task{}, ErrInvalidState
	}
	result, err := s.db.Exec(`UPDATE tasks SET status=?,waiting_reason=?,revision=revision+1,updated_at=?
		WHERE id=? AND revision=? AND status=? AND waiting_reason=?`, TaskStatusPending,
		"planning_subtasks", now, id, expectedRevision, TaskStatusPaused, "waiting_input")
	if err != nil {
		return Task{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return Task{}, ErrRevisionConflict
	}
	return s.GetTask(id)
}

// IsRobotTaskReserved 从持久化 Task 和直接 Run 派生 Robot 占用。
// excludeTaskID 允许已经分配的 Task 在真正启动前复核同一 Robot。
func (s *Store) IsRobotTaskReserved(robotID, excludeTaskID string) (bool, error) {
	robotID = strings.TrimSpace(robotID)
	if robotID == "" {
		return false, nil
	}
	err := robotReservation(s.db, robotID, strings.TrimSpace(excludeTaskID), "")
	if errors.Is(err, ErrRobotReserved) {
		return true, nil
	}
	return false, err
}

// AssignTask 在 Task 真正可运行时持久化 Worker/Robot 绑定。规划阶段只描述
// 角色和能力，因此分配必须发生在用户批准之后；revision 条件更新保证两个
// 并发调度循环不会把同一个 Task 派给不同 Robot。
func (s *Store) AssignTask(id string, expectedRevision int64, agentID, robotID string,
	now time.Time) (Task, error) {
	agentID = strings.TrimSpace(agentID)
	robotID = strings.TrimSpace(robotID)
	if agentID == "" {
		return Task{}, ErrInvalidState
	}
	current, err := s.GetTask(id)
	if err != nil {
		return Task{}, err
	}
	if current.Revision != expectedRevision || current.Status != TaskStatusPending {
		return Task{}, ErrRevisionConflict
	}
	if current.AssignedAgentID != "" {
		if current.AssignedAgentID == agentID && current.AssignedRobotID == robotID {
			return current, nil
		}
		return Task{}, ErrInvalidState
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Task{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if robotID != "" {
		if err := robotReservation(tx, robotID, id, ""); err != nil {
			return Task{}, err
		}
		if err := robotExecutionReservation(tx, robotID, id); err != nil {
			return Task{}, err
		}
		if err := robotPhysicalReservation(tx, robotID); err != nil {
			return Task{}, err
		}
	}
	result, err := tx.Exec(`UPDATE tasks SET assigned_agent_id=?,assigned_robot_id=?,
		assignment_revision=assignment_revision+1,revision=revision+1,waiting_reason='',updated_at=?
		WHERE id=? AND revision=? AND status=? AND assigned_agent_id=''`,
		agentID, robotID, now, id, expectedRevision, TaskStatusPending)
	if err != nil {
		if robotID != "" && strings.Contains(err.Error(), "UNIQUE constraint failed: tasks.assigned_robot_id") {
			return Task{}, ErrRobotReserved
		}
		return Task{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return Task{}, ErrRevisionConflict
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	return s.GetTask(id)
}

// ReassignPendingTask 只允许改派尚未启动、且没有任何 Robot Execution 事实的
// Task。Robot 临时离线时可以在用户批准的能力池内换设备；一旦已有物理命令，
// 即使 Task 表面仍是 pending，也必须先对账旧执行，不能通过改派来隐式重放。
func (s *Store) ReassignPendingTask(id string, expectedRevision int64,
	agentID, robotID string, now time.Time) (Task, error) {
	agentID = strings.TrimSpace(agentID)
	robotID = strings.TrimSpace(robotID)
	if agentID == "" {
		return Task{}, ErrInvalidState
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Task{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	var revision int64
	if err := tx.QueryRow(`SELECT status,revision FROM tasks WHERE id=?`, id).
		Scan(&status, &revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Task{}, ErrNotFound
		}
		return Task{}, err
	}
	if status != TaskStatusPending || revision != expectedRevision {
		return Task{}, ErrRevisionConflict
	}
	var executions int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM robot_executions WHERE json_extract(body,'$.task_id')=?`, id).
		Scan(&executions); err != nil {
		return Task{}, err
	}
	if executions != 0 {
		return Task{}, ErrInvalidState
	}
	if robotID != "" {
		if err := robotReservation(tx, robotID, id, ""); err != nil {
			return Task{}, err
		}
		if err := robotExecutionReservation(tx, robotID, id); err != nil {
			return Task{}, err
		}
		if err := robotPhysicalReservation(tx, robotID); err != nil {
			return Task{}, err
		}
	}
	result, err := tx.Exec(`UPDATE tasks SET assigned_agent_id=?,
		assigned_robot_id=?,assignment_revision=assignment_revision+1,
		revision=revision+1,waiting_reason='',updated_at=?
		WHERE id=? AND revision=? AND status=?`, agentID, robotID, now, id, expectedRevision, TaskStatusPending)
	if err != nil {
		if robotID != "" && strings.Contains(err.Error(), "UNIQUE constraint failed: tasks.assigned_robot_id") {
			return Task{}, ErrRobotReserved
		}
		return Task{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return Task{}, ErrRevisionConflict
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	return s.GetTask(id)
}

func (s *Store) GetSubTask(id string) (SubTask, error) {
	v, err := scanSubTask(s.db.QueryRow(`SELECT `+subTaskColumns+` FROM subtasks WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return SubTask{}, ErrNotFound
	}
	return v, err
}

func (s *Store) ListSubTasks(taskID string) ([]SubTask, error) {
	return listSubTasksQuery(s.db, taskID)
}

// ValidateSubTaskDrafts checks the same graph contract used at persistence time,
// without assigning IDs into the caller's plan. Agent review can therefore
// reject an invalid plan inside its correction loop, not after that loop exits.
func ValidateSubTaskDrafts(items []SubTaskDraft) error {
	return validateSubTaskDrafts(append([]SubTaskDraft(nil), items...))
}

func validateSubTaskDrafts(items []SubTaskDraft) error {
	if len(items) == 0 {
		return ErrInvalidState
	}
	ids := make(map[string]struct{}, len(items))
	for index := range items {
		if strings.TrimSpace(items[index].Goal) == "" || !ValidSubTaskKind(items[index].Kind) {
			return ErrInvalidState
		}
		if items[index].ID == "" {
			items[index].ID = NewSubTaskID()
		}
		if _, exists := ids[items[index].ID]; exists {
			return ErrInvalidState
		}
		ids[items[index].ID] = struct{}{}
	}
	indegree := make(map[string]int, len(ids))
	adjacency := make(map[string][]string, len(ids))
	for id := range ids {
		indegree[id] = 0
	}
	for _, item := range items {
		for _, dependency := range item.DependsOn {
			if dependency == item.ID {
				return ErrInvalidState
			}
			if _, ok := ids[dependency]; !ok {
				return ErrInvalidState
			}
			adjacency[dependency] = append(adjacency[dependency], item.ID)
			indegree[item.ID]++
		}
	}
	queue := make([]string, 0, len(ids))
	for id, degree := range indegree {
		if degree == 0 {
			queue = append(queue, id)
		}
	}
	visited := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		visited++
		for _, next := range adjacency[id] {
			indegree[next]--
			if indegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if visited != len(ids) {
		return ErrInvalidState
	}
	return nil
}

func allocateSubTaskIDsTx(tx *sql.Tx, items []SubTaskDraft) (map[string]string, error) {
	ids := make(map[string]string, len(items))
	for _, item := range items {
		id := item.ID
		var conflicts int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM subtasks WHERE id=?`, id).Scan(&conflicts); err != nil {
			return nil, err
		}
		if conflicts != 0 {
			id = NewSubTaskID()
		}
		ids[item.ID] = id
	}
	return ids, nil
}

// SetTaskSubTasks 只允许尚未启动且尚无 SubTask 的 Task Agent 写入第一次计划。
// Task/SubTask 依赖在一个事务中落库；重试读取原计划，不会重复创建另一组步骤。
func (s *Store) SetTaskSubTasks(taskID string, expectedRevision int64,
	items []SubTaskDraft, now time.Time) (Task, []SubTask, error) {
	if err := validateSubTaskDrafts(items); err != nil {
		return Task{}, nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Task{}, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	task, err := scanTask(tx.QueryRow(`SELECT `+taskColumns+` FROM tasks WHERE id=?`, taskID))
	if err != nil {
		return Task{}, nil, err
	}
	if task.Revision != expectedRevision || task.Status != TaskStatusPending {
		return Task{}, nil, ErrRevisionConflict
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM subtasks WHERE task_id=?`, taskID).Scan(&count); err != nil {
		return Task{}, nil, err
	}
	if count != 0 {
		return Task{}, nil, ErrInvalidState
	}
	subTaskIDs, err := allocateSubTaskIDsTx(tx, items)
	if err != nil {
		return Task{}, nil, err
	}
	for position := range items {
		item := &items[position]
		subTaskID := subTaskIDs[item.ID]
		if _, err := tx.Exec(`INSERT INTO subtasks (id,task_id,position,kind,goal,
			spec_json,criteria_json,status,created_at,updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, subTaskID, taskID, position, item.Kind,
			strings.TrimSpace(item.Goal), jsonOr(item.Spec, "{}"),
			jsonOr(item.CompletionCriteria, "[]"), TaskStatusPending, now, now); err != nil {
			return Task{}, nil, err
		}
		for _, dependency := range item.DependsOn {
			if _, err := tx.Exec(`INSERT INTO subtask_dependencies
				(task_id,subtask_id,depends_on_subtask_id) VALUES (?,?,?)`,
				taskID, subTaskID, subTaskIDs[dependency]); err != nil {
				return Task{}, nil, err
			}
		}
	}
	result, err := tx.Exec(`UPDATE tasks SET revision=revision+1,updated_at=?
		WHERE id=? AND revision=? AND status=?`, now, taskID, expectedRevision, TaskStatusPending)
	if err != nil {
		return Task{}, nil, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return Task{}, nil, ErrRevisionConflict
	}
	if err := tx.Commit(); err != nil {
		return Task{}, nil, err
	}
	updated, err := s.GetTask(taskID)
	if err != nil {
		return Task{}, nil, err
	}
	subs, err := s.ListSubTasks(taskID)
	return updated, subs, err
}

// ApplyFailedRobotRecovery 在一个事务中替换“尚未开始的剩余计划”。
// 只有 Pilot 已明确记录为 failed 的 Robot Execution 才能进入这里；interrupted
// 或无法确认的执行保持 paused 和 Robot 锁，避免一次恢复决策重放物理动作。
func (s *Store) ApplyFailedRobotRecovery(taskID string, expectedTaskRevision int64,
	failedSubTaskID string, replacements []SubTaskDraft, now time.Time) (Task, []SubTask, error) {
	if err := validateSubTaskDrafts(replacements); err != nil {
		return Task{}, nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Task{}, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	task, err := scanTask(tx.QueryRow(`SELECT `+taskColumns+` FROM tasks WHERE id=?`, taskID))
	if err != nil {
		return Task{}, nil, err
	}
	if task.Revision != expectedTaskRevision || task.Status != TaskStatusPaused {
		return Task{}, nil, ErrRevisionConflict
	}
	failed, err := scanSubTask(tx.QueryRow(`SELECT `+subTaskColumns+` FROM subtasks WHERE id=?`, failedSubTaskID))
	if err != nil {
		return Task{}, nil, err
	}
	if failed.TaskID != taskID || failed.Status != TaskStatusPaused || failed.Kind != "robot_skill" || failed.ExecutionRef == "" {
		return Task{}, nil, ErrInvalidState
	}
	var executionStatus string
	if err := tx.QueryRow(`SELECT status FROM robot_executions WHERE id=? AND subtask_id=?`,
		failed.ExecutionRef, failed.ID).Scan(&executionStatus); err != nil {
		return Task{}, nil, err
	}
	if executionStatus != "failed" {
		return Task{}, nil, ErrInvalidState
	}
	if _, err := tx.Exec(`UPDATE subtasks SET status=?,waiting_reason=?,revision=revision+1,
		updated_at=?,ended_at=? WHERE id=? AND revision=? AND status=?`, TaskStatusFailed,
		"replaced_after_failure", now, now, failed.ID, failed.Revision, TaskStatusPaused); err != nil {
		return Task{}, nil, err
	}
	if _, err := tx.Exec(`UPDATE subtasks SET status=?,waiting_reason=?,revision=revision+1,
		updated_at=?,ended_at=? WHERE task_id=? AND status=?`, TaskStatusStopped,
		"revised_after_failure", now, now, taskID, TaskStatusPending); err != nil {
		return Task{}, nil, err
	}
	var position int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(position),-1)+1 FROM subtasks WHERE task_id=?`, taskID).Scan(&position); err != nil {
		return Task{}, nil, err
	}
	replacementIDs, err := allocateSubTaskIDsTx(tx, replacements)
	if err != nil {
		return Task{}, nil, err
	}
	for index := range replacements {
		item := &replacements[index]
		subTaskID := replacementIDs[item.ID]
		if _, err := tx.Exec(`INSERT INTO subtasks (id,task_id,position,kind,goal,
			spec_json,criteria_json,status,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			subTaskID, taskID, position+index, item.Kind, strings.TrimSpace(item.Goal),
			jsonOr(item.Spec, "{}"), jsonOr(item.CompletionCriteria, "[]"),
			TaskStatusPending, now, now); err != nil {
			return Task{}, nil, err
		}
		for _, dependency := range item.DependsOn {
			if _, err := tx.Exec(`INSERT INTO subtask_dependencies
				(task_id,subtask_id,depends_on_subtask_id) VALUES (?,?,?)`, taskID,
				subTaskID, replacementIDs[dependency]); err != nil {
				return Task{}, nil, err
			}
		}
	}
	result, err := tx.Exec(`UPDATE tasks SET status=?,waiting_reason="",revision=revision+1,
		updated_at=? WHERE id=? AND revision=? AND status=?`, TaskStatusRunning, now,
		taskID, expectedTaskRevision, TaskStatusPaused)
	if err != nil {
		return Task{}, nil, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return Task{}, nil, ErrRevisionConflict
	}
	if err := tx.Commit(); err != nil {
		return Task{}, nil, err
	}
	updated, err := s.GetTask(taskID)
	if err != nil {
		return Task{}, nil, err
	}
	items, err := s.ListSubTasks(taskID)
	return updated, items, err
}

// ResumePausedAgentStep 只供用户显式恢复或已回答 Interaction 使用。普通 Agent
// 步骤没有外部物理执行时可以重新排队；Robot Skill 仅在 Server 重启且确认
// 尚未建立 execution_ref 时可安全回到 pending。已有物理执行必须先按原
// Execution 对账，不能通过恢复 Workflow 隐式重放。
func (s *Store) ResumePausedAgentStep(id string, expectedRevision int64, now time.Time) (SubTask, error) {
	result, err := s.db.Exec(`UPDATE subtasks SET status=?,waiting_reason='',
		revision=revision+1,updated_at=? WHERE id=? AND revision=? AND status=?
		AND execution_ref='' AND (kind='agent_step' OR
		(kind='robot_skill' AND waiting_reason='server_restarted'))`, TaskStatusPending, now, id,
		expectedRevision, TaskStatusPaused)
	if err != nil {
		return SubTask{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return SubTask{}, ErrInvalidState
	}
	return s.GetSubTask(id)
}

// ListRunnableSubTasks 返回依赖均完成的 pending SubTask；调用方仍需检查
// Robot/工作区等资源锁，依赖图本身只表达数据顺序。
func (s *Store) ListRunnableSubTasks(taskID string) ([]SubTask, error) {
	rows, err := s.db.Query(`SELECT `+subTaskColumns+` FROM subtasks s
		WHERE s.task_id=? AND s.status=? AND NOT EXISTS (
			SELECT 1 FROM subtask_dependencies d
			JOIN subtasks predecessor ON predecessor.id=d.depends_on_subtask_id
			WHERE d.subtask_id=s.id AND predecessor.status<>?
		) ORDER BY s.position,s.id`, taskID, TaskStatusPending, TaskStatusCompleted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]SubTask, 0)
	for rows.Next() {
		value, scanErr := scanSubTask(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

// TransitionSubTask 按 revision 推进 SubTask；其状态变化不隐式改变 Task。
func (s *Store) TransitionSubTask(id string, expectedRevision int64, to, reason string, result json.RawMessage, now time.Time) (SubTask, error) {
	current, err := s.GetSubTask(id)
	if err != nil {
		return SubTask{}, err
	}
	if current.Revision != expectedRevision {
		return SubTask{}, ErrRevisionConflict
	}
	if !allowedTaskTransitions[current.Status][to] {
		return SubTask{}, ErrInvalidState
	}
	var ended any
	if terminalWorkStatus(to) {
		ended = now
	}
	res, err := s.db.Exec(`UPDATE subtasks SET status=?,waiting_reason=?,result_json=?,revision=revision+1,updated_at=?,ended_at=? WHERE id=? AND revision=?`, to, reason, jsonOr(result, "{}"), now, ended, id, expectedRevision)
	if err != nil {
		return SubTask{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return SubTask{}, ErrRevisionConflict
	}
	return s.GetSubTask(id)
}

// AttachSubTaskExecution 在 robot.run 被接受后记录精确 Robot Execution。
// 这里不改变 SubTask 状态：accepted 只证明 Pilot 接受请求，完成仍由后续
// Robot Execution 终态与 Skill Result 驱动。
func (s *Store) AttachSubTaskExecution(id string, expectedRevision int64,
	executionID string, now time.Time) (SubTask, error) {
	executionID = strings.TrimSpace(executionID)
	if executionID == "" {
		return SubTask{}, ErrInvalidState
	}
	result, err := s.db.Exec(`UPDATE subtasks SET execution_ref=?,revision=revision+1,updated_at=?
		WHERE id=? AND revision=? AND status=? AND execution_ref=''`, executionID, now,
		id, expectedRevision, TaskStatusRunning)
	if err != nil {
		return SubTask{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return SubTask{}, ErrRevisionConflict
	}
	return s.GetSubTask(id)
}

// ListRunnableTasks 只返回依赖均已完成的 pending Task；顺序稳定。
func (s *Store) ListRunnableTasks(workflowID string) ([]Task, error) {
	rows, err := s.db.Query(`SELECT `+taskColumns+` FROM tasks t WHERE t.workflow_id=? AND t.status=?
		AND NOT EXISTS (SELECT 1 FROM task_dependencies d JOIN tasks p ON p.id=d.depends_on_task_id
		WHERE d.task_id=t.id AND p.status<>?) ORDER BY t.position,t.id`, workflowID, TaskStatusPending, TaskStatusCompleted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Task, 0)
	for rows.Next() {
		v, e := scanTask(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
