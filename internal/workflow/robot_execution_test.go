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

package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	robotdomain "insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/store"
)

func prepareRunningRobotTask(t *testing.T, st *store.Store, project store.Project,
	conversation store.ChatSession, drafts []store.SubTaskDraft) (store.WorkflowView, store.Task, []store.SubTask) {
	t.Helper()
	now := time.Now().UTC()
	ready, err := st.SubmitPlanProposal(project.ID, conversation.ID,
		store.WorkflowDraft{Goal: "搬运", Tasks: []store.TaskDraft{{
			ID: "task-robot", RequiredRole: "robot", Goal: "搬运一个箱子",
		}}}, "", nil, "# 搬运", now)
	if err != nil {
		t.Fatal(err)
	}
	view, err := st.ApprovePlanProposal(ready.ID, ready.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.AssignTask(view.Tasks[0].ID, view.Tasks[0].Revision,
		"robot:r1pro-test", "r1pro-test", now)
	if err != nil {
		t.Fatal(err)
	}
	task, subTasks, err := st.SetTaskSubTasks(task.ID, task.Revision, drafts, now)
	if err != nil {
		t.Fatal(err)
	}
	task, err = st.TransitionTask(task.ID, task.Revision, store.TaskStatusRunning,
		"", "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	subTasks[0], err = st.TransitionSubTask(subTasks[0].ID, subTasks[0].Revision,
		store.TaskStatusRunning, "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	return view, task, subTasks
}

func TestRobotExecutionCompletedAdvancesOnlyItsSubTask(t *testing.T) {
	st, project, conversation, service, _, executor := newWorkflowFixture(t)
	view, task, subTasks := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{
			{ID: "sub-grasp", Kind: "robot_skill", Goal: "抓取"},
			{ID: "sub-place", Kind: "robot_skill", Goal: "放置", DependsOn: []string{"sub-grasp"}},
		})
	execution := store.RobotExecution{ID: "rex-grasp", ProjectID: project.ID,
		WorkflowID: view.Workflow.ID, TaskID: task.ID, SubtaskID: subTasks[0].ID,
		RobotID: "r1pro-test", SkillName: "grasp-object", SkillVersion: "0.1.0",
		Status: "completed", Result: map[string]any{"held_object_ref": "box-17"}}
	if err := service.OnRobotExecutionChanged(context.Background(), execution,
		"execution.terminal", execution.Result); err != nil {
		t.Fatal(err)
	}
	completed, _ := st.GetSubTask(subTasks[0].ID)
	pending, _ := st.GetSubTask(subTasks[1].ID)
	if completed.Status != store.TaskStatusCompleted || completed.ExecutionRef != execution.ID {
		t.Fatalf("完成事件没有收敛到精确 SubTask: %+v", completed)
	}
	if pending.Status != store.TaskStatusPending {
		t.Fatalf("不得批量完成后续 SubTask: %+v", pending)
	}
	next := receiveExecution(t, executor)
	if len(next.SubTasks) != 1 || next.SubTasks[0].ID != subTasks[1].ID {
		t.Fatalf("应只启动依赖已满足的下一 SubTask: %+v", next.SubTasks)
	}
	executor.release <- struct{}{}
}

func TestRobotWorkflowWaitsForSkillResultAfterLastPhotoAndSummary(t *testing.T) {
	st, project, conversation, service, _, executor := newWorkflowFixture(t)
	view, task, subTasks := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{
			{ID: "sub-nav", Kind: "robot_skill", Goal: "导航"},
			{ID: "sub-grasp-next", Kind: "robot_skill", Goal: "抓取", DependsOn: []string{"sub-nav"}},
		})
	robotService := robotdomain.NewService(st, nil)
	robotService.SetExecutionObserver(service)
	_, disconnect, err := robotService.Connect(store.RobotPilot{
		PilotInstanceID: "pilot-photo", RobotID: "r1pro-test", RobotStatus: "busy",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	now := time.Now().UTC()
	execution := store.RobotExecution{
		ID: "rex-nav-photo", ProjectID: project.ID, WorkflowID: view.Workflow.ID,
		TaskID: task.ID, SubtaskID: subTasks[0].ID, PilotInstanceID: "pilot-photo",
		RobotID: "r1pro-test", SkillName: "semantic-navigation", SkillVersion: "0.4.7",
		RequestKey: "nav-photo", Status: "running", Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	// Real candidate03 sequence: final capture -> summary with completed snapshot -> terminal result.
	if err := robotService.HandlePilotEvent("pilot-photo", "action.terminal", 47, map[string]any{
		"execution_id": execution.ID, "skill_status": "running", "status": "succeeded",
		"result": map[string]any{"artifact_candidates": []any{"last-photo"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := robotService.HandlePilotEvent("pilot-photo", "artifact.summary.announced", 50, map[string]any{
		"execution_id": execution.ID, "skill_status": "completed", "local_ref": "pilot-artifact://pilot-photo/summary",
	}); err != nil {
		t.Fatal(err)
	}
	current, _ := st.GetSubTask(subTasks[0].ID)
	pending, _ := st.GetSubTask(subTasks[1].ID)
	if current.Status != store.TaskStatusRunning || pending.Status != store.TaskStatusPending || string(current.Result) != "{}" {
		t.Fatalf("summary prematurely completed or polluted SubTask: current=%+v pending=%+v", current, pending)
	}
	select {
	case next := <-executor.started:
		t.Fatalf("next physical step started before Skill terminal: %+v", next)
	default:
	}
	finalResult := map[string]any{"reached": true, "distance_to_target_m": 0.003, "target_ref": "source-1"}
	if err := robotService.HandlePilotEvent("pilot-photo", "execution.terminal", 51, map[string]any{
		"execution_id": execution.ID, "skill_status": "completed", "status": "completed", "result": finalResult,
	}); err != nil {
		t.Fatal(err)
	}
	current, _ = st.GetSubTask(subTasks[0].ID)
	var result struct {
		Result          map[string]any `json:"result"`
		TerminalPayload map[string]any `json:"terminal_payload"`
	}
	if err := json.Unmarshal(current.Result, &result); err != nil {
		t.Fatal(err)
	}
	if current.Status != store.TaskStatusCompleted || result.Result["reached"] != true ||
		result.TerminalPayload["target_ref"] != "source-1" || result.Result["artifact_candidates"] != nil {
		t.Fatalf("SubTask did not retain exact Skill result: %s", current.Result)
	}
	next := receiveExecution(t, executor)
	if len(next.SubTasks) != 1 || next.SubTasks[0].ID != subTasks[1].ID {
		t.Fatalf("wrong next SubTask after formal completion: %+v", next)
	}
	executor.release <- struct{}{}
}

func TestWorkflowIgnoresNonLifecycleTerminalSnapshots(t *testing.T) {
	for _, status := range []string{"completed", "failed", "stopped", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			st, project, conversation, service, _, _ := newWorkflowFixture(t)
			view, task, subTasks := prepareRunningRobotTask(t, st, project, conversation,
				[]store.SubTaskDraft{{ID: "sub-snapshot", Kind: "robot_skill", Goal: "执行"}})
			execution := store.RobotExecution{
				ID: "rex-snapshot", ProjectID: project.ID, WorkflowID: view.Workflow.ID,
				TaskID: task.ID, SubtaskID: subTasks[0].ID, Status: status,
				Result: map[string]any{"artifact_candidates": []any{"photo"}},
			}
			for _, eventType := range []string{"action.terminal", "stage.completed", "artifact.summary.announced", "artifact.summary.failed"} {
				if err := service.OnRobotExecutionChanged(context.Background(), execution, eventType, execution.Result); err != nil {
					t.Fatal(err)
				}
			}
			current, _ := st.GetSubTask(subTasks[0].ID)
			currentTask, _ := st.GetTask(task.ID)
			if current.Status != store.TaskStatusRunning || currentTask.Status != store.TaskStatusRunning || string(current.Result) != "{}" {
				t.Fatalf("non-lifecycle snapshot changed Workflow: subtask=%+v task=%+v", current, currentTask)
			}
		})
	}
}

func TestRobotExecutionInterruptedPausesTaskWithoutReplay(t *testing.T) {
	st, project, conversation, service, _, executor := newWorkflowFixture(t)
	view, task, subTasks := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "sub-nav", Kind: "robot_skill", Goal: "导航"}})
	execution := store.RobotExecution{ID: "rex-nav", ProjectID: project.ID,
		WorkflowID: view.Workflow.ID, TaskID: task.ID, SubtaskID: subTasks[0].ID,
		RobotID: "r1pro-test", SkillName: "semantic-navigation", SkillVersion: "0.1.0",
		Status: "interrupted", Error: map[string]any{"code": "STATE_UNKNOWN"}}
	if err := service.OnRobotExecutionChanged(context.Background(), execution,
		"robot.execution.interrupted", execution.Error); err != nil {
		t.Fatal(err)
	}
	pausedTask, _ := st.GetTask(task.ID)
	pausedSubTask, _ := st.GetSubTask(subTasks[0].ID)
	if pausedTask.Status != store.TaskStatusPaused ||
		pausedSubTask.Status != store.TaskStatusPaused ||
		pausedSubTask.WaitingReason != "execution_state_unknown" {
		t.Fatalf("interrupted 必须暂停且保留未知原因: task=%+v subtask=%+v",
			pausedTask, pausedSubTask)
	}
	var result map[string]any
	if err := json.Unmarshal(pausedSubTask.Result, &result); err != nil ||
		result["robot_execution_id"] != execution.ID {
		t.Fatalf("中断证据未保存: result=%s err=%v", pausedSubTask.Result, err)
	}
	select {
	case replay := <-executor.started:
		t.Fatalf("interrupted 不得自动重放 Robot SubTask: %+v", replay)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRobotExecutionFailedAndStoppedPreserveTerminalReason(t *testing.T) {
	for _, test := range []struct {
		name, status, reason string
	}{
		{name: "failed", status: "failed", reason: "robot_execution_failed"},
		{name: "stopped", status: "stopped", reason: "robot_execution_stopped"},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, project, conversation, service, _, executor := newWorkflowFixture(t)
			view, task, subTasks := prepareRunningRobotTask(t, st, project, conversation,
				[]store.SubTaskDraft{{ID: "sub-terminal", Kind: "robot_skill", Goal: "执行动作"}})
			execution := store.RobotExecution{ID: "rex-" + test.status,
				ProjectID: project.ID, WorkflowID: view.Workflow.ID, TaskID: task.ID,
				SubtaskID: subTasks[0].ID, RobotID: "r1pro-test",
				SkillName: "grasp-object", SkillVersion: "0.1.0", Status: test.status,
				Error: map[string]any{"code": "TEST_TERMINAL"}}
			if err := service.OnRobotExecutionChanged(context.Background(), execution,
				"execution."+test.status, execution.Error); err != nil {
				t.Fatal(err)
			}
			pausedTask, _ := st.GetTask(task.ID)
			pausedSubTask, _ := st.GetSubTask(subTasks[0].ID)
			if pausedTask.Status != store.TaskStatusPaused ||
				pausedSubTask.Status != store.TaskStatusPaused ||
				pausedSubTask.WaitingReason != test.reason {
				t.Fatalf("终态必须保留等待原因: task=%+v subtask=%+v",
					pausedTask, pausedSubTask)
			}
			select {
			case replay := <-executor.started:
				t.Fatalf("物理终态不得自动重放: %+v", replay)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

func TestFailedPhysicalExecutionWaitsForReconciliationWithoutRecovery(t *testing.T) {
	st, project, conversation, service, _, executor := newWorkflowFixture(t)
	view, task, subTasks := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "sub-grasp", Kind: "robot_skill", Goal: "抓取"}})
	execution := store.RobotExecution{ID: "rex-physical-failed", ProjectID: project.ID,
		WorkflowID: view.Workflow.ID, TaskID: task.ID, SubtaskID: subTasks[0].ID,
		RobotID: "r1pro-test", SkillName: "grasp-object", SkillVersion: "0.4.16",
		Status: "failed", Error: map[string]any{"code": "PLANNING_FAILED"}}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendRobotExecutionEvent(store.RobotExecutionEvent{
		ExecutionID: execution.ID, Sequence: 1, Type: "action.started",
		Payload:   map[string]any{"action_type": "motion.move_end_effector", "physical_started": true},
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.OnRobotExecutionChanged(context.Background(), execution,
		"execution.failed", execution.Error); err != nil {
		t.Fatal(err)
	}
	pausedTask, _ := st.GetTask(task.ID)
	pausedSubTask, _ := st.GetSubTask(subTasks[0].ID)
	if pausedTask.Status != store.TaskStatusPaused ||
		pausedSubTask.WaitingReason != "execution_state_unknown" {
		t.Fatalf("物理动作后的失败必须保留Robot锁等待对账: task=%+v subtask=%+v",
			pausedTask, pausedSubTask)
	}
	select {
	case replay := <-executor.started:
		t.Fatalf("状态未知时不得自动重放完整 Robot Skill: %+v", replay)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestFailedPhysicalExecutionWithHoldEvidenceStopsWorkflow(t *testing.T) {
	st, project, conversation, service, _, executor := newWorkflowFixture(t)
	view, task, subTasks := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "sub-held", Kind: "robot_skill", Goal: "抓取"}})
	now := time.Now().UTC()
	// 与策略超时现场一致：Skill 失败结束，但 Ability 撤销执行后已核对 hold，
	// 把 safe 证据写进了该次 Action 的 result。
	execution := store.RobotExecution{ID: "rex-failed-held", ProjectID: project.ID,
		WorkflowID: view.Workflow.ID, TaskID: task.ID, SubtaskID: subTasks[0].ID,
		RobotID: "r1pro-test", SkillName: "vla-manipulation", SkillVersion: "0.1.7",
		Status: "failed", Error: map[string]any{"code": "policy_timeout"},
		Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	for sequence, event := range []struct {
		kind    string
		payload map[string]any
	}{
		{"action.started", map[string]any{"action_id": "policy", "physical": true, "physical_started": true}},
		{"action.terminal", map[string]any{"action_id": "policy", "status": "failed",
			"result": map[string]any{"safe": true, "physical_state": "hold", "hold_confirmed": true},
			"error":  map[string]any{"code": "policy_timeout"}}},
	} {
		if err := st.AppendRobotExecutionEvent(store.RobotExecutionEvent{
			ExecutionID: execution.ID, Sequence: int64(sequence + 1),
			Type: event.kind, Payload: event.payload, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.OnRobotExecutionChanged(context.Background(), execution,
		"execution.failed", execution.Error); err != nil {
		t.Fatal(err)
	}
	stoppedWorkflow, _ := st.GetWorkflow(view.Workflow.ID)
	stoppedTask, _ := st.GetTask(task.ID)
	stoppedSubTask, _ := st.GetSubTask(subTasks[0].ID)
	if stoppedWorkflow.Status != store.WorkflowStatusStopped ||
		stoppedTask.Status != store.TaskStatusStopped ||
		stoppedSubTask.Status != store.TaskStatusStopped {
		t.Fatalf("失败但已安全保持必须直接收敛 stopped: workflow=%+v task=%+v subtask=%+v",
			stoppedWorkflow, stoppedTask, stoppedSubTask)
	}
	if stoppedSubTask.WaitingReason != "robot_execution_failed_but_held" {
		t.Fatalf("收敛原因必须说明失败但已保持: %+v", stoppedSubTask)
	}
	select {
	case replay := <-executor.started:
		t.Fatalf("失败但已安全保持不得自动重放: %+v", replay)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRobotExecutionSafelyHeldTracksLastPhysicalFact(t *testing.T) {
	st, _, _, service, _, _ := newWorkflowFixture(t)
	now := time.Now().UTC()
	appendEvent := func(sequence int64, kind string, payload map[string]any) {
		t.Helper()
		if err := st.AppendRobotExecutionEvent(store.RobotExecutionEvent{
			ExecutionID: "rex-scan", Sequence: sequence, Type: kind,
			Payload: payload, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	held, err := service.robotExecutionSafelyHeld("rex-scan")
	if err != nil || held {
		t.Fatalf("无 Action 事实时不得声称已保持: held=%v err=%v", held, err)
	}
	appendEvent(1, "action.started", map[string]any{"action_id": "a", "physical_started": true})
	appendEvent(2, "action.terminal", map[string]any{"action_id": "a", "status": "failed",
		"result": map[string]any{"safe": true, "physical_state": "hold"}})
	if held, err = service.robotExecutionSafelyHeld("rex-scan"); err != nil || !held {
		t.Fatalf("Ability 的保持证据必须可读: held=%v err=%v", held, err)
	}
	// 保持证明只对最后一个物理事实有效：新的物理动作会推翻它。
	appendEvent(3, "action.started", map[string]any{"action_id": "b", "physical_started": true})
	appendEvent(4, "action.terminal", map[string]any{"action_id": "b", "status": "succeeded"})
	if held, err = service.robotExecutionSafelyHeld("rex-scan"); err != nil || held {
		t.Fatalf("新的物理动作必须推翻此前保持证明: held=%v err=%v", held, err)
	}
	appendEvent(5, "action.terminal", map[string]any{"action_id": "c", "status": "stopped",
		"result": map[string]any{"safe": true}})
	if held, _ = service.robotExecutionSafelyHeld("rex-scan"); !held {
		t.Fatal("其后的安全保持证明必须重新成立")
	}
	appendEvent(6, "action.terminal", map[string]any{"action_id": "c", "status": "stopped",
		"result": map[string]any{"safe": false}})
	if held, _ = service.robotExecutionSafelyHeld("rex-scan"); held {
		t.Fatal("显式 safe=false 必须撤销保持证明")
	}
}

func TestStopWorkflowSettlesFailedHeldExecutionWithoutConfirmation(t *testing.T) {
	st, project, conversation, service, _, _ := newWorkflowFixture(t)
	view, task, subTasks := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "sub-held-stop", Kind: "robot_skill", Goal: "抓取"}})
	now := time.Now().UTC()
	execution := store.RobotExecution{ID: "rex-held-stop", ProjectID: project.ID,
		WorkflowID: view.Workflow.ID, TaskID: task.ID, SubtaskID: subTasks[0].ID,
		RobotID: task.AssignedRobotID, SkillName: "vla-manipulation", SkillVersion: "0.1.7",
		Status: "failed", Error: map[string]any{"code": "policy_timeout"},
		Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AttachSubTaskExecution(subTasks[0].ID, subTasks[0].Revision,
		execution.ID, now); err != nil {
		t.Fatal(err)
	}
	for sequence, event := range []struct {
		kind    string
		payload map[string]any
	}{
		{"action.started", map[string]any{"action_id": "policy", "physical": true, "physical_started": true}},
		{"action.terminal", map[string]any{"action_id": "policy", "status": "failed",
			"result": map[string]any{"safe": true, "physical_state": "hold", "hold_confirmed": true}}},
	} {
		if err := st.AppendRobotExecutionEvent(store.RobotExecutionEvent{
			ExecutionID: execution.ID, Sequence: int64(sequence + 1),
			Type: event.kind, Payload: event.payload, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	current, _ := st.GetWorkflow(view.Workflow.ID)
	result, err := service.StopWorkflow(context.Background(), project.OwnerID, project.ID,
		view.Workflow.ID, current.Revision)
	if err != nil {
		t.Fatalf("已安全保持的失败执行不应要求人工确认: %v", err)
	}
	if result.Workflow.Status != store.WorkflowStatusStopped {
		t.Fatalf("停止必须直接收敛: %+v", result.Workflow)
	}
	stoppedSubTask, _ := st.GetSubTask(subTasks[0].ID)
	if stoppedSubTask.Status != store.TaskStatusStopped ||
		stoppedSubTask.WaitingReason != "robot_execution_failed_but_held" {
		t.Fatalf("收敛原因必须是 failed_but_held: %+v", stoppedSubTask)
	}
}

type recoveringExecutor struct {
	*controlledExecutor
	requests chan TaskRecoveryRequest
	decision TaskRecoveryDecision
}

type waitingRecoveryExecutor struct {
	*controlledExecutor
	requests chan TaskRecoveryRequest
}

func (e *waitingRecoveryExecutor) RecoverTask(_ context.Context,
	request TaskRecoveryRequest) (TaskRecoveryDecision, error) {
	e.requests <- request
	if request.Answer == nil {
		return TaskRecoveryDecision{}, ErrTaskWaitingInput
	}
	return TaskRecoveryDecision{Decision: "fail_task", Summary: "用户确认后结束任务"}, nil
}

func (e *recoveringExecutor) RecoverTask(_ context.Context,
	request TaskRecoveryRequest) (TaskRecoveryDecision, error) {
	e.requests <- request
	return e.decision, nil
}

func TestFailedRobotExecutionUsesOneRecoveryRunWithoutReplayingOldAction(t *testing.T) {
	st := openWorkflowStore(t)
	project, conversation := createWorkflowProject(t, st)
	planner := &fakePlanner{}
	executor := &recoveringExecutor{
		controlledExecutor: &controlledExecutor{started: make(chan TaskExecution, 2),
			release: make(chan struct{}, 2)},
		requests: make(chan TaskRecoveryRequest, 1),
		decision: TaskRecoveryDecision{Decision: "revise_pending", Replacements: []store.SubTaskDraft{{
			ID: "sub-grasp-retry", Kind: "robot_skill", Goal: "刷新观测后抓取",
			Spec: json.RawMessage(`{"skill_name":"grasp-object","skill_version":"0.1.0","input":{"refresh":true}}`),
		}}},
	}
	service, err := NewService(Deps{Store: st, Planner: planner, Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	view, task, subTasks := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{
			{ID: "sub-grasp", Kind: "robot_skill", Goal: "首次抓取"},
			{ID: "sub-nav", Kind: "robot_skill", Goal: "后续导航", DependsOn: []string{"sub-grasp"}},
		})
	execution := store.RobotExecution{ID: "rex-failed", ProjectID: project.ID,
		WorkflowID: view.Workflow.ID, TaskID: task.ID, SubtaskID: subTasks[0].ID,
		RobotID: "r1pro-test", SkillName: "grasp-object", SkillVersion: "0.1.0",
		Status: "failed", Revision: 2, UpdatedAt: time.Now().UTC(),
		Error: map[string]any{"code": "NO_CONTACT"}}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	if err := service.OnRobotExecutionChanged(context.Background(), execution,
		"execution.failed", execution.Error); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-executor.requests:
		if request.SubTask.ID != subTasks[0].ID || request.Execution.ID != execution.ID {
			t.Fatalf("恢复 Run 没有绑定失败事实: %+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("明确 failed 后未启动 Task recovery Run")
	}
	next := receiveExecution(t, executor.controlledExecutor)
	if len(next.SubTasks) != 1 || next.SubTasks[0].ID != "sub-grasp-retry" {
		t.Fatalf("恢复后必须执行替代步骤，不能重放旧 Action: %+v", next.SubTasks)
	}
	old, _ := st.GetSubTask("sub-grasp")
	cancelled, _ := st.GetSubTask("sub-nav")
	if old.Status != store.TaskStatusFailed || old.WaitingReason != "replaced_after_failure" ||
		cancelled.Status != store.TaskStatusStopped || cancelled.WaitingReason != "revised_after_failure" {
		t.Fatalf("旧步骤审计状态错误: old=%+v cancelled=%+v", old, cancelled)
	}
	executor.release <- struct{}{}
}

func TestRecoveryInteractionReturnsToFailedExecutionRecovery(t *testing.T) {
	st := openWorkflowStore(t)
	project, conversation := createWorkflowProject(t, st)
	executor := &waitingRecoveryExecutor{
		controlledExecutor: &controlledExecutor{started: make(chan TaskExecution, 1),
			release: make(chan struct{}, 1)},
		requests: make(chan TaskRecoveryRequest, 2),
	}
	service, err := NewService(Deps{Store: st, Planner: &fakePlanner{}, Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	view, task, subTasks := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "sub-recovery-question", Kind: "robot_skill", Goal: "抓取"}})
	execution := store.RobotExecution{ID: "rex-recovery-question", ProjectID: project.ID,
		WorkflowID: view.Workflow.ID, TaskID: task.ID, SubtaskID: subTasks[0].ID,
		RobotID: "r1pro-test", SkillName: "grasp-object", SkillVersion: "0.4.0",
		Status: "failed", Error: map[string]any{"code": "TARGET_AMBIGUOUS"}}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	if err := service.OnRobotExecutionChanged(context.Background(), execution,
		"execution.failed", execution.Error); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-executor.requests:
		if request.Answer != nil || request.Task.WaitingReason != "recovery_running" {
			t.Fatalf("首次Recovery应持有明确责任方且没有伪造回答: %+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("失败Execution没有启动Recovery")
	}
	deadline := time.Now().Add(time.Second)
	for {
		service.mu.Lock()
		_, busy := service.activeTasks[task.ID]
		service.mu.Unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("首次Recovery Run没有释放Task写权")
		}
		time.Sleep(time.Millisecond)
	}
	currentTask, _ := st.GetTask(task.ID)
	paused, err := service.PauseTaskForInteraction(project.OwnerID, project.ID,
		view.Workflow.ID, task.ID, currentTask.Revision)
	if err != nil || paused.WaitingReason != "waiting_input" {
		t.Fatalf("Recovery问题没有形成waiting_input边界: task=%+v err=%v", paused, err)
	}
	currentSubTask, _ := st.GetSubTask(subTasks[0].ID)
	run := store.RunSession{ID: "run-recovery-question", ProjectID: project.ID,
		ChatSessionID: conversation.ID, ContextID: task.ContextID,
		AgentID: task.AssignedAgentID, AgentName: task.AssignedAgentID,
		WorkflowID: view.Workflow.ID, TaskID: task.ID, Kind: store.RunKindTaskExecution,
		Status: store.RunStatusWaitingInput, StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := st.CreateRunSession(run); err != nil {
		t.Fatal(err)
	}
	answeredAt := time.Now().UTC()
	answer := store.Interaction{ID: "int-recovery-question", ProjectID: project.ID,
		SessionID: conversation.ID, WorkflowID: view.Workflow.ID, TaskID: task.ID,
		RunID: run.ID, Agent: task.AssignedAgentID, Type: store.InteractionTypeInput,
		UIKind: store.InteractionUIForm, Status: store.InteractionStatusAnswered,
		Reply: `{"strategy":"abort"}`, SourceRevision: paused.Revision,
		CreatedAt: answeredAt, AnsweredAt: &answeredAt}
	if err := st.CreateInteraction(answer); err != nil {
		t.Fatal(err)
	}
	if err := service.RouteInteractionAnswer(context.Background(), answer); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-executor.requests:
		if request.Answer == nil || request.Answer.ID != answer.ID ||
			request.SubTask.ID != currentSubTask.ID || request.Execution.ID != execution.ID {
			t.Fatalf("Interaction没有返回原Recovery路径: %+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("回答后没有恢复原Recovery")
	}
}

func TestWorkspaceWriteReservationOutlivesOneAgentRun(t *testing.T) {
	service, err := NewService(Deps{Store: openWorkflowStore(t)})
	if err != nil {
		t.Fatal(err)
	}
	workflowValue := store.Workflow{ProjectID: "project-a"}
	requirements := json.RawMessage(`{"workspace_write":true}`)
	first := store.Task{ID: "task-a", ResourceRequirements: requirements}
	second := store.Task{ID: "task-b", ResourceRequirements: requirements}
	if !service.claimTaskResources(workflowValue, first) {
		t.Fatal("首个 Task 应取得工作区写锁")
	}
	service.reserveTask(first)
	service.releaseTask(first)
	if service.claimTaskResources(workflowValue, second) {
		t.Fatal("单次 Agent Run 结束不得释放 Task 的工作区写锁")
	}
	service.releaseTaskResources(first.ID)
	if !service.claimTaskResources(workflowValue, second) {
		t.Fatal("Task 终态后应释放工作区写锁")
	}
}

func TestDirectRobotStopWithSafetyEvidenceStopsWorkflow(t *testing.T) {
	st, project, conversation, service, _, _ := newWorkflowFixture(t)
	view, task, subTasks := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "sub-direct-stop", Kind: "robot_skill", Goal: "导航"}})
	execution := store.RobotExecution{ID: "rex-direct-stop", ProjectID: project.ID,
		WorkflowID: view.Workflow.ID, TaskID: task.ID, SubtaskID: subTasks[0].ID,
		RobotID: "r1pro-test", SkillName: "semantic-navigation", SkillVersion: "0.1.0",
		Status: "stopped", Result: map[string]any{"safe": true, "physical_state": "hold"}}
	if err := service.OnRobotExecutionChanged(context.Background(), execution,
		"skill.stop.finalized", execution.Result); err != nil {
		t.Fatal(err)
	}
	stoppedWorkflow, _ := st.GetWorkflow(view.Workflow.ID)
	stoppedTask, _ := st.GetTask(task.ID)
	stoppedSubTask, _ := st.GetSubTask(subTasks[0].ID)
	if stoppedWorkflow.Status != store.WorkflowStatusStopped ||
		stoppedTask.Status != store.TaskStatusStopped ||
		stoppedSubTask.Status != store.TaskStatusStopped {
		t.Fatalf("有安全证据的 robot.stop 必须收敛整个 Workflow: workflow=%+v task=%+v subtask=%+v",
			stoppedWorkflow, stoppedTask, stoppedSubTask)
	}
}

func TestSafeRobotStopConvergesWorkflowPausedDuringReconciliation(t *testing.T) {
	st, project, conversation, service, _, _ := newWorkflowFixture(t)
	view, task, subTasks := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "sub-reconciled-stop", Kind: "robot_skill", Goal: "携物导航"}})

	// Pilot 暂时失联时，执行先进入 interrupted，Task/SubTask 暂停并保留 Robot；
	// Server 重启恢复随后把所属 Workflow 暂停，等待按 execution ID 对账。
	interrupted := store.RobotExecution{ID: "rex-reconciled-stop", ProjectID: project.ID,
		WorkflowID: view.Workflow.ID, TaskID: task.ID, SubtaskID: subTasks[0].ID,
		RobotID: "r1pro-test", SkillName: "semantic-navigation", SkillVersion: "0.3.0",
		Status: "interrupted", Error: map[string]any{"code": "STATE_UNKNOWN"}}
	if err := service.OnRobotExecutionChanged(context.Background(), interrupted,
		"robot.execution.interrupted", interrupted.Error); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetWorkflow(view.Workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.TransitionWorkflowWithReason(current.ID, current.Revision,
		store.WorkflowStatusPaused, "execution_state_unknown", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	paused, _ := st.GetWorkflow(view.Workflow.ID)
	if paused.Status != store.WorkflowStatusPaused {
		t.Fatalf("对账前 Workflow 应处于 paused: %+v", paused)
	}

	// 重连后 Pilot 确认 Robot 已 hold。真实安全证据必须把此前 paused 的
	// Workflow 一并收敛到 stopped，而不是只停止 Task 后遗留活动 Workflow。
	interrupted.Status = "stopped"
	interrupted.Error = nil
	interrupted.Result = map[string]any{"safe": true, "hold_confirmed": true}
	if err := service.OnRobotExecutionChanged(context.Background(), interrupted,
		"skill.stop.finalized", interrupted.Result); err != nil {
		t.Fatal(err)
	}
	stoppedWorkflow, _ := st.GetWorkflow(view.Workflow.ID)
	stoppedTask, _ := st.GetTask(task.ID)
	stoppedSubTask, _ := st.GetSubTask(subTasks[0].ID)
	if stoppedWorkflow.Status != store.WorkflowStatusStopped ||
		stoppedTask.Status != store.TaskStatusStopped ||
		stoppedSubTask.Status != store.TaskStatusStopped {
		t.Fatalf("安全停止没有收敛 paused Workflow: workflow=%+v task=%+v subtask=%+v",
			stoppedWorkflow, stoppedTask, stoppedSubTask)
	}
}

type recordingRobotStopper struct {
	requests chan string
}

func (s *recordingRobotStopper) Stop(_ context.Context, _, executionID, _ string) (store.RobotExecution, error) {
	s.requests <- executionID
	return store.RobotExecution{ID: executionID, Status: "stopping"}, nil
}

func (s *recordingRobotStopper) ConfirmOperatorStop(
	_ context.Context, expected store.RobotExecution, _, _ string,
) (store.RobotExecution, error) {
	expected.Status = "stopped"
	expected.Result = map[string]any{"safe": true, "hold_confirmed": true}
	return expected, nil
}

func TestStopWorkflowWaitsForRobotStopEvidence(t *testing.T) {
	st, project, conversation, service, _, _ := newWorkflowFixture(t)
	stopper := &recordingRobotStopper{requests: make(chan string, 1)}
	service.robotStop = stopper
	view, task, subTasks := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "sub-stop", Kind: "robot_skill", Goal: "安全停止导航"}})
	execution := store.RobotExecution{ID: "rex-stop", ProjectID: project.ID,
		WorkflowID: view.Workflow.ID, TaskID: task.ID, SubtaskID: subTasks[0].ID,
		RobotID: "r1pro-test", SkillName: "semantic-navigation", SkillVersion: "0.1.0",
		RequestKey: "stop-workflow", Status: "running", Revision: 1,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AttachSubTaskExecution(subTasks[0].ID, subTasks[0].Revision,
		execution.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	currentWorkflow, _ := st.GetWorkflow(view.Workflow.ID)
	if _, err := service.StopWorkflow(context.Background(), project.OwnerID, project.ID,
		view.Workflow.ID, currentWorkflow.Revision); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-stopper.requests:
		if got != execution.ID {
			t.Fatalf("停止请求没有定位精确 Robot Execution: %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Workflow stop 没有下发 Robot 安全停止")
	}
	stoppingWorkflow, _ := st.GetWorkflow(view.Workflow.ID)
	stoppingTask, _ := st.GetTask(task.ID)
	stoppingSubTask, _ := st.GetSubTask(subTasks[0].ID)
	if stoppingWorkflow.Status != store.WorkflowStatusStopping ||
		stoppingTask.Status != store.TaskStatusStopping ||
		stoppingSubTask.Status != store.TaskStatusStopping {
		t.Fatalf("Pilot 证据前不得伪造 stopped: workflow=%+v task=%+v subtask=%+v",
			stoppingWorkflow, stoppingTask, stoppingSubTask)
	}
	execution.Status = "stopped"
	execution.Result = map[string]any{"safe": true, "hold_confirmed": true}
	if err := service.OnRobotExecutionChanged(context.Background(), execution,
		"execution.stopped", execution.Result); err != nil {
		t.Fatal(err)
	}
	stoppedWorkflow, _ := st.GetWorkflow(view.Workflow.ID)
	stoppedTask, _ := st.GetTask(task.ID)
	stoppedSubTask, _ := st.GetSubTask(subTasks[0].ID)
	if stoppedWorkflow.Status != store.WorkflowStatusStopped ||
		stoppedTask.Status != store.TaskStatusStopped ||
		stoppedSubTask.Status != store.TaskStatusStopped {
		t.Fatalf("停止证据没有收敛 Workflow: workflow=%+v task=%+v subtask=%+v",
			stoppedWorkflow, stoppedTask, stoppedSubTask)
	}
}

func TestPlainStopRequiresOperatorConfirmationForUnknownPhysicalState(t *testing.T) {
	st, project, conversation, service, _, _ := newWorkflowFixture(t)
	robotService := robotdomain.NewService(st, nil)
	robotService.SetExecutionObserver(service)
	service.robotStop = robotService
	view, task, subTasks := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "sub-physical-unknown", Kind: "robot_skill", Goal: "抓取"}})
	now := time.Now().UTC()
	// 与 libero/mujoco 现场一致：Skill 已经失败，但事件证明它确实做过物理动作，
	// 因此服务端无法证明机器人已安全停止。
	execution := store.RobotExecution{ID: "rex-physical-unknown", ProjectID: project.ID,
		WorkflowID: view.Workflow.ID, TaskID: task.ID, SubtaskID: subTasks[0].ID,
		RobotID: task.AssignedRobotID, SkillName: "vla-manipulation", SkillVersion: "0.1.7",
		Status: "failed", Error: map[string]any{"code": "OBJECTIVE_NOT_MET"},
		Revision: 2, CreatedAt: now, UpdatedAt: now}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AttachSubTaskExecution(
		subTasks[0].ID, subTasks[0].Revision, execution.ID, now,
	); err != nil {
		t.Fatal(err)
	}
	for sequence, event := range []struct {
		kind    string
		payload map[string]any
	}{
		{"action.started", map[string]any{"action_id": "pick", "physical": true, "physical_started": true}},
		{"action.terminal", map[string]any{"action_id": "pick", "status": "interrupted"}},
	} {
		if err := st.AppendRobotExecutionEvent(store.RobotExecutionEvent{
			ExecutionID: execution.ID, Sequence: int64(sequence + 1),
			Type: event.kind, Payload: event.payload, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	current, _ := st.GetWorkflow(view.Workflow.ID)
	_, err := service.StopWorkflow(context.Background(), project.OwnerID, project.ID,
		view.Workflow.ID, current.Revision)
	if !errors.Is(err, store.ErrOperatorConfirmationRequired) {
		t.Fatalf("普通 stop 不得对物理状态未知假成功: %v", err)
	}
	// 第一次 stop 已经把视图推进到 paused/execution_state_unknown，重试仍然
	// 必须是"需要人工确认"，而不是反复回退却返回成功（用户看到的老毛病）。
	stuck, err := st.GetWorkflow(view.Workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stuck.Status != store.WorkflowStatusPaused || stuck.Reason != "execution_state_unknown" {
		t.Fatalf("未知状态必须保留给人工确认: %+v", stuck)
	}
	if _, err := service.StopWorkflow(context.Background(), project.OwnerID, project.ID,
		view.Workflow.ID, stuck.Revision); !errors.Is(err, store.ErrOperatorConfirmationRequired) {
		t.Fatalf("重复普通 stop 仍须要求人工确认: %v", err)
	}
	// 每次普通 stop 都会推进 revision（stopping 再被安全策略退回 paused），
	// 人工确认必须使用用户当下看到的最新现场 revision。
	retried, err := st.GetWorkflow(view.Workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 唯一出口：人工安全确认，随后 Workflow 必须真正终结并释放 Robot。
	confirmed, err := service.ConfirmWorkflowStop(context.Background(), project.OwnerID,
		project.ID, view.Workflow.ID, retried.Revision, true, "现场已确认机器人处于安全保持")
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Workflow.Status != store.WorkflowStatusStopped {
		t.Fatalf("人工确认必须终结 Workflow: %+v", confirmed.Workflow)
	}
	reserved, err := st.IsRobotTaskReserved(task.AssignedRobotID, "")
	if err != nil || reserved {
		t.Fatalf("终结后必须释放 Robot: reserved=%v err=%v", reserved, err)
	}
}

func TestConfirmWorkflowStopConvergesUnknownRobotExecution(t *testing.T) {
	st, project, conversation, service, _, _ := newWorkflowFixture(t)
	robotService := robotdomain.NewService(st, nil)
	robotService.SetExecutionObserver(service)
	service.robotStop = robotService
	view, task, subTasks := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "sub-confirm-stop", Kind: "robot_skill", Goal: "抓取"}})
	now := time.Now().UTC()
	execution := store.RobotExecution{
		ID: "rex-confirm-stop", ProjectID: project.ID, WorkflowID: view.Workflow.ID,
		TaskID: task.ID, SubtaskID: subTasks[0].ID, RobotID: task.AssignedRobotID,
		PilotInstanceID: "pilot-confirm-stop", SkillName: "grasp-object", SkillVersion: "0.5.0",
		RequestKey: "confirm-stop", Status: "interrupted", Revision: 2,
		Error:     map[string]any{"code": "PILOT_OFFLINE", "message": "连接断开"},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRobotPilot(store.RobotPilot{
		PilotInstanceID: execution.PilotInstanceID, RobotID: execution.RobotID,
		Status: "offline", RobotStatus: "interrupted", CurrentExecutionID: execution.ID,
		Revision: 2, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AttachSubTaskExecution(
		subTasks[0].ID, subTasks[0].Revision, execution.ID, now,
	); err != nil {
		t.Fatal(err)
	}

	current, _ := st.GetWorkflow(view.Workflow.ID)
	if _, err := service.ConfirmWorkflowStop(
		context.Background(), project.OwnerID, project.ID, view.Workflow.ID,
		current.Revision, true, "不能跳过正常停止",
	); !errors.Is(err, store.ErrInvalidState) {
		t.Fatalf("运行中的 Workflow 不得直接人工终结: %v", err)
	}
	paused, err := service.StopWorkflow(
		context.Background(), project.OwnerID, project.ID, view.Workflow.ID, current.Revision,
	)
	if !errors.Is(err, store.ErrOperatorConfirmationRequired) {
		t.Fatalf("离线停止必须明确要求人工确认: %v", err)
	}
	if paused.Workflow.Status != store.WorkflowStatusPaused ||
		paused.Workflow.Reason != "execution_state_unknown" {
		t.Fatalf("离线停止必须收敛为状态未知: %+v", paused.Workflow)
	}
	if _, err := service.ConfirmWorkflowStop(
		context.Background(), project.OwnerID, project.ID, view.Workflow.ID,
		paused.Workflow.Revision, false, "已检查",
	); !errors.Is(err, store.ErrInvalidState) {
		t.Fatalf("缺少物理确认必须拒绝: %v", err)
	}
	if _, err := service.ConfirmWorkflowStop(
		context.Background(), project.OwnerID, project.ID, view.Workflow.ID,
		paused.Workflow.Revision-1, true, "现场已检查",
	); !errors.Is(err, store.ErrRevisionConflict) {
		t.Fatalf("旧 revision 的人工确认必须拒绝: %v", err)
	}
	confirmed, err := service.ConfirmWorkflowStop(
		context.Background(), project.OwnerID, project.ID, view.Workflow.ID,
		paused.Workflow.Revision, true, "现场急停已释放，机器人处于安全保持状态",
	)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Workflow.Status != store.WorkflowStatusStopped ||
		confirmed.Tasks[0].Status != store.TaskStatusStopped ||
		confirmed.SubTasks[0].Status != store.TaskStatusStopped {
		t.Fatalf("人工确认没有统一终结 Workflow: %+v", confirmed)
	}
	stoppedExecution, err := st.GetRobotExecution(execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stoppedExecution.Status != "stopped" || stoppedExecution.Result["safe"] != true ||
		stoppedExecution.Result["hold_confirmed"] != true ||
		stoppedExecution.Result["confirmation_source"] != "operator" ||
		stoppedExecution.Result["confirmed_by"] != project.OwnerID ||
		stoppedExecution.Result["pre_stop_error"] == nil {
		t.Fatalf("人工安全确认审计不完整: %#v", stoppedExecution)
	}
	pilot, err := st.GetRobotPilot(execution.PilotInstanceID)
	if err != nil || pilot.CurrentExecutionID != "" {
		t.Fatalf("精确 Pilot 执行指针没有释放: pilot=%+v err=%v", pilot, err)
	}
	reserved, err := st.IsRobotTaskReserved(task.AssignedRobotID, "")
	if err != nil || reserved {
		t.Fatalf("终态 Task 不应继续占用 Robot: reserved=%v err=%v", reserved, err)
	}
	// HTTP 超时后的重复确认返回同一终态，不因旧 revision 产生冲突。
	if repeated, repeatErr := service.ConfirmWorkflowStop(
		context.Background(), project.OwnerID, project.ID, view.Workflow.ID,
		paused.Workflow.Revision, true, "现场急停已释放，机器人处于安全保持状态",
	); repeatErr != nil || repeated.Workflow.Status != store.WorkflowStatusStopped {
		t.Fatalf("重复人工确认应幂等: workflow=%+v err=%v", repeated.Workflow, repeatErr)
	}
}

func TestMissingRobotExecutionCanBeConfirmedWithoutDeletingWorkflow(t *testing.T) {
	st, project, conversation, service, _, _ := newWorkflowFixture(t)
	robotService := robotdomain.NewService(st, nil)
	robotService.SetExecutionObserver(service)
	service.robotStop = robotService
	view, _, subTasks := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "sub-missing-execution", Kind: "robot_skill", Goal: "放置"}})
	if _, err := st.AttachSubTaskExecution(
		subTasks[0].ID, subTasks[0].Revision, "rex-missing", time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	current, _ := st.GetWorkflow(view.Workflow.ID)
	paused, err := service.StopWorkflow(
		context.Background(), project.OwnerID, project.ID, view.Workflow.ID, current.Revision,
	)
	if !errors.Is(err, store.ErrOperatorConfirmationRequired) {
		t.Fatalf("缺失 Execution 应明确要求人工确认: err=%v", err)
	}
	if paused.Workflow.Status != store.WorkflowStatusPaused ||
		paused.Workflow.Reason != "execution_state_unknown" {
		t.Fatalf("缺失 Execution 应进入可恢复未知态: view=%+v", paused.Workflow)
	}
	confirmed, err := service.ConfirmWorkflowStop(
		context.Background(), project.OwnerID, project.ID, view.Workflow.ID,
		paused.Workflow.Revision, true, "现场确认机器人未运动且已安全保持",
	)
	if err != nil || confirmed.Workflow.Status != store.WorkflowStatusStopped {
		t.Fatalf("缺失 Execution 无法人工收敛: view=%+v err=%v", confirmed.Workflow, err)
	}
	recovered, err := st.GetRobotExecution("rex-missing")
	if err != nil || recovered.Status != "stopped" ||
		recovered.Result["confirmation_source"] != "operator" ||
		recovered.SubtaskID != subTasks[0].ID {
		t.Fatalf("缺失 Execution 没有补建审计事实: execution=%+v err=%v", recovered, err)
	}
}

func TestRecoverStoppingWorkflowWithOfflinePilotDoesNotBlockServerStartup(t *testing.T) {
	st, project, conversation, service, _, executor := newWorkflowFixture(t)
	robotService := robotdomain.NewService(st, nil)
	robotService.SetExecutionObserver(service)
	service.robotStop = robotService
	view, task, subTasks := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "sub-recover-stop", Kind: "robot_skill", Goal: "导航"}})
	now := time.Now().UTC()
	execution := store.RobotExecution{
		ID: "rex-recover-stop", ProjectID: project.ID, WorkflowID: view.Workflow.ID,
		TaskID: task.ID, SubtaskID: subTasks[0].ID, RobotID: task.AssignedRobotID,
		PilotInstanceID: "pilot-offline-recovery", SkillName: "semantic-navigation",
		SkillVersion: "0.5.0", RequestKey: "recover-stop", Status: "stopping",
		Revision: 3, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	attached, err := st.AttachSubTaskExecution(
		subTasks[0].ID, subTasks[0].Revision, execution.ID, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionSubTask(
		attached.ID, attached.Revision, store.TaskStatusStopping, "user_stopped", nil, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionTask(
		task.ID, task.Revision, store.TaskStatusStopping, "user_stopped", "", nil, now,
	); err != nil {
		t.Fatal(err)
	}
	current, _ := st.GetWorkflow(view.Workflow.ID)
	if _, err := st.TransitionWorkflowWithReason(
		view.Workflow.ID, current.Revision, store.WorkflowStatusStopping, "user_stopped", now,
	); err != nil {
		t.Fatal(err)
	}

	if err := service.RecoverInterruptedWorkflows(now.Add(time.Second)); err != nil {
		t.Fatalf("预期的 Pilot 离线不得阻止 Server 启动恢复: %v", err)
	}
	recovered, err := st.GetWorkflowView(view.Workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Workflow.Status != store.WorkflowStatusPaused ||
		recovered.Workflow.Reason != "execution_state_unknown" ||
		recovered.Tasks[0].Status != store.TaskStatusPaused ||
		recovered.SubTasks[0].Status != store.TaskStatusPaused {
		t.Fatalf("重启恢复没有收敛到未知态: %+v", recovered)
	}
	latestExecution, err := st.GetRobotExecution(execution.ID)
	if err != nil || latestExecution.Status != "interrupted" {
		t.Fatalf("离线 stopping Execution 应成为 interrupted: execution=%+v err=%v",
			latestExecution, err)
	}
	select {
	case replay := <-executor.started:
		t.Fatalf("状态未知时不得重放 Robot 动作: %+v", replay)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestStopWorkflowSettlesPreActionFailureWithoutPilotStop(t *testing.T) {
	st, project, conversation, service, _, _ := newWorkflowFixture(t)
	view, task, subTasks := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "sub-invalid-input", Kind: "robot_skill", Goal: "校验输入"}})
	execution := store.RobotExecution{ID: "rex-invalid-input", ProjectID: project.ID,
		WorkflowID: view.Workflow.ID, TaskID: task.ID, SubtaskID: subTasks[0].ID,
		RobotID: "r1pro-test", SkillName: "grasp-object", SkillVersion: "0.4.0",
		Status: "failed", Error: map[string]any{"code": "INVALID_INPUT"},
		Revision: 2, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AttachSubTaskExecution(subTasks[0].ID, subTasks[0].Revision,
		execution.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	current, _ := st.GetWorkflow(view.Workflow.ID)
	result, err := service.StopWorkflow(context.Background(), project.OwnerID, project.ID,
		view.Workflow.ID, current.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if result.Workflow.Status != store.WorkflowStatusStopped {
		t.Fatalf("Action前失败应直接收敛停止，workflow=%+v", result.Workflow)
	}
	stoppedSubTask, _ := st.GetSubTask(subTasks[0].ID)
	if stoppedSubTask.Status != store.TaskStatusStopped ||
		stoppedSubTask.WaitingReason != "robot_execution_failed_before_action" {
		t.Fatalf("Action前失败的SubTask状态错误: %+v", stoppedSubTask)
	}
}

type robotDecisionExecutor struct {
	*controlledExecutor
	requests chan RobotAgentDecisionRequest
}

func (e *robotDecisionExecutor) ResolveRobotAgentRequest(_ context.Context,
	request RobotAgentDecisionRequest) (map[string]any, error) {
	e.requests <- request
	return map[string]any{"choice": "refresh_target"}, nil
}

type robotReplyRecord struct {
	executionID string
	payload     map[string]any
}

type recordingRobotReply struct{ replies chan robotReplyRecord }

func (r *recordingRobotReply) ReplyAgentRequest(executionID string,
	payload map[string]any) error {
	r.replies <- robotReplyRecord{executionID: executionID, payload: payload}
	return nil
}

func TestRobotWaitingAgentUsesSameTaskContextAndRepliesPilot(t *testing.T) {
	st := openWorkflowStore(t)
	project, conversation := createWorkflowProject(t, st)
	planner := &fakePlanner{}
	executor := &robotDecisionExecutor{
		controlledExecutor: &controlledExecutor{started: make(chan TaskExecution, 1),
			release: make(chan struct{}, 1)},
		requests: make(chan RobotAgentDecisionRequest, 1),
	}
	reply := &recordingRobotReply{replies: make(chan robotReplyRecord, 1)}
	service, err := NewService(Deps{Store: st, Planner: planner, Executor: executor,
		RobotReply: reply})
	if err != nil {
		t.Fatal(err)
	}
	view, task, subTasks := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "sub-grasp", Kind: "robot_skill", Goal: "抓取"}})
	execution := store.RobotExecution{ID: "rex-waiting", ProjectID: project.ID,
		WorkflowID: view.Workflow.ID, TaskID: task.ID, SubtaskID: subTasks[0].ID,
		RobotID: "r1pro-test", SkillName: "grasp-object", SkillVersion: "0.1.0",
		Status: "waiting_agent"}
	// Robot Service 总是先持久化 Execution，再把事件交给 Workflow Observer。
	// 测试必须遵循同一顺序，否则决策完成后的“仍是当前 Execution”检查只能
	// 看到一个并不存在于 Store 的临时值，并会正确丢弃这份迟到回复。
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	event := map[string]any{
		"stage": "grasp", "decision_key": "grasp-recovery",
		"decision_revision": float64(3), "reason": "候选已耗尽",
		"context":        map[string]any{"remaining_budget": float64(0)},
		"response_model": "GraspRecoveryDecisionV1",
	}
	if err := st.AppendRobotExecutionEvent(store.RobotExecutionEvent{
		ExecutionID: execution.ID, Sequence: 1, Type: "agent.requested",
		Payload: event, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	// 模拟Pilot的agent.requested恰好先于当前Task Execution Run释放写门禁。
	// 即时回调不能并发启动第二个Agent Run；Run结束后的统一延续入口必须从
	// 持久事件补接决策，否则Skill会永久停在waiting_agent。
	if !service.reserveTask(task) {
		t.Fatal("测试无法预留Task写门禁")
	}
	if err := service.OnRobotExecutionChanged(context.Background(), execution,
		"agent.requested", event); err != nil {
		t.Fatal(err)
	}
	select {
	case <-executor.requests:
		t.Fatal("Task Execution尚未释放时不应并发启动Robot决策")
	default:
	}
	service.releaseTask(task)
	if err := service.continueOrFinishTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-executor.requests:
		if request.Task.ID != task.ID || request.SubTask.ID != subTasks[0].ID ||
			request.Execution.ID != execution.ID {
			t.Fatalf("Robot Agent 没有复用精确 Task Context: %+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("Robot Agent 决策 Run 未启动")
	}
	select {
	case got := <-reply.replies:
		if got.executionID != execution.ID ||
			got.payload["decision_key"] != "grasp-recovery" ||
			got.payload["decision_revision"] != int64(3) {
			t.Fatalf("Pilot 回复信封不匹配原请求: %#v", got)
		}
		body, _ := got.payload["payload"].(map[string]any)
		if body["choice"] != "refresh_target" {
			t.Fatalf("Pilot 回复没有携带类型化决策: %#v", got.payload)
		}
	case <-time.After(time.Second):
		t.Fatal("决策没有回复 Pilot")
	}
	currentTask, _ := st.GetTask(task.ID)
	currentSubTask, _ := st.GetSubTask(subTasks[0].ID)
	if currentTask.Status != store.TaskStatusRunning ||
		currentSubTask.Status != store.TaskStatusRunning {
		t.Fatalf("正常 waiting_agent 不应暂停任务: task=%+v subtask=%+v",
			currentTask, currentSubTask)
	}
}

type scriptedDecisionReply struct {
	value map[string]any
	err   error
}

type scriptedRobotDecisionExecutor struct {
	*controlledExecutor
	mu      sync.Mutex
	replies []scriptedDecisionReply
	seen    chan RobotAgentDecisionRequest
}

func (e *scriptedRobotDecisionExecutor) ResolveRobotAgentRequest(_ context.Context,
	request RobotAgentDecisionRequest) (map[string]any, error) {
	e.seen <- request
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.replies) == 0 {
		return nil, fmt.Errorf("没有预置决策: %w", ErrRobotAgentDecisionInvalid)
	}
	next := e.replies[0]
	e.replies = e.replies[1:]
	return next.value, next.err
}

func invalidRobotDecision() error {
	return fmt.Errorf("Robot Agent 回复为空: %w", ErrRobotAgentDecisionInvalid)
}

func prepareWaitingAgentDecision(t *testing.T, st *store.Store, service *Service,
	project store.Project, conversation store.ChatSession, event map[string]any,
) (store.WorkflowView, store.Task, []store.SubTask, store.RobotExecution) {
	t.Helper()
	view, task, subTasks := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "sub-grasp", Kind: "robot_skill", Goal: "抓取"}})
	execution := store.RobotExecution{ID: "rex-waiting-decision", ProjectID: project.ID,
		WorkflowID: view.Workflow.ID, TaskID: task.ID, SubtaskID: subTasks[0].ID,
		RobotID: "r1pro-test", SkillName: "grasp-object", SkillVersion: "0.1.0",
		Status: "waiting_agent"}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendRobotExecutionEvent(store.RobotExecutionEvent{
		ExecutionID: execution.ID, Sequence: 1, Type: "agent.requested",
		Payload: event, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if !service.reserveTask(task) {
		t.Fatal("测试无法预留Task写门禁")
	}
	if err := service.OnRobotExecutionChanged(context.Background(), execution,
		"agent.requested", event); err != nil {
		t.Fatal(err)
	}
	service.releaseTask(task)
	if err := service.continueOrFinishTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	return view, task, subTasks, execution
}

func waitDecisionRequest(t *testing.T, seen <-chan RobotAgentDecisionRequest) RobotAgentDecisionRequest {
	t.Helper()
	select {
	case request := <-seen:
		return request
	case <-time.After(workflowEventWait):
		t.Fatalf("Robot Agent 决策 Run 未在 %s 内启动", workflowEventWait)
	}
	return RobotAgentDecisionRequest{}
}

func TestRobotDecisionEmptyReplyRetriesOnceWhileTaskKeepsRunning(t *testing.T) {
	st := openWorkflowStore(t)
	project, conversation := createWorkflowProject(t, st)
	executor := &scriptedRobotDecisionExecutor{
		controlledExecutor: &controlledExecutor{started: make(chan TaskExecution, 1),
			release: make(chan struct{}, 1)},
		replies: []scriptedDecisionReply{
			{err: invalidRobotDecision()},
			{value: map[string]any{"action": "retry_lift", "reason": "重新抬升"}},
		},
		seen: make(chan RobotAgentDecisionRequest, 4),
	}
	reply := &recordingRobotReply{replies: make(chan robotReplyRecord, 1)}
	service, err := NewService(Deps{Store: st, Planner: &fakePlanner{}, Executor: executor,
		RobotReply: reply})
	if err != nil {
		t.Fatal(err)
	}
	event := map[string]any{
		"stage": "lift_and_verify", "decision_key": "grasp-retry-lift",
		"decision_revision": float64(1), "reason": "抬升未下发",
		"response_schema": map[string]any{"required": []any{"action", "reason"}},
	}
	view, task, subTasks, _ := prepareWaitingAgentDecision(t, st, service, project,
		conversation, event)
	_ = waitDecisionRequest(t, executor.seen)
	_ = waitDecisionRequest(t, executor.seen)
	select {
	case got := <-reply.replies:
		body, _ := got.payload["payload"].(map[string]any)
		if body["action"] != "retry_lift" || got.payload["decision_attempts"] != 2 {
			t.Fatalf("第二次决策应成功回复 Pilot: %#v", got.payload)
		}
	case <-time.After(workflowEventWait):
		t.Fatalf("自动重试后未在 %s 内回复 Pilot", workflowEventWait)
	}
	currentTask, _ := st.GetTask(task.ID)
	currentSubTask, _ := st.GetSubTask(subTasks[0].ID)
	currentWorkflow, _ := st.GetWorkflow(view.Workflow.ID)
	if currentTask.Status != store.TaskStatusRunning ||
		currentSubTask.Status != store.TaskStatusRunning ||
		currentWorkflow.Status != store.WorkflowStatusRunning {
		t.Fatalf("自动重试期间不得暂停: task=%+v subtask=%+v workflow=%+v",
			currentTask, currentSubTask, currentWorkflow)
	}
}

func TestRobotDecisionEmptyReplyPausesWorkflowAfterThreeAttempts(t *testing.T) {
	st := openWorkflowStore(t)
	project, conversation := createWorkflowProject(t, st)
	executor := &scriptedRobotDecisionExecutor{
		controlledExecutor: &controlledExecutor{started: make(chan TaskExecution, 1),
			release: make(chan struct{}, 1)},
		replies: []scriptedDecisionReply{
			{err: invalidRobotDecision()},
			{err: invalidRobotDecision()},
			{err: invalidRobotDecision()},
		},
		seen: make(chan RobotAgentDecisionRequest, 4),
	}
	reply := &recordingRobotReply{replies: make(chan robotReplyRecord, 1)}
	service, err := NewService(Deps{Store: st, Planner: &fakePlanner{}, Executor: executor,
		RobotReply: reply})
	if err != nil {
		t.Fatal(err)
	}
	event := map[string]any{
		"stage": "lift_and_verify", "decision_key": "grasp-empty",
		"decision_revision": float64(1), "reason": "抬升未下发",
		"response_schema": map[string]any{"required": []any{"action", "reason"}},
	}
	_, task, subTasks, execution := prepareWaitingAgentDecision(t, st, service, project,
		conversation, event)
	for index := 0; index < 3; index++ {
		_ = waitDecisionRequest(t, executor.seen)
	}
	deadline := time.Now().Add(2 * time.Second)
	var currentWorkflow store.Workflow
	for {
		currentWorkflow, _ = st.GetWorkflow(execution.WorkflowID)
		if currentWorkflow.Status == store.WorkflowStatusPaused &&
			currentWorkflow.Reason == "robot_agent_decision_failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("第三次空回复后 Workflow 未暂停: %+v", currentWorkflow)
		}
		time.Sleep(20 * time.Millisecond)
	}
	currentTask, _ := st.GetTask(task.ID)
	currentSubTask, _ := st.GetSubTask(subTasks[0].ID)
	currentExecution, _ := st.GetRobotExecution(execution.ID)
	if currentTask.Status != store.TaskStatusPaused ||
		currentTask.WaitingReason != "robot_agent_decision_failed" ||
		currentSubTask.Status != store.TaskStatusPaused ||
		currentSubTask.WaitingReason != "robot_agent_decision_failed" ||
		currentExecution.Status != "waiting_agent" {
		t.Fatalf("决策用尽后状态错误: task=%+v subtask=%+v execution=%+v",
			currentTask, currentSubTask, currentExecution)
	}
	select {
	case got := <-reply.replies:
		t.Fatalf("用尽重试后不得回复 Pilot: %#v", got)
	default:
	}
}

func TestRetryRobotAgentDecisionOnlyStartsDecisionRun(t *testing.T) {
	st := openWorkflowStore(t)
	project, conversation := createWorkflowProject(t, st)
	executor := &scriptedRobotDecisionExecutor{
		controlledExecutor: &controlledExecutor{started: make(chan TaskExecution, 1),
			release: make(chan struct{}, 1)},
		replies: []scriptedDecisionReply{
			{err: invalidRobotDecision()},
			{err: invalidRobotDecision()},
			{err: invalidRobotDecision()},
			{value: map[string]any{"action": "abort_subtask", "reason": "保持现场"}},
		},
		seen: make(chan RobotAgentDecisionRequest, 8),
	}
	reply := &recordingRobotReply{replies: make(chan robotReplyRecord, 1)}
	service, err := NewService(Deps{Store: st, Planner: &fakePlanner{}, Executor: executor,
		RobotReply: reply})
	if err != nil {
		t.Fatal(err)
	}
	event := map[string]any{
		"stage": "lift_and_verify", "decision_key": "grasp-retry-entry",
		"decision_revision": float64(2), "reason": "抬升未下发",
	}
	view, _, _, _ := prepareWaitingAgentDecision(t, st, service, project,
		conversation, event)
	for index := 0; index < 3; index++ {
		_ = waitDecisionRequest(t, executor.seen)
	}
	deadline := time.Now().Add(2 * time.Second)
	var paused store.Workflow
	for {
		paused, _ = st.GetWorkflow(view.Workflow.ID)
		if paused.Status == store.WorkflowStatusPaused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("测试未能等到决策失败暂停")
		}
		time.Sleep(20 * time.Millisecond)
	}
	retried, err := service.RetryRobotAgentDecision(context.Background(), project.OwnerID,
		project.ID, paused.ID, paused.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Workflow.Status != store.WorkflowStatusRunning {
		t.Fatalf("重试决策后 Workflow 应回到 running: %+v", retried.Workflow)
	}
	_ = waitDecisionRequest(t, executor.seen)
	select {
	case started := <-executor.started:
		t.Fatalf("重试决策不得启动 Task 执行通道: %+v", started)
	case <-time.After(150 * time.Millisecond):
	}
	select {
	case got := <-reply.replies:
		body, _ := got.payload["payload"].(map[string]any)
		if body["action"] != "abort_subtask" {
			t.Fatalf("重试决策没有把类型化回复交给 Pilot: %#v", got.payload)
		}
	case <-time.After(workflowEventWait):
		t.Fatalf("重试决策未在 %s 内回复 Pilot", workflowEventWait)
	}
}
