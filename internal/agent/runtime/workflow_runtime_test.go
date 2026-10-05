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

package runtime

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk/middlewares/filesystem"
	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/agent/team"
	"insightos.cn/semantic-framework/internal/skill"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/workflow"
	"insightos.cn/semantic-framework/pkg/llm"
)

func newWorkflowRuntimeForTest(t *testing.T, fx *testFixture,
	model kernel.Model) *Service {
	t.Helper()
	developerDir := filepath.Join(fx.profileRoot, "developer")
	if err := os.MkdirAll(developerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(developerDir, "role.yaml"), []byte(
		"name: developer\nmode: worker\ndescription: Developer Task\nmodel: mock\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(developerDir, "AGENT.md"),
		[]byte("# Developer\n只处理当前 Task。"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := NewService(Deps{Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st,
		Bus: fx.bus, Logger: fx.logger,
		BuildModel: func(context.Context, llm.Provider, string) (kernel.Model, error) {
			return model, nil
		}})
	if err := svc.AssembleTeam(context.Background(), &team.Def{Name: "default",
		Leader:  team.MemberDef{ID: "leader", Role: "leader"},
		Members: []team.MemberDef{{ID: "developer-1", Role: "developer"}}}); err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestRobotExecutionDoesNotExposeRedundantPlanningSkillTool(t *testing.T) {
	fx := newTestFixture(t)
	robotDir := filepath.Join(fx.profileRoot, "robot")
	if err := os.MkdirAll(robotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(robotDir, "role.yaml"), []byte(
		"name: robot\nmode: worker\ndescription: Robot Task\nmodel: mock\n"+
			"skills:\n  allowlist: [depalletizing-robot-task]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(robotDir, "AGENT.md"),
		[]byte("# Robot\n只推进当前 Robot SubTask。"), 0o644); err != nil {
		t.Fatal(err)
	}

	skillsRoot := t.TempDir()
	skillDir := filepath.Join(skillsRoot, "robot", "depalletizing-robot-task")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(
		"---\nname: depalletizing-robot-task\ndescription: 拆解搬箱任务\n---\n"+
			"# ROBOT_PLANNING_SKILL_MARKER\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillStore, err := skill.NewStore(skillsRoot, fx.logger)
	if err != nil {
		t.Fatal(err)
	}
	model := kernel.NewMockChatModel()
	model.SetResponse(`{"kind":"result","summary":"done","evidence":{}}`)
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus,
		Logger: fx.logger, SkillStore: skillStore,
		BuildModel: func(context.Context, llm.Provider, string) (kernel.Model, error) {
			return model, nil
		},
	})
	project, err := fx.st.EnsureDefaultProject("usr-robot-execution-skill-boundary")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	conversation := store.ChatSession{
		ID: "cs-robot-execution-skill-boundary", UserID: project.OwnerID,
		ProjectID: project.ID, Title: "Robot execution skill boundary",
		CreatedAt: now, UpdatedAt: now,
	}
	if err = fx.st.CreateChatSession(conversation); err != nil {
		t.Fatal(err)
	}

	// Robot执行Run已经由taskExecutionPrompt直接获得当前Skill完整契约，
	// 不应再注入用于Task Planning的通用skill工具。
	executionRuntime, err := svc.buildPurposeRuntime(
		context.Background(), conversation.ID, "robot:r1pro-test",
		store.RunKindTaskExecution,
	)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := executionRuntime.runner.Run(context.Background(), nil, "执行当前步骤")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, ok := stream.Next(); !ok {
			break
		}
	}

	// 同一份Skill仍需保留在Task Planning中，避免为了修执行面而破坏四步拆解。
	planningRuntime, err := svc.buildPurposeRuntimeWithSkills(
		context.Background(), conversation.ID, "robot:r1pro-test",
		store.RunKindTaskPlanning, skillStore,
	)
	if err != nil {
		t.Fatal(err)
	}
	stream, err = planningRuntime.runner.Run(context.Background(), nil, "规划当前Task")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, ok := stream.Next(); !ok {
			break
		}
	}

	inputs := model.CallInputs()
	if len(inputs) != 2 {
		t.Fatalf("执行与规划应各调用模型一次，实际: %d", len(inputs))
	}
	joined := func(messages []*schema.Message) string {
		var value strings.Builder
		for _, message := range messages {
			if message != nil {
				value.WriteString(message.Content)
			}
		}
		return value.String()
	}
	if value := joined(inputs[0]); strings.Contains(value, "Skill 系统") ||
		strings.Contains(value, "ROBOT_PLANNING_SKILL_MARKER") {
		t.Fatalf("Robot执行Run不应暴露Planning Skill middleware: %s", value)
	}
	if value := joined(inputs[1]); !strings.Contains(value, "Skill 系统") {
		t.Fatalf("Task Planning仍应注入Skill middleware: %s", value)
	}
}

func approveRuntimeWorkflow(t *testing.T, st *store.Store, project store.Project,
	conversation store.ChatSession, draft store.WorkflowDraft, now time.Time) store.WorkflowView {
	t.Helper()
	ready, err := st.SubmitPlanProposal(project.ID, conversation.ID, draft, "", nil, "# "+draft.Goal, now)
	if err != nil {
		t.Fatal(err)
	}
	view, err := st.ApprovePlanProposal(ready.ID, ready.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	for index := range view.Tasks {
		agentID := "developer-1"
		if view.Tasks[index].RequiredRole == "robot" {
			agentID = "robot:r1pro-test"
		}
		assigned, assignErr := st.AssignTask(view.Tasks[index].ID, view.Tasks[index].Revision, agentID, "", now)
		if assignErr != nil {
			t.Fatal(assignErr)
		}
		view.Tasks[index] = assigned
	}
	return view
}

func TestTaskPlanningUsesTaskInputWithoutWorkflowMapOrLeaderHistory(t *testing.T) {
	fx := newTestFixture(t)
	developerDir := filepath.Join(fx.profileRoot, "developer")
	if err := os.MkdirAll(developerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(developerDir, "role.yaml"), []byte(
		"name: developer\nmode: worker\ndescription: Developer Task\nmodel: mock\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(developerDir, "AGENT.md"),
		[]byte("# Developer\n只处理当前 Task。"), 0o644); err != nil {
		t.Fatal(err)
	}
	model := kernel.NewMockChatModel()
	model.SetResponse(`{"summary":"按Task输入生成一个步骤","subtasks":[{"id":"sub-map","kind":"agent_step","goal":"处理已解析目标"}]}`)
	svc := NewService(Deps{Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st,
		Bus: fx.bus, Logger: fx.logger,
		BuildModel: func(context.Context, llm.Provider, string) (kernel.Model, error) {
			return model, nil
		}})
	if err := svc.AssembleTeam(context.Background(), &team.Def{Name: "default",
		Leader:  team.MemberDef{ID: "leader", Role: "leader"},
		Members: []team.MemberDef{{ID: "developer-1", Role: "developer"}}}); err != nil {
		t.Fatal(err)
	}
	project, err := fx.st.EnsureDefaultProject("usr-task-plan")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	conversation := store.ChatSession{ID: "cs-task-plan", UserID: project.OwnerID,
		ProjectID: project.ID, Title: "Task Planning", CreatedAt: now, UpdatedAt: now}
	if err := fx.st.CreateChatSession(conversation); err != nil {
		t.Fatal(err)
	}
	if err := fx.st.AppendChatMessage(store.ChatMessage{ID: store.NewChatMessageID(),
		SessionID: conversation.ID, Message: schema.UserMessage("LEADER_SECRET_HISTORY"),
		CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	mapBinding := []byte(`{"map_id":"simulation_map","generation":1,"selections":[{"kind":"point","frame_id":"world","position":[1,2,3]}]}`)
	taskInput := json.RawMessage(`{"target_ref":"slot-1","target_pose":{"frame_id":"world","position_m":[1,2,3]}}`)
	draft := store.WorkflowDraft{Goal: "使用选定地图区域完成开发", Constraints: []byte(`{"readonly":true}`),
		CompletionCriteria: []byte(`{"tests":"pass"}`), MapScope: mapBinding,
		Tasks: []store.TaskDraft{{ID: "task-map-plan", RequiredRole: "developer", Goal: "生成地图报告",
			Input: taskInput}}}
	view := approveRuntimeWorkflow(t, fx.st, project, conversation, draft, now)
	result, err := svc.PlanTaskWithResult(context.Background(), workflow.TaskPlanRequest{UserID: project.OwnerID,
		Project: project, Conversation: conversation, Workflow: view.Workflow,
		Task: store.TaskDraft{ID: view.Tasks[0].ID, RequiredRole: view.Tasks[0].RequiredRole,
			Goal: view.Tasks[0].Goal, Input: taskInput}})
	if err != nil || len(result.SubTasks) != 1 || result.Summary != "按Task输入生成一个步骤" {
		t.Fatalf("Task Planning 失败: result=%+v err=%v", result, err)
	}
	inputs := model.CallInputs()
	if len(inputs) != 1 {
		t.Fatalf("Task Planning 应只有一次模型调用: %d", len(inputs))
	}
	var contents strings.Builder
	for _, message := range inputs[0] {
		if message != nil {
			contents.WriteString(message.Content)
		}
	}
	text := contents.String()
	if !strings.Contains(text, `"target_ref":"slot-1"`) ||
		!strings.Contains(text, `"frame_id":"world"`) || !strings.Contains(text, `"goal":"生成地图报告"`) {
		t.Fatalf("Task Planning 缺少当前 Task 的稳定输入: %s", text)
	}
	if strings.Contains(text, `"map_id":"simulation_map"`) || strings.Contains(text, `"map_binding"`) {
		t.Fatalf("Task Planning 不得重新注入 Workflow Semantic Map 事实: %s", text)
	}
	if strings.Contains(text, "LEADER_SECRET_HISTORY") {
		t.Fatalf("Task Planning 不得装入 Leader Conversation 历史: %s", text)
	}
}

func TestWorkflowRuntimeAdaptersUseStrictRunsAndTaskContext(t *testing.T) {
	t.Run("developer execution", func(t *testing.T) {
		fx := newTestFixture(t)
		model := kernel.NewMockChatModel()
		model.SetResponse(`{"kind":"result","summary":"开发完成","evidence":["artifact-1"]}`)
		svc := newWorkflowRuntimeForTest(t, fx, model)
		project, err := fx.st.EnsureDefaultProject("usr-real-task")
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		conversation := store.ChatSession{ID: "cs-real-task", UserID: project.OwnerID,
			ProjectID: project.ID, Title: "Task", CreatedAt: now, UpdatedAt: now}
		if err := fx.st.CreateChatSession(conversation); err != nil {
			t.Fatal(err)
		}
		confirmed := approveRuntimeWorkflow(t, fx.st, project, conversation, store.WorkflowDraft{
			Goal: "开发", Tasks: []store.TaskDraft{{ID: "task-real", RequiredRole: "developer",
				Goal: "实现代码", Input: json.RawMessage(`{"file":"main.go"}`),
				SubTasks: []store.SubTaskDraft{{ID: "sub-real", Kind: "agent_step", Goal: "编码"}}}}}, now)
		task, err := fx.st.TransitionTask(confirmed.Tasks[0].ID, confirmed.Tasks[0].Revision,
			store.TaskStatusRunning, "", "", nil, now)
		if err != nil {
			t.Fatal(err)
		}
		run := store.RunSession{ID: "run-real-developer", ProjectID: project.ID,
			ChatSessionID: conversation.ID, WorkflowID: confirmed.Workflow.ID, TaskID: task.ID,
			Kind: store.RunKindTaskExecution, ContextID: task.ContextID,
			AgentID: task.AssignedAgentID, AgentName: task.AssignedAgentID, Status: store.RunStatusRunning,
			StartedAt: now, UpdatedAt: now}
		if err := fx.st.CreateRunSession(run); err != nil {
			t.Fatal(err)
		}
		result, err := svc.ExecuteTask(context.Background(), workflow.TaskExecution{
			UserID: project.OwnerID, Project: project, Conversation: conversation,
			Workflow: confirmed.Workflow, Task: task, SubTasks: confirmed.SubTasks, Run: run,
		})
		if err != nil || result.Summary != "开发完成" {
			t.Fatalf("真实 Developer Task adapter 失败: result=%+v err=%v", result, err)
		}
		storedRun, err := fx.st.GetRunSession(run.ID)
		if err != nil || storedRun.ID != run.ID || storedRun.TraceID == "" {
			t.Fatalf("Developer 必须接管预留 Run，而非创建第二条: %+v %v", storedRun, err)
		}
		messages, err := fx.st.ListContextMessagesAfter(task.ContextID, "", 0)
		if err != nil || len(messages) != 2 || messages[0].RunID != run.ID || messages[1].RunID != run.ID {
			t.Fatalf("Developer 输入输出应写入独立 Task Context: %+v %v", messages, err)
		}
		conversationMessages, err := fx.st.ListChatMessages(conversation.ID, 0, 0)
		if err != nil || len(conversationMessages) != 0 {
			t.Fatalf("Developer Task 不得写入 Leader Conversation: %+v %v", conversationMessages, err)
		}
	})
}

func TestRobotTaskPlanningCatalogUsesAssignedPilotSkills(t *testing.T) {
	fx := newTestFixture(t)
	now := time.Now().UTC()
	archivePath := filepath.Join(t.TempDir(), "grasp-object.zip")
	output, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(output)
	manifest, err := writer.Create("grasp-object/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	_, err = manifest.Write([]byte(`---
name: grasp-object
description: 抓取并验证目标
category: robot_skill
version: 0.1.0
runtime:
  input_model: scripts.models:GraspObjectInput
  result_model: scripts.models:GraspObjectResult
required_actions:
  - { type: gripper.close, schema_version: 1 }
stop_actions:
  - { type: gripper.hold_object, schema_version: 1 }
---
# 抓取语义
只有独立验证持物后才能完成。
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fx.st.SaveRobotSkillPackage(store.RobotSkillPackage{
		Name: "grasp-object", Version: "0.1.0", Description: "抓取并验证目标",
		Category: "robot_skill", PackagePath: archivePath,
		RequiredActions: []map[string]any{{"type": "gripper.close", "schema_version": 1}},
		StopActions:     []map[string]any{{"type": "gripper.hold_object", "schema_version": 1}},
		PublishedAt:     now,
	}); err != nil {
		t.Fatal(err)
	}
	pilot := store.RobotPilot{PilotInstanceID: "pilot-r1pro-02", RobotID: "r1pro-fake-02",
		RobotModel: "r1pro", Backend: "fake", Status: "online", RobotStatus: "idle",
		AbilityFrameworkStatus: "ready", LastSeenAt: now,
		Configuration: map[string]any{"sdk": map[string]any{"options": map[string]any{
			"base_footprint_radius_m":      0.42,
			"navigation_resolution_m":      0.05,
			"manipulation_work_distance_m": 0.55,
		}}}}
	if err := fx.st.SaveRobotPilot(pilot); err != nil {
		t.Fatal(err)
	}
	if err := fx.st.SaveRobotPilotSkill(store.RobotPilotSkill{
		PilotInstanceID: pilot.PilotInstanceID, Name: "grasp-object", Version: "0.1.0",
		Enabled: true, Status: "installed", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(Deps{Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st,
		Bus: fx.bus, Logger: fx.logger})
	assignment, err := svc.resolveRobotTaskAssignment(store.Workflow{ApprovedScope: json.RawMessage(`{"robot_ids":["r1pro-fake-02"],"robot_models":["r1pro"],"allowed_skills":["grasp-object"]}`)}, store.Task{RequiredRole: "robot"})
	if err != nil || assignment.RobotID != "r1pro-fake-02" {
		t.Fatalf("Robot 后绑定必须同时满足批准范围和实时状态: assignment=%+v err=%v", assignment, err)
	}
	if _, err = svc.resolveRobotTaskAssignment(store.Workflow{ApprovedScope: json.RawMessage(`{"robot_ids":["another-robot"]}`)}, store.Task{RequiredRole: "robot"}); !errors.Is(err, workflow.ErrTaskWaitingResource) {
		t.Fatalf("批准范围外的在线 Robot 不得被分配: %v", err)
	}
	catalog, err := svc.robotTaskPlanningCatalog(store.Workflow{ApprovedScope: json.RawMessage(`{"allowed_skills":["grasp-object"]}`)}, store.Task{
		ID: "task-move", RequiredRole: "robot",
		AssignedRobotID: "r1pro-fake-02",
	})
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Robot.ID != "r1pro-fake-02" || len(catalog.Skills) != 1 ||
		catalog.Skills[0].Name != "grasp-object" ||
		catalog.Robot.BaseFootprintRadiusM != 0.42 ||
		catalog.Robot.NavigationResolutionM != 0.05 ||
		catalog.Robot.ManipulationWorkDistanceM != 0.55 ||
		catalog.Skills[0].InputModel != "scripts.models:GraspObjectInput" ||
		catalog.Skills[0].Documentation != "" {
		t.Fatalf("Robot Task 常驻目录应包含实际Robot与紧凑Skill契约，不应注入完整正文: %+v", catalog)
	}

	project, err := fx.st.EnsureDefaultProject("usr-robot-execution-context")
	if err != nil {
		t.Fatal(err)
	}
	conversation := store.ChatSession{ID: "cs-robot-execution-context", UserID: project.OwnerID,
		ProjectID: project.ID, Title: "Robot execution context", CreatedAt: now, UpdatedAt: now}
	if err := fx.st.CreateChatSession(conversation); err != nil {
		t.Fatal(err)
	}
	proposal, err := fx.st.SubmitPlanProposal(project.ID, conversation.ID,
		store.WorkflowDraft{Goal: "搬运", Tasks: []store.TaskDraft{{
			ID: "task-robot-execution-context", RequiredRole: "robot", Goal: "搬运 box-17",
		}}}, "", json.RawMessage(`{"allowed_skills":["grasp-object"]}`), "# 搬运", now)
	if err != nil {
		t.Fatal(err)
	}
	workflowView, err := fx.st.ApprovePlanProposal(proposal.ID, proposal.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	task, err := fx.st.AssignTask(workflowView.Tasks[0].ID, workflowView.Tasks[0].Revision,
		"robot:r1pro-fake-02", "r1pro-fake-02", now)
	if err != nil {
		t.Fatal(err)
	}
	task, subTasks, err := fx.st.SetTaskSubTasks(task.ID, task.Revision, []store.SubTaskDraft{
		{ID: "sub-grasp-completed", Kind: "robot_skill", Goal: "抓取 box-17",
			Spec: json.RawMessage(`{"skill_name":"grasp-object","skill_version":"0.1.0","input":{"object_ref":"object://box-17"}}`)},
		{ID: "sub-place-current", Kind: "robot_skill", Goal: "放置 box-17",
			Spec:      json.RawMessage(`{"skill_name":"grasp-object","skill_version":"0.1.0","input":{}}`),
			DependsOn: []string{"sub-grasp-completed"}},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	first, err := fx.st.TransitionSubTask(subTasks[0].ID, subTasks[0].Revision,
		store.TaskStatusRunning, "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fx.st.TransitionSubTask(first.ID, first.Revision, store.TaskStatusCompleted, "",
		json.RawMessage(`{"held_object":{"object_ref":"object://box-17","grasp_candidate_id":"candidate-2"}}`), now); err != nil {
		t.Fatal(err)
	}
	prompt, err := svc.taskExecutionPrompt(workflow.TaskExecution{
		Project: project, Conversation: conversation, Workflow: workflowView.Workflow,
		Task: task, SubTasks: []store.SubTask{subTasks[1]},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`"completed_subtasks"`, `"result_summary":"抓取 box-17已完成"`,
		`"execution_id":""`,
		`"current_skill_contract"`, `"input_model":"scripts.models:GraspObjectInput"`,
		`"base_footprint_radius_m":0.42`, `"navigation_resolution_m":0.05`,
		`"manipulation_work_distance_m":0.55`,
		`"execution_guidance"`,
		"地图信息不能替代Skill要求的执行期物理验证",
		"同一Run中参数和revision未变化的查询必须复用",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("Robot 执行上下文缺少 %q: %s", required, prompt)
		}
	}
	if strings.Contains(prompt, `"grasp_candidate_id":"candidate-2"`) {
		t.Fatalf("Task 执行上下文不应复制完整前序 Robot Skill 结果: %s", prompt)
	}
}

func TestTaskExecutionGuidanceUsesProjectAgentSkills(t *testing.T) {
	fx := newTestFixture(t)
	skills, err := skill.NewStore("../../../configs/skills", fx.logger)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(Deps{Profiles: newLoaderForRoot(t, "../../../configs/agents"), Store: fx.st, SkillStore: skills})
	project, err := fx.st.EnsureDefaultProject("user-guidance")
	if err != nil {
		t.Fatal(err)
	}
	guidance, err := svc.taskExecutionGuidance(project.ID, "robot")
	if err != nil {
		t.Fatal(err)
	}
	current, ok := skills.Get("depalletizing-robot-task")
	if !ok || strings.TrimSpace(current.Body) == "" {
		t.Fatal("project task skill is missing or empty")
	}
	// Verify injection of the installed application knowledge, not wording from
	// one particular revision of a user-editable Skill.
	for _, required := range []string{"Agent Skill: depalletizing-robot-task", current.Body} {
		if !strings.Contains(guidance, required) {
			t.Fatalf("Agent Skill knowledge missing %q", required)
		}
	}
	// 角色及 Project 的既有 allowlist 限定应用知识，不在 Framework 内选择场景。
	if strings.Contains(guidance, "Agent Skill: depalletizing-workflow-planning") {
		t.Fatal("another role's skill injected")
	}
}

func TestRobotTaskExecutionCorrectsFabricatedAcceptedResultInSameRun(t *testing.T) {
	t.Run("fabricated-accepted", func(t *testing.T) {
		testRobotExecutionCorrectionBoundary(t, []kernel.MockReply{
			{Content: `{"kind":"result","summary":"已accepted","evidence":{"execution_id":"rex-fabricated"}}`},
			{Content: `{"kind":"result","summary":"已accepted","evidence":{"execution_id":"rex-fabricated-again"}}`},
		})
	})
	t.Run("call-shaped-text", func(t *testing.T) {
		testRobotExecutionCorrectionBoundary(t, []kernel.MockReply{
			{Content: `{"kind":"call","calls":[{"name":"robot_run","arguments":{}}]}`},
			{Content: `{"kind":"call","calls":[{"name":"robot_run","arguments":{}}]}`},
			{Content: `{"kind":"call","calls":[{"name":"robot_run","arguments":{}}]}`},
		})
	})
	t.Run("rejected-tool-then-text-must-not-report-stale-error", func(t *testing.T) {
		testRobotExecutionCorrectionBoundary(t, []kernel.MockReply{
			{ToolCalls: []schema.ToolCall{{ID: "bad-input", Type: "function", Function: schema.FunctionCall{Name: "robot_run", Arguments: `{}`}}}},
			{Content: `{"kind":"call","calls":[{"name":"robot_run","arguments":{}}]}`},
			{Content: `{"kind":"call","calls":[{"name":"robot_run","arguments":{}}]}`},
		})
	})
}

func testRobotExecutionCorrectionBoundary(t *testing.T, replies []kernel.MockReply) {
	t.Helper()
	fx := newTestFixture(t)
	now := time.Now().UTC()
	archivePath := filepath.Join(t.TempDir(), "place-object.zip")
	output, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(output)
	manifest, err := writer.Create("place-object/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manifest.Write([]byte(`---
name: place-object
description: 放置并验证目标
category: robot_skill
version: 0.4.0
runtime:
  input_model: scripts.models:PlaceObjectInput
  result_model: scripts.models:PlacedObjectState
required_actions:
  - { type: robot.get_state, schema_version: 2 }
stop_actions:
  - { type: gripper.hold_object, schema_version: 2 }
---
# 放置
`)); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err = output.Close(); err != nil {
		t.Fatal(err)
	}
	if err = fx.st.SaveRobotSkillPackage(store.RobotSkillPackage{
		Name: "place-object", Version: "0.4.0", Description: "放置并验证目标",
		Category: "robot_skill", PackagePath: archivePath,
		RequiredActions: []map[string]any{{"type": "robot.get_state", "schema_version": 2}},
		StopActions:     []map[string]any{{"type": "gripper.hold_object", "schema_version": 2}},
		PublishedAt:     now,
	}); err != nil {
		t.Fatal(err)
	}
	pilot := store.RobotPilot{PilotInstanceID: "pilot-correction", RobotID: "r1pro-correction",
		RobotModel: "r1pro", Backend: "fake", Status: "online", RobotStatus: "idle",
		AbilityFrameworkStatus: "ready", LastSeenAt: now}
	if err = fx.st.SaveRobotPilot(pilot); err != nil {
		t.Fatal(err)
	}
	if err = fx.st.SaveRobotPilotSkill(store.RobotPilotSkill{
		PilotInstanceID: pilot.PilotInstanceID, Name: "place-object", Version: "0.4.0",
		Enabled: true, Status: "installed", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	robotProfileDir := filepath.Join(fx.profileRoot, "robot")
	if err = os.MkdirAll(robotProfileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(robotProfileDir, "role.yaml"), []byte(
		"name: robot\nmode: worker\ndescription: Robot Task\nmodel: mock\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(robotProfileDir, "AGENT.md"),
		[]byte("# Robot\n只处理当前 Robot Task。"), 0o644); err != nil {
		t.Fatal(err)
	}
	model := kernel.NewMockChatModel()
	model.SetScript(replies...)
	svc := newWorkflowRuntimeForTest(t, fx, model)
	project, err := fx.st.EnsureDefaultProject("usr-robot-run-correction")
	if err != nil {
		t.Fatal(err)
	}
	conversation := store.ChatSession{ID: "cs-robot-run-correction", UserID: project.OwnerID,
		ProjectID: project.ID, Title: "Robot run correction", CreatedAt: now, UpdatedAt: now}
	if err = fx.st.CreateChatSession(conversation); err != nil {
		t.Fatal(err)
	}
	proposal, err := fx.st.SubmitPlanProposal(project.ID, conversation.ID,
		store.WorkflowDraft{Goal: "放置箱体", Tasks: []store.TaskDraft{{
			ID: "task-robot-run-correction", RequiredRole: "robot", Goal: "放置 box-1",
		}}}, "", json.RawMessage(`{"allowed_skills":["place-object"]}`), "# 放置", now)
	if err != nil {
		t.Fatal(err)
	}
	view, err := fx.st.ApprovePlanProposal(proposal.ID, proposal.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	task, err := fx.st.AssignTask(view.Tasks[0].ID, view.Tasks[0].Revision,
		"robot:r1pro-correction", pilot.RobotID, now)
	if err != nil {
		t.Fatal(err)
	}
	task, subtasks, err := fx.st.SetTaskSubTasks(task.ID, task.Revision, []store.SubTaskDraft{{
		ID: "sub-place-correction", Kind: "robot_skill", Goal: "放置 box-1",
		Spec: json.RawMessage(`{"skill_name":"place-object","skill_version":"0.4.0","intent":{"object_ref":"box-1","target_ref":"slot-1"}}`),
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	run := store.RunSession{ID: "run-robot-run-correction", ProjectID: project.ID,
		ChatSessionID: conversation.ID, WorkflowID: view.Workflow.ID, TaskID: task.ID,
		Kind: store.RunKindTaskExecution, ContextID: task.ContextID,
		AgentID: task.AssignedAgentID, AgentName: task.AssignedAgentID,
		Status: store.RunStatusRunning, StartedAt: now, UpdatedAt: now}
	if err = fx.st.CreateRunSession(run); err != nil {
		t.Fatal(err)
	}
	svc.SetRobotSkillContracts(testSkillContracts{})
	_, err = svc.ExecuteTask(context.Background(), workflow.TaskExecution{
		UserID: project.OwnerID, Project: project, Conversation: conversation,
		Workflow: view.Workflow, Task: task, SubTasks: []store.SubTask{subtasks[0]}, Run: run,
	})
	if err == nil || !strings.Contains(err.Error(), "本Run没有产生可关联的robot.run执行记录") {
		t.Fatalf("重复伪造accepted必须在同一Run终止: %v", err)
	}
	if !strings.Contains(err.Error(), "ROBOT_TOOL_CALL_REQUIRED") || strings.Contains(err.Error(), "最后工具错误") {
		t.Fatalf("必须报告本轮未发起有效调用，而非历史工具错误: %v", err)
	}
	if _, lookupErr := fx.st.GetRobotExecutionBySubTask(subtasks[0].ID); !errors.Is(lookupErr, store.ErrNotFound) {
		t.Fatalf("不能将普通文本自动转换成物理执行: %v", lookupErr)
	}
	inputs := model.CallInputs()
	if len(inputs) != 3 {
		t.Fatalf("没有真实Execution时最多纠正两次: calls=%d", len(inputs))
	}
	var correction strings.Builder
	for _, message := range inputs[1] {
		if message != nil {
			correction.WriteString(message.Content)
		}
	}
	if !strings.Contains(correction.String(), "没有持久Robot Execution") ||
		!strings.Contains(correction.String(), "不得编造execution_id") {
		t.Fatalf("纠正必须明确以Store事实覆盖模型自报结果: %s", correction.String())
	}
	lastMessages := inputs[len(inputs)-1]
	lastCorrection := lastMessages[len(lastMessages)-1].Content
	if !strings.Contains(lastCorrection, "ROBOT_TOOL_CALL_REQUIRED") || !strings.Contains(lastCorrection, "原生工具调用") || strings.Contains(lastCorrection, "本次未建立 Execution 的工具错误：") {
		t.Fatalf("本轮文本错误必须得到专门反馈，而非历史字段错误: %s", lastCorrection)
	}
}

func TestTaskContextMarksSummaryHistoricalAndUsesLiveRobotSnapshot(t *testing.T) {
	fx := newTestFixture(t)
	project, err := fx.st.EnsureDefaultProject("usr-context-priority")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	conversation := store.ChatSession{ID: "cs-context-priority", UserID: project.OwnerID,
		ProjectID: project.ID, Title: "Context priority", CreatedAt: now, UpdatedAt: now}
	if err := fx.st.CreateChatSession(conversation); err != nil {
		t.Fatal(err)
	}
	proposal, err := fx.st.SubmitPlanProposal(project.ID, conversation.ID,
		store.WorkflowDraft{Goal: "搬运", Tasks: []store.TaskDraft{{
			ID: "task-context-robot", RequiredRole: "robot", Goal: "搬运 box-17",
		}}}, "", nil, "# 搬运", now)
	if err != nil {
		t.Fatal(err)
	}
	view, err := fx.st.ApprovePlanProposal(proposal.ID, proposal.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	task, err := fx.st.AssignTask(view.Tasks[0].ID, view.Tasks[0].Revision,
		"robot:r1pro-fake-02", "r1pro-fake-02", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.st.SaveContextSummary(store.ContextSummary{ContextID: task.ContextID,
		ProjectID: project.ID, SessionID: conversation.ID, TaskID: task.ID,
		Summary: "历史记录曾写 Robot idle"}, 0); err != nil {
		t.Fatal(err)
	}
	if err := fx.st.SaveRobotPilot(store.RobotPilot{PilotInstanceID: "pilot-context",
		RobotID: "r1pro-fake-02", RobotModel: "r1pro", Backend: "fake",
		Status: "online", RobotStatus: "busy", AbilityFrameworkStatus: "ready",
		CurrentExecutionID: "rex-current", LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(Deps{Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st,
		Bus: fx.bus, Logger: fx.logger})
	execution := workflow.TaskExecution{Project: project, Conversation: conversation,
		Workflow: view.Workflow, Task: task, SubTasks: []store.SubTask{{
			ID: "sub-current", TaskID: task.ID, Kind: "robot_skill", Goal: "导航",
		}}}
	messages, err := svc.loadTaskContext(execution)
	if err != nil || len(messages) != 1 ||
		!strings.Contains(messages[0].Content, "历史摘要") ||
		!strings.Contains(messages[0].Content, "当前状态以本轮结构化状态和实时资源为准") {
		t.Fatalf("摘要必须明确标记为历史来源: messages=%+v err=%v", messages, err)
	}
	prompt, err := svc.taskExecutionPrompt(execution)
	if err != nil || !strings.Contains(prompt, `"robot_status":"busy"`) ||
		!strings.Contains(prompt, `"current_execution_id":"rex-current"`) {
		t.Fatalf("实时 Robot Snapshot 必须覆盖历史叙述: prompt=%s err=%v", prompt, err)
	}
	if err := fx.st.AppendContextMessage(store.ContextMessage{
		ID: store.NewChatMessageID(), ContextID: task.ContextID, ProjectID: project.ID,
		SessionID: conversation.ID, TaskID: task.ID, AgentID: task.AssignedAgentID,
		Message:   schema.UserMessage("STALE_FULL_SKILL_DOCUMENT_AND_ROBOT_RUN_INPUT"),
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	lean, err := svc.loadRobotTaskExecutionContext(execution)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range lean {
		if message != nil && strings.Contains(message.Content,
			"STALE_FULL_SKILL_DOCUMENT_AND_ROBOT_RUN_INPUT") {
			t.Fatalf("Robot Task Execution不得重复加载历史Skill文档与robot.run参数: %s",
				message.Content)
		}
	}
}

func TestPlanningToolPolicyFailsClosedForSideEffects(t *testing.T) {
	blocked := []string{"execute", "execute_host", "artifact.put", "skill.run", "mcp.github.create_issue"}
	for _, name := range blocked {
		t.Run(name, func(t *testing.T) {
			called := false
			result, err := (planningToolPolicy{}).WrapToolCall(context.Background(),
				kernel.ToolCallMeta{Name: name}, `{}`, func(context.Context, string) (string, error) {
					called = true
					return "should-not-run", nil
				})
			if err != nil || called || !strings.Contains(result, "不允许") {
				t.Fatalf("Planning 副作用工具必须 fail-closed: result=%s called=%v err=%v", result, called, err)
			}
		})
	}
	for _, name := range []string{"system.time", "artifact.get", "artifact.list",
		filesystem.ToolNameLs, filesystem.ToolNameReadFile, filesystem.ToolNameGlob, filesystem.ToolNameGrep} {
		t.Run("block "+name, func(t *testing.T) {
			called := false
			result, err := (planningToolPolicy{}).WrapToolCall(context.Background(),
				kernel.ToolCallMeta{Name: name}, `{}`, func(context.Context, string) (string, error) {
					called = true
					return "should-not-run", nil
				})
			if err != nil || called || !strings.Contains(result, "不允许") {
				t.Fatalf("Task Planning 不得探索外部事实: result=%s called=%v err=%v", result, called, err)
			}
		})
	}
	for _, name := range []string{planningSkillTool, "map.query", "interaction.ask"} {
		t.Run("allow "+name, func(t *testing.T) {
			called := false
			_, err := (planningToolPolicy{}).WrapToolCall(context.Background(),
				kernel.ToolCallMeta{Name: name}, `{}`, func(context.Context, string) (string, error) {
					called = true
					return "ok", nil
				})
			if err != nil || !called {
				t.Fatalf("Planning 绑定的 Agent Skill 应放行: called=%v err=%v", called, err)
			}
		})
	}
	decisionPolicy := taskDecisionToolPolicy{allowed: map[string]struct{}{
		planningSkillTool: {}, "robot.get": {}, "map.query": {}, "interaction.ask": {},
	}}
	calledMap := false
	if _, err := decisionPolicy.WrapToolCall(context.Background(),
		kernel.ToolCallMeta{Name: "map.query"}, `{}`, func(context.Context, string) (string, error) {
			calledMap = true
			return "ok", nil
		}); err != nil || !calledMap {
		t.Fatalf("局部Robot决策应允许只读Map查询以替换无效目标: called=%v err=%v", calledMap, err)
	}
	for _, name := range []string{"robot.run", "robot.stop", "execute"} {
		t.Run("decision blocks "+name, func(t *testing.T) {
			called := false
			result, err := decisionPolicy.WrapToolCall(context.Background(),
				kernel.ToolCallMeta{Name: name}, `{}`, func(context.Context, string) (string, error) {
					called = true
					return "should-not-run", nil
				})
			if err != nil || called || !strings.Contains(result, "不允许") {
				t.Fatalf("局部Robot决策不得扩大为物理动作: result=%s called=%v err=%v",
					result, called, err)
			}
		})
	}
}

func TestContractToolPolicyCountsFeedbackRoundsNotCalls(t *testing.T) {
	policy := &contractToolPolicy{next: conversationPlanToolPolicy{}, budget: newContractCorrectionBudget(10)}
	failure := func(context.Context, string) (string, error) {
		return `{"ok":false,"error":{"code":"BAD_ARGUMENTS","message":"参数不是合法 JSON","retryable":false}}`, nil
	}
	meta := kernel.ToolCallMeta{Name: "plan.suggest"}

	for round := 0; round < 3; round++ {
		for call := 0; call < 2; call++ {
			if _, err := policy.WrapToolCall(context.Background(), meta, `{bad`, failure); err != nil {
				t.Fatalf("同轮工具必须全部返回反馈: %v", err)
			}
		}
		err := policy.BeforeModelRound(context.Background())
		if (err != nil) != (round == 2) {
			t.Fatalf("round=%d err=%v", round, err)
		}
	}
}

func TestContractToolPolicySuccessDoesNotEraseSiblingFailure(t *testing.T) {
	policy := &contractToolPolicy{next: planningToolPolicy{}, budget: newContractCorrectionBudget(10)}
	meta := kernel.ToolCallMeta{Name: "map.query"}
	failure := func(context.Context, string) (string, error) {
		return `{"ok":false,"error":{"code":"BAD_ARGUMENTS","message":"缺少查询条件","retryable":false}}`, nil
	}
	success := func(context.Context, string) (string, error) {
		return `{"ok":true,"data":{"entities":[]}}`, nil
	}

	if _, err := policy.WrapToolCall(context.Background(), meta, `{}`, failure); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.WrapToolCall(context.Background(), meta, `{}`, success); err != nil {
		t.Fatal(err)
	}
	if err := policy.BeforeModelRound(context.Background()); err != nil || policy.budget.used != 1 {
		t.Fatalf("同轮成功不能掩盖失败: budget=%d err=%v", policy.budget.used, err)
	}
	if _, err := policy.WrapToolCall(context.Background(), meta, `{}`, success); err != nil {
		t.Fatal(err)
	}
	if err := policy.BeforeModelRound(context.Background()); err != nil || policy.budget.used != 1 {
		t.Fatalf("无错误的轮次不消耗纠正预算: budget=%d err=%v", policy.budget.used, err)
	}
}

func TestMissingRobotExecutionErrorKeepsLastToolFailure(t *testing.T) {
	failure := parsePurposeToolFailure("robot.run",
		`{"ok":false,"error":{"code":"ROBOT_REQUEST_CONFLICT","message":"request_key 已用于不同的 Robot Skill 请求","retryable":false}}`)
	if failure == nil {
		t.Fatal("必须解析结构化工具错误")
	}
	err := missingRobotExecutionError(failure)
	for _, required := range []string{"robot.run", "ROBOT_REQUEST_CONFLICT", "request_key 已用于不同的 Robot Skill 请求"} {
		if !strings.Contains(err.Error(), required) {
			t.Fatalf("Task错误缺少真实工具诊断%q: %v", required, err)
		}
	}
	if strings.Contains(err.Error(), "记录不存在") {
		t.Fatalf("真实工具错误不能再被Store查询结果覆盖: %v", err)
	}
}

func TestRobotDecisionUsesTaskExecutionRunAndCompactCurrentContext(t *testing.T) {
	fx := newTestFixture(t)
	model := kernel.NewMockChatModel()
	model.SetResponse(`{"choice":"refresh_target"}`)
	svc := newWorkflowRuntimeForTest(t, fx, model)
	project, err := fx.st.EnsureDefaultProject("usr-robot-decision")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	conversation := store.ChatSession{ID: "cs-robot-decision", UserID: project.OwnerID,
		ProjectID: project.ID, Title: "Robot decision", CreatedAt: now, UpdatedAt: now}
	if err := fx.st.CreateChatSession(conversation); err != nil {
		t.Fatal(err)
	}
	view := approveRuntimeWorkflow(t, fx.st, project, conversation, store.WorkflowDraft{
		Goal: "处理局部抓取偏差", Tasks: []store.TaskDraft{{
			ID: "task-robot-decision", RequiredRole: "developer", Goal: "保持当前执行安全",
		}},
	}, now)
	task := view.Tasks[0]
	if err := fx.st.AppendContextMessage(store.ContextMessage{ContextID: task.ContextID,
		ProjectID: project.ID, SessionID: conversation.ID, TaskID: task.ID,
		AgentID: task.AssignedAgentID, Message: schema.UserMessage("STALE_ROBOT_RUN_PROMPT"),
		CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	decision, err := svc.ResolveRobotAgentRequest(context.Background(), workflow.RobotAgentDecisionRequest{
		UserID: project.OwnerID, Project: project, Conversation: conversation,
		Workflow: view.Workflow, Task: task,
		SubTask:   store.SubTask{ID: "sub-decision", TaskID: task.ID, Goal: "局部决策"},
		Execution: store.RobotExecution{ID: "rex-decision", SkillName: "semantic-navigation"},
		Event: map[string]any{"stage": "plan_route", "reason": "终点位于障碍物中",
			"response_model": "NavigationAgentDecision", "response_schema": map[string]any{
				"type": "object", "required": []any{"choice"},
				"properties": map[string]any{"choice": map[string]any{"enum": []any{"refresh_target"}}}}},
	})
	if err != nil || decision["choice"] != "refresh_target" {
		t.Fatalf("Robot Decision失败: decision=%+v err=%v", decision, err)
	}
	runs, _, err := fx.st.ListRunSessions(store.RunFilter{ChatSessionID: conversation.ID}, 0, 0)
	if err != nil || len(runs) != 1 || runs[0].Kind != store.RunKindTaskExecution {
		t.Fatalf("Robot Decision必须归入Task Execution而非Planning: runs=%+v err=%v", runs, err)
	}
	inputs := model.CallInputs()
	if len(inputs) != 1 {
		t.Fatalf("局部Decision应只执行一次明确模型请求: %d", len(inputs))
	}
	var decisionPrompt strings.Builder
	for _, message := range inputs[0] {
		if message != nil {
			decisionPrompt.WriteString(message.Content)
		}
		if message != nil && strings.Contains(message.Content, "STALE_ROBOT_RUN_PROMPT") {
			t.Fatalf("Robot Decision不应重新加载旧robot.run提示: %s", message.Content)
		}
	}
	for _, required := range []string{
		"终点位于障碍物中", "response_schema明确允许替换目标",
		"依据当前Skill契约补充缺失的目标信息",
		"相同查询结果必须复用",
	} {
		if !strings.Contains(decisionPrompt.String(), required) {
			t.Fatalf("Robot Decision提示缺少 %q: %s", required, decisionPrompt.String())
		}
	}
}

func TestRobotPhysicalDecisionPromptDoesNotAssumeNavigation(t *testing.T) {
	prompt := buildRobotAgentRequestPrompt([]byte(`{"skill_name":"place-object"}`), false)
	for _, forbidden := range []string{"使用replace_target", "不得用相同参数重复map.query"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("放置checkpoint不应继承导航决策指令 %q: %s", forbidden, prompt)
		}
	}
	for _, required := range []string{"response_schema", "allowed_decisions", "原样复制 context.plan_revision", "绝不能自行递增", "不要把抓取、放置或姿态恢复问题假设成导航问题"} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("局部物理决策提示缺少 %q: %s", required, prompt)
		}
	}
}

func TestDecodeWorkflowDraftAcceptsTypedWrapperOnly(t *testing.T) {
	direct, err := decodeWorkflowDraft(`{"goal":"直接格式","tasks":[]}`)
	if err != nil || direct.Goal != "直接格式" {
		t.Fatalf("直接 WorkflowDraft 应被接受: draft=%+v err=%v", direct, err)
	}
	wrapped, err := decodeWorkflowDraft(`{"workflow":{"goal":"包装格式","tasks":[]}}`)
	if err != nil || wrapped.Goal != "包装格式" {
		t.Fatalf("单层 workflow 包装应被接受: draft=%+v err=%v", wrapped, err)
	}
	if _, err := decodeWorkflowDraft("```json\n{\"goal\":\"禁止 Markdown\"}\n```"); err == nil {
		t.Fatal("Markdown 代码围栏不得被猜测为 WorkflowDraft")
	}
	if _, err := decodeWorkflowDraft(`{"workflow":{"goal":"非法字段","extra":true}}`); err == nil {
		t.Fatal("包装对象内部未知字段必须拒绝")
	}
}

func TestTaskPlanningCorrectsInvalidJSONWithinSameRun(t *testing.T) {
	fx := newTestFixture(t)
	model := kernel.NewMockChatModel()
	model.SetScript(
		kernel.MockReply{Content: `{"summary":"非法输出","subtasks":[{"id":"sub-invalid","kind":"agent_step","goal":"执行","spec":{"instruction":"执行","匿名说明"}}]}`},
		kernel.MockReply{Content: `{"summary":"生成一个开发步骤","subtasks":[{"id":"sub-valid","kind":"agent_step","goal":"执行","spec":{"instruction":"执行"},"completion_criteria":{"done":true},"depends_on":[]}]}`},
	)
	svc := newWorkflowRuntimeForTest(t, fx, model)
	project, err := fx.st.EnsureDefaultProject("usr-task-correction")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	conversation := store.ChatSession{ID: "cs-task-correction", UserID: project.OwnerID,
		ProjectID: project.ID, Title: "Task correction", CreatedAt: now, UpdatedAt: now}
	if err := fx.st.CreateChatSession(conversation); err != nil {
		t.Fatal(err)
	}
	view := approveRuntimeWorkflow(t, fx.st, project, conversation, store.WorkflowDraft{
		Goal: "执行开发步骤", Tasks: []store.TaskDraft{{
			ID: "task-correction", RequiredRole: "developer", Goal: "完成一步开发工作",
		}},
	}, now)
	items, err := svc.PlanTask(context.Background(), workflow.TaskPlanRequest{
		UserID: project.OwnerID, Project: project, Conversation: conversation,
		Workflow: view.Workflow, Task: store.TaskDraft{
			ID: view.Tasks[0].ID, RequiredRole: view.Tasks[0].RequiredRole,
			Goal: view.Tasks[0].Goal,
		},
	})
	if err != nil || len(items) != 1 || items[0].ID != "sub-valid" {
		t.Fatalf("同一Run内格式纠正应返回后续合法计划: items=%+v err=%v", items, err)
	}
	if calls := model.CallInputs(); len(calls) != 2 {
		t.Fatalf("首次错误后应在同一Run继续一次模型交换: calls=%d", len(calls))
	} else {
		var prompt strings.Builder
		for _, message := range calls[1] {
			if message != nil {
				prompt.WriteString(message.Content)
			}
		}
		if !strings.Contains(prompt.String(), "纠正次数有上限") || !strings.Contains(prompt.String(), "byte_offset=") {
			t.Fatalf("后续交换缺少纠正上限与定位信息: %s", prompt.String())
		}
	}
	runs, _, err := fx.st.ListRunSessions(store.RunFilter{ChatSessionID: conversation.ID}, 0, 0)
	if err != nil || len(runs) != 1 || runs[0].Kind != store.RunKindTaskPlanning {
		t.Fatalf("结构纠正不得拆成多个Planning Run: runs=%+v err=%v", runs, err)
	}
}

func TestRobotTaskPlanSpecUsesOnlyInstalledSkillContract(t *testing.T) {
	task := store.Task{RequiredRole: "robot"}
	catalog := robotTaskPlanningView{Skills: []robotTaskSkillView{{
		Name: "grasp-object", Version: "0.3.0",
	}}}
	valid := `{"summary":"抓取目标","subtasks":[{"id":"sub-grasp","kind":"robot_skill","goal":"抓取","spec":{"skill_name":"grasp-object","skill_version":"0.3.0","intent":{"object_ref":"tote-1"}},"completion_criteria":{"held":true},"depends_on":[]}]}`
	items, err := decodeAndValidateTaskPlan(valid, task, catalog)
	if err != nil || len(items) != 1 {
		t.Fatalf("合法 Robot SubTask 应通过: items=%+v err=%v", items, err)
	}
	withExtra := `{"summary":"非法扩展","subtasks":[{"id":"sub-grasp","kind":"robot_skill","goal":"抓取","spec":{"skill_name":"grasp-object","skill_version":"0.3.0","intent":{},"constraints":{}}}]}`
	if _, err := decodeAndValidateTaskPlan(withExtra, task, catalog); err == nil {
		t.Fatal("Robot SubTask spec 不得扩展自由 constraints")
	}
	unknown := `{"summary":"未知技能","subtasks":[{"id":"sub-fake","kind":"robot_skill","goal":"执行","spec":{"skill_name":"invented-skill","skill_version":"9.9.9","intent":{}}}]}`
	if _, err := decodeAndValidateTaskPlan(unknown, task, catalog); err == nil {
		t.Fatal("Robot SubTask 不得引用未安装 Skill")
	}
	missingRuntimeFields := `{"summary":"执行期补齐位姿","subtasks":[{"id":"sub-nav","kind":"robot_skill","goal":"导航","spec":{"skill_name":"grasp-object","skill_version":"0.3.0","intent":{"target_ref":"slot-1"}}}]}`
	if _, err := decodeAndValidateTaskPlan(missingRuntimeFields, task, catalog); err != nil {
		t.Fatalf("Planning 应允许省略执行期字段，由 Pydantic 在最终输入上校验: %v", err)
	}
	explicitNull := `{"summary":"尚未决定姿态","subtasks":[{"id":"sub-nav","kind":"robot_skill","goal":"导航","spec":{"skill_name":"grasp-object","skill_version":"0.3.0","intent":{"target_ref":"slot-1","pose_hint":null}}}]}`
	if _, err := decodeAndValidateTaskPlan(explicitNull, task, catalog); err != nil {
		t.Fatalf("Planning intent不应复制Pydantic业务字段校验，执行时再确定参数: %v", err)
	}
}

func TestRobotTaskPlanRejectsExecutionHeldStateInIntent(t *testing.T) {
	task := store.Task{RequiredRole: "robot"}
	catalog := robotTaskPlanningView{Skills: []robotTaskSkillView{
		{Name: "semantic-navigation", Version: "0.4.6"},
		{Name: "grasp-object", Version: "0.4.21"},
		{Name: "place-object", Version: "0.4.41"},
	}}
	allowed := `{"summary":"来源导航","subtasks":[{"id":"nav-to-source","kind":"robot_skill","goal":"导航到来源箱","spec":{"skill_name":"semantic-navigation","skill_version":"0.4.6","intent":{"target_ref":"tote-large-l3-r2-c1","source_pallet_ref":"pallet-a","carried_object_ref":null}},"completion_criteria":{"arrived_at_source_workstation":true,"carrying_object":false}}]}`
	if _, err := decodeAndValidateTaskPlan(allowed, task, catalog); err != nil {
		t.Fatalf("carried_object_ref 与 completion_criteria.carrying_object 应通过: %v", err)
	}

	// W-F1-01：来源导航 intent 写入布尔 carrying_object，物理成功后 Gate 判负。
	f1 := `{"summary":"来源导航","subtasks":[{"id":"subtask-991e9dc7-133d-45fd-900d-4e7782d91b6c","kind":"robot_skill","goal":"导航到来源托盘","spec":{"skill_name":"semantic-navigation","skill_version":"0.4.6","intent":{"navigation_goal":"approach_source_object","target_ref":"tote-large-l3-r2-c1","source_pallet_ref":"pallet-a","carrying_object":false}},"completion_criteria":{"arrived_at_source_workstation":true,"carrying_object":false}}]}`
	if _, err := decodeAndValidateTaskPlan(f1, task, catalog); err == nil {
		t.Fatal("W-F1-01 的 intent.carrying_object 必须在规划期拒绝")
	} else if !strings.Contains(err.Error(), "carrying_object") {
		t.Fatalf("规划错误应点名 carrying_object: %v", err)
	}

	// W-C0-02：携物导航 intent 也写入了 carrying_object。
	c0 := `{"summary":"携物导航","subtasks":[{"id":"st-3-nav-to-target","kind":"robot_skill","goal":"携物到目标列","spec":{"skill_name":"semantic-navigation","skill_version":"0.4.6","intent":{"action":"navigate_to_target","carried_object_ref":"tote-large-l3-r1-c1","carrying_object":false,"target_ref":"pallet-b-slot-r1-c1"}}}]}`
	if _, err := decodeAndValidateTaskPlan(c0, task, catalog); err == nil {
		t.Fatal("携物导航 intent.carrying_object 必须在规划期拒绝")
	}

	held := `{"summary":"放置","subtasks":[{"id":"place-1","kind":"robot_skill","goal":"放置","spec":{"skill_name":"place-object","skill_version":"0.4.41","intent":{"object_ref":"tote-1","held_object":{"object_ref":"tote-1"}}}}]}`
	if _, err := decodeAndValidateTaskPlan(held, task, catalog); err == nil {
		t.Fatal("intent.held_object 必须在规划期拒绝")
	}

	holding := `{"summary":"来源导航","subtasks":[{"id":"nav-holding","kind":"robot_skill","goal":"导航到来源箱","spec":{"skill_name":"semantic-navigation","skill_version":"0.4.6","intent":{"target_ref":"tote-1","holding_object":null}},"completion_criteria":{"carrying_object":false}}]}`
	if _, err := decodeAndValidateTaskPlan(holding, task, catalog); err == nil {
		t.Fatal("intent.holding_object 必须在规划期拒绝")
	} else if !strings.Contains(err.Error(), "holding_object") {
		t.Fatalf("规划错误应点名 holding_object: %v", err)
	}
}

func TestRobotTaskRecoveryRejectsExecutionHeldStateInIntent(t *testing.T) {
	task := store.Task{RequiredRole: "robot"}
	catalog := robotTaskPlanningView{Skills: []robotTaskSkillView{
		{Name: "semantic-navigation", Version: "0.4.6"},
	}}
	fail := `{"decision":"fail_task","summary":"无法恢复","replacements":[]}`
	decision, err := decodeAndValidateTaskRecovery(fail, task, catalog)
	if err != nil || decision.Decision != "fail_task" {
		t.Fatalf("fail_task 应通过: decision=%+v err=%v", decision, err)
	}

	clean := `{"decision":"revise_pending","summary":"重走来源导航","replacements":[{"id":"nav-retry","kind":"robot_skill","goal":"导航到来源箱","spec":{"skill_name":"semantic-navigation","skill_version":"0.4.6","intent":{"target_ref":"tote-1","source_pallet_ref":"pallet-a"}},"completion_criteria":{"carrying_object":false}}]}`
	if _, err := decodeAndValidateTaskRecovery(clean, task, catalog); err != nil {
		t.Fatalf("干净的恢复步骤应通过: %v", err)
	}

	dirty := `{"decision":"revise_pending","summary":"重走来源导航","replacements":[{"id":"nav-retry","kind":"robot_skill","goal":"导航到来源箱","spec":{"skill_name":"semantic-navigation","skill_version":"0.4.6","intent":{"target_ref":"tote-1","carrying_object":false}}}]}`
	if _, err := decodeAndValidateTaskRecovery(dirty, task, catalog); err == nil {
		t.Fatal("恢复 replacements 写入 carrying_object 必须拒绝")
	} else if !strings.Contains(err.Error(), "carrying_object") {
		t.Fatalf("恢复错误应点名 carrying_object: %v", err)
	}

	holding := `{"decision":"revise_pending","summary":"重走来源导航","replacements":[{"id":"nav-retry","kind":"robot_skill","goal":"导航到来源箱","spec":{"skill_name":"semantic-navigation","skill_version":"0.4.6","intent":{"target_ref":"tote-1","holding_object":null}}}]}`
	if _, err := decodeAndValidateTaskRecovery(holding, task, catalog); err == nil {
		t.Fatal("恢复 replacements 写入 holding_object 必须拒绝")
	} else if !strings.Contains(err.Error(), "holding_object") {
		t.Fatalf("恢复错误应点名 holding_object: %v", err)
	}
}

// wf1DirtySourceNavPlan 是 W-F1-01 落盘的那条来源导航规划：intent 里写了
// carrying_object:false，物理执行成功后被 Gate 判 protocol_failed。
const wf1DirtySourceNavPlan = `{"summary":"来源导航","subtasks":[{"id":"subtask-991e9dc7-133d-45fd-900d-4e7782d91b6c","kind":"robot_skill","goal":"导航到来源托盘 pallet-a 顶层箱 tote-large-l3-r2-c1 的可操作工位，由 Navigation Ability 依据实时场景解析基座位置并确认到达且未携物。","spec":{"skill_name":"semantic-navigation","skill_version":"0.4.6","intent":{"navigation_goal":"approach_source_object","target_ref":"tote-large-l3-r2-c1","source_pallet_ref":"pallet-a","carrying_object":false}},"completion_criteria":{"arrived_at_source_workstation":true,"carrying_object":false,"source_object_observed":true},"depends_on":[]}]}`

const wf1CleanSourceNavPlan = `{"summary":"来源导航","subtasks":[{"id":"subtask-991e9dc7-133d-45fd-900d-4e7782d91b6c","kind":"robot_skill","goal":"导航到来源托盘 pallet-a 顶层箱 tote-large-l3-r2-c1 的可操作工位，由 Navigation Ability 依据实时场景解析基座位置并确认到达且未携物。","spec":{"skill_name":"semantic-navigation","skill_version":"0.4.6","intent":{"navigation_goal":"approach_source_object","target_ref":"tote-large-l3-r2-c1","source_pallet_ref":"pallet-a"}},"completion_criteria":{"arrived_at_source_workstation":true,"carrying_object":false,"source_object_observed":true},"depends_on":[]}]}`

func writePublishedRobotSkill(t *testing.T, st *store.Store, name, version, description string) {
	t.Helper()
	now := time.Now().UTC()
	archivePath := filepath.Join(t.TempDir(), name+"-"+version+".zip")
	output, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(output)
	manifest, err := writer.Create(name + "/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manifest.Write([]byte("---\nname: " + name + "\ndescription: " + description +
		"\ncategory: robot_skill\nversion: " + version + "\n---\n# " + name + "\n")); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err = output.Close(); err != nil {
		t.Fatal(err)
	}
	if err = st.SaveRobotSkillPackage(store.RobotSkillPackage{
		Name: name, Version: version, Description: description,
		Category: "robot_skill", PackagePath: archivePath, PublishedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

func setupWF1PlanningReplay(t *testing.T, robotID string, replies ...kernel.MockReply) (
	*Service, *kernel.MockChatModel, store.Project, store.ChatSession, store.WorkflowView, store.Task) {
	t.Helper()
	fx := newTestFixture(t)
	now := time.Now().UTC()
	writePublishedRobotSkill(t, fx.st, "semantic-navigation", "0.4.6", "导航到业务目标")
	pilot := store.RobotPilot{PilotInstanceID: "pilot-" + robotID, RobotID: robotID,
		RobotModel: "r1pro", Backend: "fake", Status: "online", RobotStatus: "idle",
		AbilityFrameworkStatus: "ready", LastSeenAt: now}
	if err := fx.st.SaveRobotPilot(pilot); err != nil {
		t.Fatal(err)
	}
	if err := fx.st.SaveRobotPilotSkill(store.RobotPilotSkill{
		PilotInstanceID: pilot.PilotInstanceID, Name: "semantic-navigation", Version: "0.4.6",
		Enabled: true, Status: "installed", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	robotProfileDir := filepath.Join(fx.profileRoot, "robot")
	if err := os.MkdirAll(robotProfileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(robotProfileDir, "role.yaml"), []byte(
		"name: robot\nmode: worker\ndescription: Robot Task\nmodel: mock\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(robotProfileDir, "AGENT.md"),
		[]byte("# Robot\n只处理当前 Robot Task。"), 0o644); err != nil {
		t.Fatal(err)
	}
	model := kernel.NewMockChatModel()
	model.SetScript(replies...)
	svc := newWorkflowRuntimeForTest(t, fx, model)
	project, err := fx.st.EnsureDefaultProject("usr-" + robotID)
	if err != nil {
		t.Fatal(err)
	}
	conversation := store.ChatSession{ID: "cs-" + robotID, UserID: project.OwnerID,
		ProjectID: project.ID, Title: "W-F1-01 replay", CreatedAt: now, UpdatedAt: now}
	if err = fx.st.CreateChatSession(conversation); err != nil {
		t.Fatal(err)
	}
	proposal, err := fx.st.SubmitPlanProposal(project.ID, conversation.ID,
		store.WorkflowDraft{Goal: "搬箱", Tasks: []store.TaskDraft{{
			ID: "move-l3-r2-c1", RequiredRole: "robot", Goal: "导航到来源箱",
		}}}, "", json.RawMessage(`{"allowed_skills":["semantic-navigation"]}`), "# 搬箱", now)
	if err != nil {
		t.Fatal(err)
	}
	view, err := fx.st.ApprovePlanProposal(proposal.ID, proposal.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	task, err := fx.st.AssignTask(view.Tasks[0].ID, view.Tasks[0].Revision,
		"robot:"+robotID, pilot.RobotID, now)
	if err != nil {
		t.Fatal(err)
	}
	return svc, model, project, conversation, view, task
}

func TestRobotTaskPlanningCorrectsWF1HeldStateInSameRun(t *testing.T) {
	svc, model, project, conversation, view, task := setupWF1PlanningReplay(t, "r1pro-wf1",
		kernel.MockReply{Content: wf1DirtySourceNavPlan},
		kernel.MockReply{Content: wf1CleanSourceNavPlan},
	)
	items, err := svc.PlanTask(context.Background(), workflow.TaskPlanRequest{
		UserID: project.OwnerID, Project: project, Conversation: conversation,
		Workflow: view.Workflow, Task: store.TaskDraft{
			ID: task.ID, RequiredRole: "robot", Goal: task.Goal,
		},
	})
	if err != nil || len(items) != 1 || items[0].ID != "subtask-991e9dc7-133d-45fd-900d-4e7782d91b6c" {
		t.Fatalf("W-F1-01 脏规划应在同一 Run 被纠正: items=%+v err=%v", items, err)
	}
	var spec map[string]any
	if err = json.Unmarshal(items[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}
	intent, _ := spec["intent"].(map[string]any)
	if _, exists := intent["carrying_object"]; exists {
		t.Fatalf("纠正后的 intent 仍含 carrying_object: %+v", spec)
	}
	if intent["target_ref"] != "tote-large-l3-r2-c1" {
		t.Fatalf("纠正不得丢掉业务身份: %+v", spec)
	}
	var criteria map[string]any
	if err = json.Unmarshal(items[0].CompletionCriteria, &criteria); err != nil {
		t.Fatal(err)
	}
	if criteria["carrying_object"] != false {
		t.Fatalf("completion_criteria.carrying_object 应保留: %+v", criteria)
	}

	calls := model.CallInputs()
	if len(calls) != 2 {
		t.Fatalf("应先拒绝再纠正，实际调用 %d 次", len(calls))
	}
	var correction strings.Builder
	for _, message := range calls[1] {
		if message != nil {
			correction.WriteString(message.Content)
		}
	}
	if !strings.Contains(correction.String(), "carrying_object") {
		t.Fatalf("修正提示应点名 carrying_object: %s", correction.String())
	}

	runs, _, err := svc.st.ListRunSessions(store.RunFilter{ChatSessionID: conversation.ID}, 0, 0)
	if err != nil || len(runs) != 1 || runs[0].Kind != store.RunKindTaskPlanning ||
		runs[0].Status != store.RunStatusCompleted {
		t.Fatalf("纠正必须发生在同一个已完成 Planning Run: runs=%+v err=%v", runs, err)
	}
}

func TestRobotTaskPlanningRejectsRepeatedWF1HeldState(t *testing.T) {
	// The unified contract permits two corrections, including repeated errors;
	// exhaustion, not the second identical error text, is the stopping condition.
	svc, model, project, conversation, view, task := setupWF1PlanningReplay(t, "r1pro-wf1-repeat",
		kernel.MockReply{Content: wf1DirtySourceNavPlan},
		kernel.MockReply{Content: wf1DirtySourceNavPlan},
		kernel.MockReply{Content: wf1DirtySourceNavPlan},
	)
	_, err := svc.PlanTask(context.Background(), workflow.TaskPlanRequest{
		UserID: project.OwnerID, Project: project, Conversation: conversation,
		Workflow: view.Workflow, Task: store.TaskDraft{
			ID: task.ID, RequiredRole: "robot", Goal: task.Goal,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "carrying_object") {
		t.Fatalf("模型坚持写 carrying_object 时 Run 必须失败: %v", err)
	}
	if !strings.Contains(err.Error(), "纠正预算耗尽") || len(model.CallInputs()) != 3 {
		t.Fatalf("应在两次纠正后终止 Run: calls=%d err=%v", len(model.CallInputs()), err)
	}
}
