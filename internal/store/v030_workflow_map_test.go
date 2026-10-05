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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func v030ProjectConversation(t *testing.T, st *Store) (Project, ChatSession) {
	t.Helper()
	project, err := st.EnsureDefaultProject("usr-v030")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	session := ChatSession{ID: "cs-v030", UserID: project.OwnerID, ProjectID: project.ID,
		Title: "v0.3", CreatedAt: now, UpdatedAt: now}
	if err := st.CreateChatSession(session); err != nil {
		t.Fatal(err)
	}
	return project, session
}

func approveWorkflowDraft(t *testing.T, st *Store, project Project,
	session ChatSession, draft WorkflowDraft, now time.Time) WorkflowView {
	t.Helper()
	ready, err := st.SubmitPlanProposal(project.ID, session.ID, draft, "", nil, "# "+draft.Goal, now)
	if err != nil {
		t.Fatal(err)
	}
	view, err := st.ApprovePlanProposal(ready.ID, ready.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	for index := range view.Tasks {
		agentID := view.Tasks[index].RequiredRole + "-test"
		robotID := ""
		if view.Tasks[index].RequiredRole == "robot" {
			agentID, robotID = "robot:r1pro-test", "r1pro-test"
		}
		assigned, assignErr := st.AssignTask(view.Tasks[index].ID, view.Tasks[index].Revision, agentID, robotID, now)
		if assignErr != nil {
			t.Fatal(assignErr)
		}
		view.Tasks[index] = assigned
	}
	return view
}

func TestWorkflowRevisionDAGAndScheduling(t *testing.T) {
	st := openTestStore(t)
	project, session := v030ProjectConversation(t, st)
	now := time.Now().UTC()
	draft := WorkflowDraft{Goal: "实现功能", Tasks: []TaskDraft{
		{ID: "task-a", RequiredRole: "developer", Goal: "先实现", SubTasks: []SubTaskDraft{{Kind: "agent_step", Goal: "编码"}}},
		{ID: "task-b", RequiredRole: "developer", Goal: "再测试", SubTasks: []SubTaskDraft{{Kind: "agent_step", Goal: "测试"}}},
	}, Dependencies: []TaskDependency{{TaskID: "task-b", DependsOnTaskID: "task-a"}}}
	cyclic := draft
	cyclic.Dependencies = []TaskDependency{{TaskID: "task-a", DependsOnTaskID: "task-b"}, {TaskID: "task-b", DependsOnTaskID: "task-a"}}
	if _, err := st.SubmitPlanProposal(project.ID, session.ID, cyclic, "", nil, "", now); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("环依赖应拒绝: %v", err)
	}
	proposal, err := st.SubmitPlanProposal(project.ID, session.ID, draft, "", nil, "# 实现功能", now)
	if err != nil {
		t.Fatal(err)
	}
	revised := draft
	revised.Goal = "实现功能并验证"
	ready, err := st.SubmitPlanProposal(project.ID, session.ID, revised, "", nil, "# 实现功能并验证", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApprovePlanProposal(ready.ID, proposal.Revision, now); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("旧 revision 确认应冲突: %v", err)
	}
	confirmed, err := st.ApprovePlanProposal(ready.ID, ready.Revision, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Workflow.Status != WorkflowStatusRunning || confirmed.Workflow.ConfirmedRevision != confirmed.Workflow.Revision {
		t.Fatalf("确认结果不符: %+v", confirmed.Workflow)
	}
	paused, err := st.TransitionWorkflow(confirmed.Workflow.ID, confirmed.Workflow.Revision, WorkflowStatusPaused, now.Add(3*time.Second))
	if err != nil || paused.ConfirmedRevision != paused.Revision {
		t.Fatalf("暂停后确认 revision 未同步: workflow=%+v err=%v", paused, err)
	}
	resumed, err := st.TransitionWorkflow(paused.ID, paused.Revision, WorkflowStatusRunning, now.Add(4*time.Second))
	if err != nil || resumed.ConfirmedRevision != resumed.Revision {
		t.Fatalf("恢复后确认 revision 未同步: workflow=%+v err=%v", resumed, err)
	}
	runnable, err := st.ListRunnableTasks(confirmed.Workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runnable) != 1 || runnable[0].ID != "task-a" {
		t.Fatalf("首批可运行 Task 不符: %+v", runnable)
	}
	done, err := st.TransitionTask("task-a", confirmed.Tasks[0].Revision, TaskStatusRunning, "", "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.TransitionTask("task-a", done.Revision, TaskStatusCompleted, "", "完成", json.RawMessage(`["trace-1"]`), now); err != nil {
		t.Fatal(err)
	}
	runnable, err = st.ListRunnableTasks(confirmed.Workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runnable) != 1 || runnable[0].ID != "task-b" {
		t.Fatalf("依赖完成后可运行 Task 不符: %+v", runnable)
	}
	currentWorkflow, err := st.GetWorkflow(confirmed.Workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	pausedAfterWork, err := st.TransitionWorkflow(currentWorkflow.ID, currentWorkflow.Revision, WorkflowStatusPaused, now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	preserved, err := st.GetTask("task-a")
	if err != nil || preserved.Status != TaskStatusCompleted || preserved.ResultSummary != "完成" || string(preserved.Evidence) != `["trace-1"]` {
		t.Fatalf("completed Task evidence must be preserved: task=%+v err=%v", preserved, err)
	}
	if _, err := st.TransitionWorkflow(pausedAfterWork.ID, pausedAfterWork.Revision, WorkflowStatusRunning, now.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}

	for _, task := range confirmed.Tasks {
		if _, err := st.SaveContextSummary(ContextSummary{ContextID: task.ContextID, ProjectID: project.ID, SessionID: session.ID, TaskID: task.ID, Summary: "摘要-" + task.ID}, 0); err != nil {
			t.Fatalf("同一 Conversation 的多个 Task Context 应可各自保存摘要: %v", err)
		}
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(index int) {
			<-start
			task := confirmed.Tasks[0]
			results <- st.CreateRunSession(RunSession{ID: fmt.Sprintf("run-concurrent-%d", index), ProjectID: project.ID, ChatSessionID: session.ID, WorkflowID: confirmed.Workflow.ID, TaskID: task.ID, Kind: RunKindTaskExecution, ContextID: task.ContextID, AgentID: task.AssignedAgentID, AgentName: task.AssignedAgentID, Status: RunStatusRunning, StartedAt: now.Add(time.Duration(index) * time.Millisecond)})
		}(i)
	}
	close(start)
	succeeded := 0
	for i := 0; i < 2; i++ {
		if err := <-results; err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("同一 Task Context 并发 Run 只能有一个成功，实际 %d", succeeded)
	}
	other := confirmed.Tasks[1]
	if err := st.CreateRunSession(RunSession{ID: "run-other-context", ProjectID: project.ID, ChatSessionID: session.ID, WorkflowID: confirmed.Workflow.ID, TaskID: other.ID, Kind: RunKindTaskExecution, ContextID: other.ContextID, AgentID: other.AssignedAgentID, AgentName: other.AssignedAgentID, Status: RunStatusRunning, StartedAt: now.Add(time.Second)}); err != nil {
		t.Fatalf("不同 Task Context 应允许并行: %v", err)
	}
}

func TestWorkflowRunAllowsCheckpointApprovalWithoutTaskContinuation(t *testing.T) {
	st := openTestStore(t)
	project, session := v030ProjectConversation(t, st)
	now := time.Now().UTC()
	view := approveWorkflowDraft(t, st, project, session, WorkflowDraft{Goal: "Robot 执行审批",
		Tasks: []TaskDraft{{ID: "task-approval", RequiredRole: "robot", Goal: "执行 Robot Skill",
			SubTasks: []SubTaskDraft{{Kind: "robot_skill", Goal: "启动 Skill"}}}}}, now)
	task := view.Tasks[0]
	run := RunSession{ID: "run-task-approval", ProjectID: project.ID,
		ChatSessionID: session.ID, WorkflowID: view.Workflow.ID, TaskID: task.ID,
		Kind: RunKindTaskExecution, ContextID: task.ContextID, AgentID: task.AssignedAgentID,
		AgentName: task.AssignedAgentID, Status: RunStatusWaitingInput, StartedAt: now, UpdatedAt: now}
	if err := st.CreateRunSession(run); err != nil {
		t.Fatal(err)
	}

	// 工具审批属于当前 Run 的 checkpoint，不是要求 Workflow 创建新 attempt 的
	// 结构化 Task 交互，因此无需重复保存可由 Run 唯一确定的 Workflow/Task 字段。
	approval := Interaction{ID: "int-task-approval", ProjectID: project.ID,
		SessionID: session.ID, RunID: run.ID, CheckpointID: run.ID, Agent: task.AssignedAgentID,
		Type: InteractionTypeConfirm, Status: InteractionStatusPending,
		Payload: `{"question":"允许 Robot 执行？"}`, CreatedAt: now}
	if err := st.CreateInteraction(approval); err != nil {
		t.Fatalf("Workflow Run 的 checkpoint 审批应可创建: %v", err)
	}
	got, err := st.GetInteraction(approval.ID)
	if err != nil || got.WorkflowID != "" || got.TaskID != "" || got.CheckpointID != run.ID {
		t.Fatalf("checkpoint 审批不应伪装成 Task continuation: interaction=%+v err=%v", got, err)
	}

	approval.ID = "int-task-approval-mismatch"
	approval.WorkflowID = "wf-other"
	if err := st.CreateInteraction(approval); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("显式提供错误 Workflow 归属时必须拒绝，实际: %v", err)
	}
}

func TestTaskContinuationKeepsHistoryAndAllowsOneActiveAttempt(t *testing.T) {
	st := openTestStore(t)
	project, session := v030ProjectConversation(t, st)
	now := time.Now().UTC()
	view := approveWorkflowDraft(t, st, project, session, WorkflowDraft{Goal: "续跑",
		Tasks: []TaskDraft{{ID: "task-continuation", RequiredRole: "developer", Goal: "实现",
			SubTasks: []SubTaskDraft{{Kind: "agent_step", Goal: "继续执行"}}}}}, now)
	task, err := st.TransitionTask(view.Tasks[0].ID, view.Tasks[0].Revision,
		TaskStatusRunning, "", "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	task, err = st.TransitionTask(task.ID, task.Revision, TaskStatusPaused,
		"waiting_input", "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	interactionID := "int-task-continuation"
	if err := st.CreateInteraction(Interaction{ID: interactionID, ProjectID: project.ID,
		SessionID: session.ID, WorkflowID: view.Workflow.ID, TaskID: task.ID,
		Agent: task.AssignedAgentID, Type: InteractionTypeInput, UIKind: InteractionUIForm,
		Status: InteractionStatusPending, Payload: `{}`, ResponseSchema: `{}`,
		SourceRevision: task.Revision, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.AnswerInteractionAtRevision(interactionID, `{"value":"继续"}`,
		task.Revision, now); err != nil {
		t.Fatal(err)
	}
	newRun := func(id string) RunSession {
		return RunSession{ID: id, ProjectID: project.ID, ChatSessionID: session.ID,
			WorkflowID: view.Workflow.ID, TaskID: task.ID, SourceInteractionID: interactionID,
			Kind: RunKindTaskExecution, ContextID: task.ContextID, AgentID: task.AssignedAgentID,
			AgentName: task.AssignedAgentID, Status: RunStatusQueued, StartedAt: now, UpdatedAt: now}
	}
	first, running, created, err := st.StartTaskContinuation(task.ID, task.Revision,
		newRun("run-continuation-1"), false, now)
	if err != nil || !created || running.Status != TaskStatusRunning {
		t.Fatalf("首次 continuation 预留失败: run=%+v task=%+v created=%v err=%v", first, running, created, err)
	}
	if _, err := st.FinishRunSession(first.ID, []string{RunStatusQueued}, RunStatusFailed,
		"server_restarted", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	paused, err := st.TransitionTask(running.ID, running.Revision, TaskStatusPaused,
		"server_restarted", "", nil, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	second, running, created, err := st.StartTaskContinuation(task.ID, paused.Revision,
		newRun("run-continuation-2"), true, now.Add(2*time.Second))
	if err != nil || !created || running.Status != TaskStatusRunning {
		t.Fatalf("显式恢复未创建新 attempt: run=%+v task=%+v created=%v err=%v", second, running, created, err)
	}
	old, err := st.GetRunSession(first.ID)
	if err != nil || old.SourceInteractionID != interactionID || second.SourceInteractionID != interactionID {
		t.Fatalf("续跑不应清除历史 Interaction 关联: old=%+v new=%+v err=%v", old, second, err)
	}
	active, _, created, err := st.StartTaskContinuation(task.ID, running.Revision,
		newRun("run-continuation-3"), true, now.Add(3*time.Second))
	if err != nil || created || active.ID != second.ID {
		t.Fatalf("同一 Interaction 只能有一个 active attempt: active=%+v created=%v err=%v", active, created, err)
	}
}

func TestNewProjectCreatesTwoMapsAtomically(t *testing.T) {
	st := openTestStore(t)
	project, err := st.CreateProject("usr-project-maps", "Map Project")
	if err != nil {
		t.Fatal(err)
	}
	maps, err := st.ListSemanticMaps(project.ID)
	if err != nil || len(maps) != 2 {
		t.Fatalf("新 Project 必须直接具有两张 Map: maps=%+v err=%v", maps, err)
	}
}

func TestSemanticMapIsolationGenerationAndManualFields(t *testing.T) {
	st := openTestStore(t)
	project, _ := v030ProjectConversation(t, st)
	now := time.Now().UTC()
	maps, err := st.EnsureSemanticMaps(project.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(maps) != 2 || maps[0].Slot == maps[1].Slot {
		t.Fatalf("两张地图未隔离: %+v", maps)
	}
	sim, err := st.GetSemanticMap(project.ID, MapSlotSimulation)
	if err != nil {
		t.Fatal(err)
	}
	entity := MapEntity{ID: "box-1", Type: "box", Name: "人工箱体", Status: EntityStatusActive, FrameID: "world", Pose: Pose{Orientation: Quaternion{W: 1}}, Bounds: Bounds{Kind: "box", Size: &Position{X: 1, Y: 1, Z: 1}}, Properties: map[string]any{"label": "keep"}, Evidence: []string{"artifact-model"}}
	snapshot, err := st.ApplyMapUpdate(project.ID, MapSlotSimulation, MapUpdate{Generation: sim.Generation, ExpectedRevision: sim.Revision, Source: MapSourceUser, Entities: []MapEntity{entity}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Entities) != 1 || snapshot.Map.Revision != sim.Revision+1 {
		t.Fatalf("人工更新结果不符: %+v", snapshot)
	}
	real, err := st.GetMapSnapshot(project.ID, MapSlotReal, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(real.Entities) != 0 {
		t.Fatalf("real_map 被 simulation_map 污染: %+v", real.Entities)
	}
	incoming := entity
	incoming.Name = "来源名称"
	incoming.Properties = map[string]any{"label": "overwrite"}
	incoming.Evidence = []string{"artifact-live-image", "artifact-model"}
	snapshot, err = st.ApplyMapUpdate(project.ID, MapSlotSimulation, MapUpdate{Generation: sim.Generation, ExpectedRevision: snapshot.Map.Revision, Source: "observation", Entities: []MapEntity{incoming}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Entities[0].Name != "人工箱体" || snapshot.Entities[0].Properties["label"] != "keep" {
		t.Fatalf("人工字段被来源覆盖: %+v", snapshot.Entities[0])
	}
	if got := snapshot.Entities[0].Evidence; len(got) != 2 || got[0] != "artifact-model" || got[1] != "artifact-live-image" {
		t.Fatalf("Observation 应追加而不是覆盖历史证据引用: %+v", got)
	}
	oldGeneration := snapshot.Map.Generation
	next, err := st.CreateMapGeneration(project.ID, MapSlotSimulation, snapshot.Map.Revision, "reset", now)
	if err != nil {
		t.Fatal(err)
	}
	if next.Generation != oldGeneration+1 {
		t.Fatalf("generation 未推进: %+v", next)
	}
	if _, err := st.ApplyMapUpdate(project.ID, MapSlotSimulation, MapUpdate{Generation: oldGeneration, ExpectedRevision: next.Revision, Source: MapSourceTask}, now); !errors.Is(err, ErrStaleMapGeneration) {
		t.Fatalf("旧 generation 更新应拒绝: %v", err)
	}
	if err := st.ValidateMapReference(project.ID, MapSlotSimulation, oldGeneration, []string{"box-1"}); !errors.Is(err, ErrStaleMapGeneration) {
		t.Fatalf("旧引用应失效: %v", err)
	}
	old, err := st.GetMapSnapshot(project.ID, MapSlotSimulation, oldGeneration)
	if err != nil || len(old.Entities) != 1 || !old.ReadOnly || old.Map.Generation != oldGeneration {
		t.Fatalf("旧 generation 应可读: snapshot=%+v err=%v", old, err)
	}
}

func TestSemanticMapProtectsManualDataAndSourceMapping(t *testing.T) {
	st := openTestStore(t)
	project, _ := v030ProjectConversation(t, st)
	now := time.Now().UTC()
	sim, err := st.GetSemanticMap(project.ID, MapSlotSimulation)
	if err != nil {
		t.Fatal(err)
	}
	box := MapEntity{ID: "box-manual", Type: "box", Name: "人工箱", Pose: Pose{Orientation: Quaternion{W: 1}}, Bounds: Bounds{Kind: "box", Size: &Position{X: 1, Y: 1, Z: 1}}}
	zone := MapEntity{ID: "zone-manual", Type: "region", Name: "人工区", Pose: Pose{Orientation: Quaternion{W: 1}}, Bounds: Bounds{Kind: "polygon", Points: []Position{{X: -2, Y: -2}, {X: 2, Y: -2}, {X: 2, Y: 2}}}}
	manual, err := st.ApplyMapUpdate(project.ID, MapSlotSimulation, MapUpdate{Generation: sim.Generation, ExpectedRevision: sim.Revision, Source: MapSourceUser, Entities: []MapEntity{box, zone}, Relations: []MapRelation{{ID: "rel-manual", SubjectID: box.ID, Predicate: "inside", ObjectID: zone.ID}}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyMapUpdate(project.ID, MapSlotSimulation, MapUpdate{Generation: sim.Generation, ExpectedRevision: manual.Map.Revision, Source: "observation", RemoveEntityIDs: []string{box.ID}}, now); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("非用户来源移除人工 Entity 应拒绝: %v", err)
	}
	if _, err := st.ApplyMapUpdate(project.ID, MapSlotSimulation, MapUpdate{Generation: sim.Generation, ExpectedRevision: manual.Map.Revision, Source: "observation", Relations: []MapRelation{{ID: "rel-manual", SubjectID: zone.ID, Predicate: "contains", ObjectID: box.ID}}}, now); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("非用户来源覆盖人工 Relation 应拒绝: %v", err)
	}
	removed, err := st.ApplyMapUpdate(project.ID, MapSlotSimulation, MapUpdate{Generation: sim.Generation, ExpectedRevision: manual.Map.Revision, Source: MapSourceUser, RemoveEntityIDs: []string{box.ID}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed.Relations) != 0 {
		t.Fatalf("removed Entity 的关系不应返回: %+v", removed.Relations)
	}

	real, err := st.GetSemanticMap(project.ID, MapSlotReal)
	if err != nil {
		t.Fatal(err)
	}
	sourceID := "runtime-private-object-42"
	sourced, err := st.ApplyMapUpdate(project.ID, MapSlotReal, MapUpdate{Generation: real.Generation, ExpectedRevision: real.Revision, Source: "simulation", SourceEntities: []MapSourceEntity{{SourceID: sourceID, Entity: MapEntity{Type: "box", Name: "来源箱体", Pose: Pose{Orientation: Quaternion{W: 1}}, Bounds: Bounds{Kind: "box", Size: &Position{X: 1, Y: 1, Z: 1}}}}}}, now)
	if err != nil {
		t.Fatal(err)
	}
	entityID, err := st.ResolveMapSourceEntity(project.ID, MapSlotReal, real.Generation, "simulation", sourceID)
	if err != nil || entityID != sourced.Entities[0].ID {
		t.Fatalf("来源映射错误: id=%s snapshot=%+v err=%v", entityID, sourced.Entities, err)
	}
	encoded, err := json.Marshal(sourced)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), sourceID) || sourced.Map.MapID != MapSlotReal || sourced.Map.Slot != MapSlotReal {
		t.Fatalf("公共 Map 响应泄漏内部来源或 map_id 错误: %s", encoded)
	}
}

func TestSemanticMapQueryReturnsOnlyRelatedSubset(t *testing.T) {
	st := openTestStore(t)
	project, _ := v030ProjectConversation(t, st)
	sim, err := st.GetSemanticMap(project.ID, MapSlotSimulation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.QuerySemanticMap(project.ID, MapSlotSimulation, MapQuery{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("map.query 空条件必须拒绝，避免把整图自动放入 Context: %v", err)
	}
	entity := func(id, kind string) MapEntity {
		return MapEntity{ID: id, Type: kind, Name: id, Pose: Pose{Orientation: Quaternion{W: 1}},
			Bounds: Bounds{Kind: "box", Size: &Position{X: 1, Y: 1, Z: 1}}}
	}
	_, err = st.ApplyMapUpdate(project.ID, MapSlotSimulation, MapUpdate{
		Generation: sim.Generation, ExpectedRevision: sim.Revision, Source: MapSourceUser,
		Entities: []MapEntity{entity("box-a", "box"), entity("zone-a", "region"),
			entity("box-b", "box"), entity("zone-b", "region")},
		Relations: []MapRelation{
			{ID: "rel-a", SubjectID: "box-a", Predicate: "inside", ObjectID: "zone-a"},
			{ID: "rel-b", SubjectID: "box-b", Predicate: "stored_in", ObjectID: "zone-b"},
		},
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	byEntity, err := st.QuerySemanticMap(project.ID, MapSlotSimulation, MapQuery{EntityID: "box-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byEntity.Entities) != 2 || len(byEntity.Relations) != 1 || byEntity.Relations[0].ID != "rel-a" {
		t.Fatalf("Entity 查询泄漏无关地图内容: entities=%+v relations=%+v", byEntity.Entities, byEntity.Relations)
	}
	byRelation, err := st.QuerySemanticMap(project.ID, MapSlotSimulation, MapQuery{Predicate: "stored_in"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byRelation.Entities) != 2 || len(byRelation.Relations) != 1 || byRelation.Relations[0].ID != "rel-b" {
		t.Fatalf("Relation 查询泄漏整图: entities=%+v relations=%+v", byRelation.Entities, byRelation.Relations)
	}
}
