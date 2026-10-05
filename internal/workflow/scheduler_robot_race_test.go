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
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

// TestFastRobotTerminalBeforeAgentRunReturns 覆盖真实 Fake/高速后端竞态：
// Skill 终态先于模型尾声到达时，旧 revision 的关联不能把已经完成的 SubTask
// 判成 Task 失败；Agent Run 退出后必须从持久状态启动下一步。
func TestFastRobotTerminalBeforeAgentRunReturns(t *testing.T) {
	st, project, conversation, service, _, executor := newWorkflowFixture(t)
	now := time.Now().UTC()
	ready, err := st.SubmitPlanProposal(project.ID, conversation.ID,
		store.WorkflowDraft{Goal: "搬运", Tasks: []store.TaskDraft{{
			ID: "task-fast-terminal", RequiredRole: "robot", Goal: "搬运一个箱子",
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
	task, subTasks, err := st.SetTaskSubTasks(task.ID, task.Revision,
		[]store.SubTaskDraft{
			{ID: "sub-fast-grasp", Kind: "robot_skill", Goal: "抓取"},
			{ID: "sub-after-grasp", Kind: "robot_skill", Goal: "导航",
				DependsOn: []string{"sub-fast-grasp"}},
		}, now)
	if err != nil {
		t.Fatal(err)
	}
	task, err = st.TransitionTask(task.ID, task.Revision, store.TaskStatusRunning,
		"", "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	run := store.RunSession{
		ID: "run-fast-terminal", ProjectID: project.ID,
		ChatSessionID: conversation.ID, WorkflowID: view.Workflow.ID, TaskID: task.ID,
		Kind: store.RunKindTaskExecution, ContextID: task.ContextID,
		AgentID: task.AssignedAgentID, AgentName: task.AssignedAgentID,
		Status: store.RunStatusRunning, StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateRunSession(run); err != nil {
		t.Fatal(err)
	}
	if !service.reserveTask(task) {
		t.Fatal("测试 Task 应可取得单写 Run")
	}
	go service.executeTask(context.Background(), project.OwnerID, view.Workflow, task, nil, run)
	first := receiveExecution(t, executor)
	if len(first.SubTasks) != 1 || first.SubTasks[0].ID != subTasks[0].ID {
		t.Fatalf("首个 Run 没有执行预期 Robot SubTask: %+v", first.SubTasks)
	}

	execution := store.RobotExecution{
		ID: "rex-fast-terminal", ProjectID: project.ID, WorkflowID: view.Workflow.ID,
		TaskID: task.ID, SubtaskID: subTasks[0].ID, RobotID: "r1pro-test",
		SkillName: "grasp-object", SkillVersion: "0.1.0", RequestKey: "fast-terminal",
		Status: "completed", Result: map[string]any{"held": true},
		Revision: 2, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	if err := service.OnRobotExecutionChanged(context.Background(), execution,
		"execution.terminal", execution.Result); err != nil {
		t.Fatal(err)
	}

	executor.release <- struct{}{}
	second := receiveExecution(t, executor)
	if len(second.SubTasks) != 1 || second.SubTasks[0].ID != subTasks[1].ID {
		t.Fatalf("终态竞态后没有推进到下一 SubTask: %+v", second.SubTasks)
	}
	currentTask, _ := st.GetTask(task.ID)
	firstSubTask, _ := st.GetSubTask(subTasks[0].ID)
	if currentTask.Status != store.TaskStatusRunning ||
		firstSubTask.Status != store.TaskStatusCompleted {
		t.Fatalf("已先收敛的终态不得被旧 revision 覆盖: task=%+v subtask=%+v",
			currentTask, firstSubTask)
	}

	currentWorkflow, _ := st.GetWorkflow(view.Workflow.ID)
	if _, err := service.StopWorkflow(context.Background(), project.OwnerID, project.ID,
		view.Workflow.ID, currentWorkflow.Revision); err != nil {
		t.Fatal(err)
	}
	waitWorkflowStatus(t, st, view.Workflow.ID, store.WorkflowStatusStopped)
}
