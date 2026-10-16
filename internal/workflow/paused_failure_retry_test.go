package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

func TestExplicitRetryStartsRecoveryForFailureArrivingWhileWorkflowPaused(t *testing.T) {
	for _, confirmed := range []bool{true, false} {
		name := "confirmed_stop"
		if !confirmed {
			name = "unconfirmed_stop"
		}
		t.Run(name, func(t *testing.T) {
			st, project, conversation, service, _, executor := newWorkflowFixture(t)
			recovery := &recoveringExecutor{controlledExecutor: executor,
				requests: make(chan TaskRecoveryRequest, 1),
				decision: TaskRecoveryDecision{Decision: "revise_pending", Replacements: []store.SubTaskDraft{
					{ID: "refresh-navigation", Kind: "robot_skill", Goal: "Read current navigation state before retry",
						Spec: json.RawMessage(`{"skill_name":"navigation","intent":{"task":"state"}}`)},
				}}}
			service.executor = recovery
			view, task, steps := prepareRunningRobotTask(t, st, project, conversation,
				[]store.SubTaskDraft{{ID: "navigate", Kind: "robot_skill", Goal: "Navigate to destination"}})
			paused, err := service.PauseWorkflow(context.Background(), project.OwnerID, project.ID,
				view.Workflow.ID, view.Workflow.Revision)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			execution := store.RobotExecution{ID: "rex-navigation-timeout", ProjectID: project.ID,
				WorkflowID: view.Workflow.ID, TaskID: task.ID, SubtaskID: steps[0].ID,
				RobotID: task.AssignedRobotID, Status: "failed", Revision: 1,
				Error: map[string]any{"code": "deadline_exceeded"}, CreatedAt: now, UpdatedAt: now}
			if err := st.SaveRobotExecution(execution); err != nil {
				t.Fatal(err)
			}
			for i, event := range []struct {
				kind string
				body map[string]any
			}{
				{"action.started", map[string]any{"action_id": "nav", "physical": true, "physical_started": true,
					"execution_protocol": "sdk-task@1.0.0", "framework_task_id": "sdk-nav", "ability_instance_id": "nav-instance"}},
				{"action.terminal", map[string]any{"action_id": "nav", "status": "failed", "error": execution.Error,
					"execution_protocol": "sdk-task@1.0.0", "framework_task_id": "sdk-nav", "ability_instance_id": "nav-instance",
					"stop_evidence": map[string]any{"confirmed": confirmed}}},
			} {
				if err := st.AppendRobotExecutionEvent(store.RobotExecutionEvent{ExecutionID: execution.ID,
					Sequence: int64(i + 1), Type: event.kind, Payload: event.body, CreatedAt: now}); err != nil {
					t.Fatal(err)
				}
			}
			if err := service.OnRobotExecutionChanged(context.Background(), execution, "execution.terminal", execution.Error); err != nil {
				t.Fatal(err)
			}
			current, _ := st.GetTask(task.ID)
			wantReason := "robot_execution_failed"
			if !confirmed {
				wantReason = "execution_state_unknown"
			}
			if current.Status != store.TaskStatusPaused || current.WaitingReason != wantReason {
				t.Fatalf("unexpected paused failure: %+v", current)
			}
			select {
			case <-recovery.requests:
				t.Fatal("recovery started while workflow was paused")
			default:
			}
			_, err = service.RetryRobotAgentDecision(context.Background(), project.OwnerID, project.ID,
				view.Workflow.ID, paused.Workflow.Revision)
			if !confirmed {
				if err == nil {
					t.Fatal("unconfirmed motion allowed retry")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-recovery.requests:
				if got.Execution.ID != execution.ID {
					t.Fatal("wrong failed execution supplied to recovery")
				}
			case <-time.After(workflowEventWait):
				t.Fatal("deferred recovery was not started")
			}
			next := receiveExecution(t, executor)
			defer func() { executor.release <- struct{}{} }()
			if next.SubTasks[0].ID != "refresh-navigation" {
				t.Fatal("replayed failed navigation instead of recovery prefix")
			}
			old, _ := st.GetRobotExecution(execution.ID)
			if old.Status != "failed" {
				t.Fatal("failed execution history was overwritten")
			}
		})
	}
}
