package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

func TestHeldPhysicalFailureUsesRecoveryAndKeepsFailedExecution(t *testing.T) {
	for _, scenario := range []string{"recover", "wrong_task_evidence", "unconfirmed_stop", "explicit_stop"} {
		t.Run(scenario, func(t *testing.T) {
			st, project, conversation, service, _, executor := newWorkflowFixture(t)
			recovery := &recoveringExecutor{controlledExecutor: executor,
				requests: make(chan TaskRecoveryRequest, 2),
				decision: TaskRecoveryDecision{Decision: "revise_pending", Replacements: []store.SubTaskDraft{
					{ID: "refresh-current-state", Kind: "robot_skill", Goal: "查询当前姿态后补齐运输准备",
						Spec: json.RawMessage(`{"skill_name":"r1pro-manipulation","skill_version":"test","intent":{"task":"state"}}`)},
				}}}
			service.executor = recovery
			view, task, steps := prepareRunningRobotTask(t, st, project, conversation,
				[]store.SubTaskDraft{{ID: "navigation", Kind: "robot_skill", Goal: "导航"}})
			now := time.Now().UTC()
			execution := store.RobotExecution{ID: "rex-transport-rejected", ProjectID: project.ID,
				WorkflowID: view.Workflow.ID, TaskID: task.ID, SubtaskID: steps[0].ID, RobotID: task.AssignedRobotID,
				SkillName: "r1pro-navigate", SkillVersion: "test", Status: "failed", Revision: 1,
				Error:     map[string]any{"code": "command_rejected", "retryable": false, "message": "尚未进入已配置的直立运输姿态"},
				CreatedAt: now, UpdatedAt: now}
			if err := st.SaveRobotExecution(execution); err != nil {
				t.Fatal(err)
			}
			terminalTask := "sdk-task"
			if scenario == "wrong_task_evidence" {
				terminalTask = "another-task"
			}
			for i, entry := range []struct {
				kind    string
				payload map[string]any
			}{
				{"action.started", map[string]any{"action_id": "action-nav", "physical": true, "physical_started": true, "execution_protocol": "sdk-task@1.0.0", "framework_task_id": "sdk-task", "ability_instance_id": "nav-instance"}},
				{"action.terminal", map[string]any{"action_id": "action-nav", "execution_protocol": "sdk-task@1.0.0", "framework_task_id": terminalTask, "ability_instance_id": "nav-instance", "status": "failed", "error": execution.Error,
					"stop_evidence": map[string]any{"confirmed": scenario != "unconfirmed_stop"}}},
			} {
				if err := st.AppendRobotExecutionEvent(store.RobotExecutionEvent{ExecutionID: execution.ID, Sequence: int64(i + 1), Type: entry.kind, Payload: entry.payload, CreatedAt: now}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "explicit_stop" {
				if _, err := st.TransitionWorkflowWithReason(view.Workflow.ID, view.Workflow.Revision, store.WorkflowStatusStopping, "user_stopped", now); err != nil {
					t.Fatal(err)
				}
				if _, err := service.moveTaskTowardStop(task); err != nil {
					t.Fatal(err)
				}
				if _, err := service.moveSubTaskTowardStop(steps[0]); err != nil {
					t.Fatal(err)
				}
			}
			if err := service.OnRobotExecutionChanged(context.Background(), execution, "execution.terminal", execution.Error); err != nil {
				t.Fatal(err)
			}
			if scenario != "recover" {
				current, _ := st.GetTask(task.ID)
				if scenario == "explicit_stop" {
					if current.Status != store.TaskStatusStopped {
						t.Fatalf("explicit stop not honored: %+v", current)
					}
				} else if current.Status != store.TaskStatusPaused || current.WaitingReason != "execution_state_unknown" {
					t.Fatalf("unconfirmed proof entered recovery: %+v", current)
				}
				select {
				case got := <-recovery.requests:
					t.Fatalf("unexpected recovery: %+v", got)
				default:
				}
				return
			}
			select {
			case request := <-recovery.requests:
				if len(request.ActionEvidence) != 2 || request.ActionEvidence[1]["framework_task_id"] != "sdk-task" {
					t.Fatalf("recovery lost exact proof: %+v", request)
				}
			case <-time.After(workflowEventWait):
				t.Fatal("held failure never entered recovery")
			}
			next := receiveExecution(t, executor)
			defer func() { executor.release <- struct{}{} }()
			if next.SubTasks[0].ID != "refresh-current-state" {
				t.Fatalf("old action replayed: %+v", next)
			}
			if err := service.OnRobotExecutionChanged(context.Background(), execution, "execution.terminal", execution.Error); err != nil {
				t.Fatal(err)
			}
			current, _ := st.GetTask(task.ID)
			old, _ := st.GetSubTask(steps[0].ID)
			preserved, _ := st.GetRobotExecution(execution.ID)
			if current.Status != store.TaskStatusRunning || old.WaitingReason != "replaced_after_failure" || preserved.Status != "failed" {
				t.Fatalf("duplicate failure changed replacement or history: %+v %+v %+v", current, old, preserved)
			}
			select {
			case got := <-recovery.requests:
				t.Fatalf("duplicate recovery: %+v", got)
			default:
			}
		})
	}
}
