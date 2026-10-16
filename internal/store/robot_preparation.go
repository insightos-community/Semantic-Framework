package store

import (
	"encoding/json"
	"time"
)

// TransitionRobotPreparation changes the Task and its unstarted Robot SubTask
// together. Existing executions (even not yet attached to execution_ref) must
// continue through execution reconciliation, never through this preparation path.
func (s *Store) TransitionRobotPreparation(task Task, step SubTask, resume bool,
	result json.RawMessage, now time.Time) (Task, error) {
	from, to, stepTo, reason := TaskStatusRunning, TaskStatusPaused, TaskStatusPaused, "waiting_resource"
	if resume {
		from, to, stepTo, reason = TaskStatusPaused, TaskStatusRunning, TaskStatusPending, ""
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Task{}, err
	}
	defer func() { _ = tx.Rollback() }()
	changed, err := tx.Exec(`UPDATE subtasks SET status=?,waiting_reason=?,result_json=?,
		revision=revision+1,updated_at=? WHERE id=? AND task_id=? AND revision=? AND status=?
		AND kind='robot_skill' AND execution_ref=''
		AND (?=0 OR waiting_reason='waiting_resource')
		AND NOT EXISTS (SELECT 1 FROM robot_executions e WHERE e.subtask_id=subtasks.id)`,
		stepTo, reason, jsonOr(result, "{}"), now, step.ID, task.ID, step.Revision, from, resume)
	if err != nil {
		return Task{}, err
	}
	if count, _ := changed.RowsAffected(); count != 1 {
		return Task{}, ErrInvalidState
	}
	changed, err = tx.Exec(`UPDATE tasks SET status=?,waiting_reason=?,revision=revision+1,updated_at=?
		WHERE id=? AND revision=? AND status=? AND required_role='robot'
		AND (?=0 OR waiting_reason='waiting_resource')
		AND EXISTS (SELECT 1 FROM workflows w WHERE w.id=tasks.workflow_id AND w.status='running')`,
		to, reason, now, task.ID, task.Revision, from, resume)
	if err != nil {
		return Task{}, err
	}
	if count, _ := changed.RowsAffected(); count != 1 {
		return Task{}, ErrInvalidState
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	return s.GetTask(task.ID)
}
