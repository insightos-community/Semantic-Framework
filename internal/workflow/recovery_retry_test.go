package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

type retryRecoveryExecutor struct {
	*recoveringExecutor
	attempts atomic.Int32
}

func (e *retryRecoveryExecutor) RecoverTask(ctx context.Context, request TaskRecoveryRequest) (TaskRecoveryDecision, error) {
	if e.attempts.Add(1) == 1 {
		e.requests <- request
		return TaskRecoveryDecision{}, errors.New("read-only decision failed")
	}
	return e.recoveringExecutor.RecoverTask(ctx, request)
}

func TestExplicitRecoveryRetryPreservesFailureAndDoesNotReplayAction(t *testing.T) {
	for _, scenario := range []string{"retry", "server_restarted", "unknown", "user_paused", "user_paused_unknown", "interrupted_recovery", "interrupted_recovery_unknown", "paused_recovery", "paused_recovery_unknown", "paused_live_recovery", "live_recovery"} {
		t.Run(scenario, func(t *testing.T) {
			st, project, conversation, service, _, executor := newWorkflowFixture(t)
			recovery := &retryRecoveryExecutor{recoveringExecutor: &recoveringExecutor{
				controlledExecutor: executor, requests: make(chan TaskRecoveryRequest, 3),
				decision: TaskRecoveryDecision{Decision: "revise_pending", Replacements: []store.SubTaskDraft{
					{ID: "correct-current-step", Kind: "robot_skill", Goal: "execute the missing segment",
						Spec: json.RawMessage(`{"skill_name":"manipulation","intent":{"task":"execute_prepared"}}`)},
				}}}}
			service.executor = recovery
			view, task, steps := prepareRunningRobotTask(t, st, project, conversation,
				[]store.SubTaskDraft{{ID: "verify", Kind: "robot_skill", Goal: "verify placement"}})
			execution := store.RobotExecution{ID: "rex-verify", ProjectID: project.ID, WorkflowID: view.Workflow.ID,
				TaskID: task.ID, SubtaskID: steps[0].ID, RobotID: task.AssignedRobotID, Status: "failed",
				Error: map[string]any{"code": "planning_failed"}, Revision: 1, UpdatedAt: time.Now().UTC()}
			if err := st.SaveRobotExecution(execution); err != nil {
				t.Fatal(err)
			}
			if err := service.OnRobotExecutionChanged(context.Background(), execution, "execution.failed", execution.Error); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(workflowEventWait)
			for {
				latest, _ := st.GetTask(task.ID)
				service.mu.Lock()
				_, busy := service.activeTasks[task.ID]
				service.mu.Unlock()
				if latest.WaitingReason == "recovery_exhausted" && !busy {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("recovery did not release its failed decision")
				}
				time.Sleep(5 * time.Millisecond)
			}
			<-recovery.requests
			current, _ := st.GetWorkflowView(view.Workflow.ID)
			unknown := scenario == "unknown" || scenario == "user_paused_unknown" || scenario == "interrupted_recovery_unknown" || scenario == "paused_recovery_unknown"
			pausedRecovery := scenario == "paused_recovery" || scenario == "paused_recovery_unknown" || scenario == "paused_live_recovery"
			interrupted := scenario == "interrupted_recovery" || scenario == "interrupted_recovery_unknown" || pausedRecovery
			if unknown {
				if err := st.AppendRobotExecutionEvent(store.RobotExecutionEvent{ExecutionID: execution.ID, Sequence: 1,
					Type: "action.started", Payload: map[string]any{"action_id": "unknown-motion", "physical": true, "physical_started": true}}); err != nil {
					t.Fatal(err)
				}
			}
			if interrupted || scenario == "live_recovery" {
				latestTask, _ := st.GetTask(task.ID)
				latestStep, _ := st.GetSubTask(steps[0].ID)
				if _, _, err := st.UpdatePausedRobotWaitReason(latestTask.ID, latestTask.Revision,
					latestStep.ID, latestStep.Revision, "recovery_exhausted", "recovery_running", time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "user_paused" || scenario == "user_paused_unknown" || scenario == "server_restarted" || interrupted {
				reason := scenario
				if interrupted {
					reason = "server_restarted"
				}
				if pausedRecovery {
					reason = "user_paused"
				}
				if reason == "user_paused_unknown" {
					reason = "user_paused"
				}
				_, err := st.TransitionWorkflowWithReason(view.Workflow.ID, current.Workflow.Revision, store.WorkflowStatusPaused, reason, time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
				current, _ = st.GetWorkflowView(view.Workflow.ID)
			}
			// Pausing alone must not launch recovery. Only the explicit action below does.
			if scenario == "paused_live_recovery" {
				latest, _ := st.GetTask(task.ID)
				if !service.reserveTask(latest) {
					t.Fatal("could not simulate active recovery")
				}
				defer service.releaseTask(latest)
			}
			if recovery.attempts.Load() != 1 {
				t.Fatal("recovery started without an explicit retry")
			}
			_, err := service.RetryRobotAgentDecision(context.Background(), project.OwnerID, project.ID,
				view.Workflow.ID, current.Workflow.Revision)
			if unknown || scenario == "live_recovery" || scenario == "paused_live_recovery" {
				if err == nil {
					t.Fatal("retry bypassed unknown motion")
				}
				if recovery.attempts.Load() != 1 {
					t.Fatal("unexpected recovery")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-recovery.requests:
				if got.Execution.ID != execution.ID || got.Execution.Status != "failed" {
					t.Fatal("lost failed execution")
				}
			case <-time.After(workflowEventWait):
				t.Fatal("recovery was not retried")
			}
			next := receiveExecution(t, executor)
			defer func() { executor.release <- struct{}{} }()
			if next.SubTasks[0].ID != "correct-current-step" {
				t.Fatal("replayed original step")
			}
			old, _ := st.GetRobotExecution(execution.ID)
			if old.Status != "failed" {
				t.Fatal("rewrote failed history")
			}
		})
	}
}
