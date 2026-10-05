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
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

type recordingRobotAgentExecutor struct {
	st      *store.Store
	started chan TaskExecution
}

func (r *recordingRobotAgentExecutor) ExecuteTask(_ context.Context,
	input TaskExecution) (TaskResult, error) {
	subtask := input.SubTasks[0]
	now := time.Now().UTC()
	execution := store.RobotExecution{
		ID: "rex-agent-dispatch", ProjectID: input.Workflow.ProjectID,
		WorkflowID: input.Workflow.ID, TaskID: input.Task.ID, SubtaskID: subtask.ID,
		RunID: input.Run.ID, RobotID: input.Task.AssignedRobotID,
		PilotInstanceID: "pilot-r1pro-test", SkillName: "grasp-object",
		SkillVersion: "0.3.0", RequestKey: subtask.ID, Status: "running",
		Input:    map[string]any{"target": map[string]any{"object_ref": "box-17"}},
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := r.st.SaveRobotExecution(execution); err != nil {
		return TaskResult{}, err
	}
	r.started <- input
	return TaskResult{}, nil
}

func TestRobotSkillSubTaskUsesExecutionTimeRobotAgent(t *testing.T) {
	st := openWorkflowStore(t)
	project, conversation := createWorkflowProject(t, st)
	planner := &fakePlanner{}
	executor := &recordingRobotAgentExecutor{
		st: st, started: make(chan TaskExecution, 1),
	}
	service, err := NewService(Deps{
		Store: st, Planner: planner, Executor: executor,
	})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := st.SubmitPlanProposal(project.ID, conversation.ID,
		store.WorkflowDraft{Goal: "搬运", Tasks: []store.TaskDraft{{
			ID: "task-deterministic", RequiredRole: "robot", Goal: "搬运箱体",
		}}}, "", nil, "# 搬运", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	view, err := st.ApprovePlanProposal(ready.ID, ready.Revision, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.AssignTask(view.Tasks[0].ID, view.Tasks[0].Revision,
		"robot:r1pro-test", "r1pro-test", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	spec := json.RawMessage(`{"skill_name":"grasp-object","skill_version":"0.3.0","input":{"target":{"object_ref":"box-17"}}}`)
	task, subtasks, err := st.SetTaskSubTasks(task.ID, task.Revision,
		[]store.SubTaskDraft{{ID: "sub-deterministic", Kind: "robot_skill",
			Goal: "抓取箱体", Spec: spec}}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	task, err = st.TransitionTask(task.ID, task.Revision, store.TaskStatusRunning,
		"", "", nil, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	run := store.RunSession{
		ID: "run-deterministic", ProjectID: project.ID,
		ChatSessionID: conversation.ID, WorkflowID: view.Workflow.ID, TaskID: task.ID,
		Kind: store.RunKindTaskExecution, ContextID: task.ContextID,
		AgentID: task.AssignedAgentID, AgentName: task.AssignedAgentID,
		Status: store.RunStatusRunning, StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateRunSession(run); err != nil {
		t.Fatal(err)
	}
	if !service.reserveTask(task) {
		t.Fatal("测试 Task 应取得单写执行权")
	}
	go service.executeTask(context.Background(), project.OwnerID, view.Workflow, task, nil, run)
	select {
	case started := <-executor.started:
		if len(started.SubTasks) != 1 || started.SubTasks[0].ID != subtasks[0].ID {
			t.Fatalf("Robot Agent收到错误的 SubTask: %+v", started)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("执行期 Robot Agent Run 未启动")
	}
	var current store.SubTask
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, err = st.GetSubTask(subtasks[0].ID)
		if err == nil && current.ExecutionRef == "rex-agent-dispatch" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || current.ExecutionRef != "rex-agent-dispatch" ||
		current.Status != store.TaskStatusRunning {
		t.Fatalf(
			"Robot Execution 未正确关联且保持 running: subtask=%+v err=%v",
			current, err,
		)
	}
}
