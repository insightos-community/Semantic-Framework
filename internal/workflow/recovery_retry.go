package workflow

import (
	"context"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

// Retry the failed read-only recovery decision, never the failed robot Action.
// This is the existing explicit retry-decision endpoint, with the same stored
// evidence and replacement transaction as automatic recovery.
func (s *Service) retryExhaustedTaskRecovery(ctx context.Context, view store.WorkflowView) (store.WorkflowView, error) {
	recovery, ok := s.executor.(TaskRecoveryExecutor)
	if !ok {
		return store.WorkflowView{}, ErrExecutorUnavailable
	}
	var task *store.Task
	for _, item := range view.Tasks {
		// A Server restart or explicit pause ends the read-only Run, but Task/SubTask
		// records can still say recovery_running. Explicit retry may reclaim
		// that abandoned decision; a live recovery remains ineligible.
		interrupted := item.WaitingReason == "recovery_running" &&
			view.Workflow.Status == store.WorkflowStatusPaused &&
			(view.Workflow.Reason == "server_restarted" || view.Workflow.Reason == "user_paused")
		// A physical action may finish with a confirmed failure after the operator
		// pauses scheduling. Automatic recovery correctly stays stopped, leaving
		// robot_execution_failed rather than recovery_running/exhausted. Explicit
		// retry must be able to start that first recovery with the same proof checks.
		deferred := item.WaitingReason == "robot_execution_failed" &&
			view.Workflow.Status == store.WorkflowStatusPaused &&
			(view.Workflow.Reason == "server_restarted" || view.Workflow.Reason == "user_paused")
		if item.Status == store.TaskStatusPaused && (item.WaitingReason == "recovery_exhausted" || interrupted || deferred) {
			if task != nil {
				return store.WorkflowView{}, store.ErrInvalidState
			}
			copy := item
			task = &copy
		}
	}
	if task == nil || task.RequiredRole != "robot" {
		return store.WorkflowView{}, store.ErrInvalidState
	}
	var subtask *store.SubTask
	for _, item := range view.SubTasks {
		if item.TaskID == task.ID && item.Status == store.TaskStatusPaused &&
			item.WaitingReason == task.WaitingReason && item.Kind == "robot_skill" && item.ExecutionRef != "" {
			if subtask != nil {
				return store.WorkflowView{}, store.ErrInvalidState
			}
			copy := item
			subtask = &copy
		}
	}
	if subtask == nil {
		return store.WorkflowView{}, store.ErrInvalidState
	}
	execution, err := s.st.GetRobotExecution(subtask.ExecutionRef)
	if err != nil {
		return store.WorkflowView{}, err
	}
	if execution.Status != "failed" || execution.ProjectID != view.Workflow.ProjectID ||
		execution.WorkflowID != view.Workflow.ID || execution.TaskID != task.ID ||
		execution.SubtaskID != subtask.ID || execution.RobotID != task.AssignedRobotID {
		return store.WorkflowView{}, store.ErrInvalidState
	}
	physical, err := s.robotExecutionStartedPhysicalAction(execution.ID)
	if err != nil {
		return store.WorkflowView{}, err
	}
	if physical && !safelyStoppedRobotExecution(execution) {
		held, err := s.robotExecutionSafelyHeld(execution.ID)
		if err != nil {
			return store.WorkflowView{}, err
		}
		if !held {
			return store.WorkflowView{}, store.ErrInvalidState
		}
	}
	if !s.reserveTask(*task) {
		return store.WorkflowView{}, ErrTaskBusy
	}
	runningWorkflow := view.Workflow
	if view.Workflow.Status == store.WorkflowStatusPaused {
		runningWorkflow, err = s.st.TransitionWorkflow(view.Workflow.ID, view.Workflow.Revision,
			store.WorkflowStatusRunning, time.Now().UTC())
		if err != nil {
			s.releaseTask(*task)
			return store.WorkflowView{}, err
		}
	}
	updatedTask, updatedSubtask, err := s.st.UpdatePausedRobotWaitReason(task.ID, task.Revision,
		subtask.ID, subtask.Revision, task.WaitingReason, "recovery_running", time.Now().UTC())
	if err != nil {
		if view.Workflow.Status == store.WorkflowStatusPaused {
			_, _ = s.st.TransitionWorkflowWithReason(runningWorkflow.ID, runningWorkflow.Revision,
				view.Workflow.Status, view.Workflow.Reason, time.Now().UTC())
		}
		s.releaseTask(*task)
		return store.WorkflowView{}, err
	}
	s.launchReservedTaskRecovery(context.WithoutCancel(ctx), updatedTask, updatedSubtask, execution, nil, recovery)
	result, err := s.st.GetWorkflowView(view.Workflow.ID)
	if err == nil {
		s.publishView(result, "workflow.task_recovery_retried")
	}
	return result, err
}
