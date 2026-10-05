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

package robot

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/robotruntime"
	"insightos.cn/semantic-framework/internal/store"
)

func prepareVerticalFlowTask(t *testing.T, st *store.Store, project store.Project) string {
	t.Helper()
	now := time.Now().UTC()
	conversation := store.ChatSession{ID: "conversation-vertical", UserID: project.OwnerID,
		ProjectID: project.ID, Title: "执行审批", CreatedAt: now, UpdatedAt: now}
	if err := st.CreateChatSession(conversation); err != nil {
		t.Fatal(err)
	}
	proposal, err := st.SubmitPlanProposal(project.ID, conversation.ID, store.WorkflowDraft{
		Goal: "抓取", Tasks: []store.TaskDraft{{ID: "task-1", RequiredRole: "robot", Goal: "抓取",
			SubTasks: []store.SubTaskDraft{{ID: "subtask-1", Kind: "robot_skill", Goal: "抓取",
				Spec: json.RawMessage(`{"skill_name":"grasp-object","skill_version":"0.1.0"}`)}}}},
	}, "", nil, "# 抓取", now)
	if err != nil {
		t.Fatal(err)
	}
	view, err := st.ApprovePlanProposal(proposal.ID, proposal.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.AssignTask("task-1", view.Tasks[0].Revision, "robot:r1pro-1", "r1pro-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionTask(task.ID, task.Revision, store.TaskStatusRunning, "", "", nil, now); err != nil {
		t.Fatal(err)
	}
	subtask, err := st.GetSubTask("subtask-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionSubTask(subtask.ID, subtask.Revision, store.TaskStatusRunning, "", nil, now); err != nil {
		t.Fatal(err)
	}
	return view.Workflow.ID
}

func directRunFixture(t *testing.T) (*Service, <-chan PilotCommand, RunRequest) {
	t.Helper()
	st := openRobotTestStore(t)
	project, err := st.CreateProject("direct-user", "Direct Robot")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.CreateChatSession(store.ChatSession{ID: "conversation-direct", UserID: project.OwnerID,
		ProjectID: project.ID, Title: "Direct", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRunSession(store.RunSession{ID: "run-direct", ProjectID: project.ID,
		ChatSessionID: "conversation-direct", AgentID: "robot:robot-direct", Status: store.RunStatusRunning}); err != nil {
		t.Fatal(err)
	}
	service := NewService(st, nil)
	commands, disconnect, err := service.Connect(store.RobotPilot{PilotInstanceID: "pilot-direct",
		RobotID: "robot-direct", RobotStatus: "idle", AbilityFrameworkStatus: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(disconnect)
	if err := st.SaveRobotPilotSkill(store.RobotPilotSkill{PilotInstanceID: "pilot-direct",
		Name: "skill", Version: "1", Enabled: true, Status: "installed"}); err != nil {
		t.Fatal(err)
	}
	return service, commands, RunRequest{ProjectID: project.ID, RunID: "run-direct", AgentID: "robot:robot-direct",
		RobotID: "robot-direct", SkillName: "skill", SkillVersion: "1", RequestKey: "run-direct:call-1",
		Input: map[string]any{"target_ref": "box-1"}}
}

func TestDirectRunRetainsOwnershipBetweenSkills(t *testing.T) {
	service, commands, request := directRunFixture(t)
	first, err := runWithValidSkillInput(t, service, commands, "pilot-direct", request)
	if err != nil {
		t.Fatal(err)
	}
	receiveCommand(t, commands, "execution.start")
	if first.TaskID != "" || first.WorkflowID != "" || first.RunID != request.RunID {
		t.Fatalf("直接 Skill 不得伪造 Task: %+v", first)
	}
	if err := service.HandlePilotEvent("pilot-direct", "execution.completed", 1,
		map[string]any{"execution_id": first.ID, "status": "completed"}); err != nil {
		t.Fatal(err)
	}
	device, err := service.Device(request.RobotID)
	if err != nil || device["status"] != "busy" || device["run_id"] != request.RunID {
		t.Fatalf("模型等待时应显示请求占用: %+v %v", device, err)
	}
	independent := request
	independent.RunID, independent.AgentID, independent.RequestKey = "", "", "http-call"
	if _, err := service.Run(context.Background(), independent); !errors.Is(err, ErrRobotBusy) {
		t.Fatalf("HTTP 不能抢占连续请求间隙: %v", err)
	}
	request.RequestKey = "run-direct:call-2"
	second, err := runWithValidSkillInput(t, service, commands, "pilot-direct", request)
	if err != nil {
		t.Fatal(err)
	}
	receiveCommand(t, commands, "execution.start")
	if first.ID == second.ID {
		t.Fatal("不同工具调用应生成新的 Execution")
	}
	if _, err := service.Stop(context.Background(), request.ProjectID, first.ID, "stale_history"); !errors.Is(err, ErrExecutionNotActive) {
		t.Fatalf("历史已完成 Skill 不可停止: %v", err)
	}
	if run, err := service.st.GetRunSession(request.RunID); err != nil || run.Status != store.RunStatusRunning {
		t.Fatalf("停止历史Skill不能取消当前请求: %+v %v", run, err)
	}
	if err := service.StopDirectRun(context.Background(), request.RunID); err != nil {
		t.Fatal(err)
	}
	receiveCommand(t, commands, "execution.stop")
	run, err := service.st.GetRunSession(request.RunID)
	if err != nil || run.Status != store.RunStatusCancelling {
		t.Fatalf("停止必须先阻止后续技能: %+v %v", run, err)
	}
	request.RequestKey = "run-direct:call-3"
	if _, err := service.Run(context.Background(), request); !errors.Is(err, ErrDirectRunScope) {
		t.Fatalf("停止请求后不得开始下个 Skill: %v", err)
	}
	pilot, _ := service.st.GetActiveRobotPilot(request.RobotID)
	if pilot.CurrentExecutionID != second.ID {
		t.Fatalf("stop 接受不能代替物理停止证据释放指针: %+v", pilot)
	}
}

func TestFinishDirectRunDoesNotReleaseUnconfirmedPhysicalStop(t *testing.T) {
	service, commands, request := directRunFixture(t)
	execution, err := runWithValidSkillInput(t, service, commands, "pilot-direct", request)
	if err != nil {
		t.Fatal(err)
	}
	receiveCommand(t, commands, "execution.start")
	if _, err := service.st.FinishRunSession(request.RunID, nil, store.RunStatusFailed, "Agent失败", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := service.FinishDirectRun(context.Background(), request.RunID); err != nil {
		t.Fatal(err)
	}
	receiveCommand(t, commands, "execution.stop")
	independent := request
	independent.RunID, independent.AgentID, independent.RequestKey = "", "", "after-finish"
	if _, err := service.Run(context.Background(), independent); !errors.Is(err, ErrRobotBusy) {
		t.Fatalf("Run终态不能绕过未完成的物理停止: %v", err)
	}
	pilot, _ := service.st.GetActiveRobotPilot(request.RobotID)
	stored, _ := service.st.GetRobotExecution(execution.ID)
	if pilot.CurrentExecutionID != execution.ID || stored.Status != "stopping" {
		t.Fatalf("必须保留物理占用: pilot=%+v execution=%+v", pilot, stored)
	}
}

func TestDirectCancelDuringValidationNeverStartsSkill(t *testing.T) {
	service, commands, request := directRunFixture(t)
	done := make(chan error, 1)
	go func() {
		_, err := service.Run(context.Background(), request)
		done <- err
	}()
	validation := receiveCommand(t, commands, "skill.validate_input")
	if err := service.StopDirectRun(context.Background(), request.RunID); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleCommandAckResult("pilot-direct", validation.CommandID, true, "",
		map[string]any{"valid": true, "input": request.Input}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrDirectRunScope) {
		t.Fatalf("校验期间取消必须拒绝准入: %v", err)
	}
	executions, _ := service.st.ListRobotExecutionsByRun(request.RunID)
	if len(executions) != 0 {
		t.Fatalf("取消后的请求不得入队: %+v", executions)
	}
	select {
	case command := <-commands:
		t.Fatalf("取消后出现物理命令: %+v", command)
	default:
	}
}

func TestCompletedDirectConversationKeepsPhysicalExecution(t *testing.T) {
	service, commands, request := directRunFixture(t)
	execution, err := runWithValidSkillInput(t, service, commands, "pilot-direct", request)
	if err != nil {
		t.Fatal(err)
	}
	receiveCommand(t, commands, "execution.start")
	if _, err := service.st.FinishRunSession(request.RunID, nil, store.RunStatusCompleted, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := service.FinishDirectRun(context.Background(), request.RunID); err != nil {
		t.Fatal(err)
	}
	pilot, _ := service.st.GetActiveRobotPilot(request.RobotID)
	stored, _ := service.st.GetRobotExecution(execution.ID)
	if pilot.CurrentExecutionID != execution.ID || stored.Status == "stopped" || stored.Status == "stopping" {
		t.Fatalf("对话完成不能结束或释放仍执行中的Skill: pilot=%+v execution=%+v", pilot, stored)
	}
	select {
	case command := <-commands:
		t.Fatalf("等待逻辑不得自行重发 Skill: %+v", command)
	default:
	}
}

func TestDirectRunCancellationStopsPhysicalExecution(t *testing.T) {
	service, commands, request := directRunFixture(t)
	execution, err := runWithValidSkillInput(t, service, commands, "pilot-direct", request)
	if err != nil {
		t.Fatal(err)
	}
	receiveCommand(t, commands, "execution.start")
	if err := service.StopDirectRun(context.Background(), request.RunID); err != nil {
		t.Fatal(err)
	}
	receiveCommand(t, commands, "execution.stop")
	stored, _ := service.st.GetRobotExecution(execution.ID)
	if stored.Status != "stopping" {
		t.Fatalf("取消必须安全停止，不能假报stopped: %+v", stored)
	}
}

func TestRobotRetryUsesApprovedArgumentsBeforeNormalization(t *testing.T) {
	service, commands, request := directRunFixture(t)
	type outcome struct {
		execution store.RobotExecution
		err       error
	}
	done := make(chan outcome, 1)
	go func() {
		execution, err := service.Run(context.Background(), request)
		done <- outcome{execution, err}
	}()
	validation := receiveCommand(t, commands, "skill.validate_input")
	if err := service.HandleCommandAckResult("pilot-direct", validation.CommandID, true, "",
		map[string]any{"valid": true, "input": map[string]any{"target_ref": "box-1", "default_field": true}}); err != nil {
		t.Fatal(err)
	}
	first := <-done
	if first.err != nil {
		t.Fatal(first.err)
	}
	receiveCommand(t, commands, "execution.start")
	duplicate, err := service.Run(context.Background(), request)
	if err != nil || duplicate.ID != first.execution.ID {
		t.Fatalf("补默认值不得破坏相同调用幂等: %+v %v", duplicate, err)
	}
	changed := request
	changed.Input = map[string]any{"target_ref": "box-2"}
	if _, err := service.Run(context.Background(), changed); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同一批准调用不能换参数: %v", err)
	}
}

func TestManagedRobotRejectsOtherProjectBeforeValidation(t *testing.T) {
	service, commands, request := directRunFixture(t)
	if err := service.st.SaveRuntimeInstance(context.Background(), robotruntime.RuntimeInstance{
		InstanceID: "runtime-direct", RobotID: request.RobotID, ProjectID: "another-project",
		Status: "ready", Revision: 1, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(context.Background(), request); !errors.Is(err, ErrDirectRunScope) {
		t.Fatalf("托管Robot不能跨Project调用: %v", err)
	}
	select {
	case command := <-commands:
		t.Fatalf("跨Project请求不应送达Pilot: %+v", command)
	default:
	}
}
