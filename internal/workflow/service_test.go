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
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/event"
	interactionsvc "insightos.cn/semantic-framework/internal/interaction"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/log"
)

type fakePlanner struct {
	mu           sync.Mutex
	taskPlans    []string
	taskSubTasks map[string][]store.SubTaskDraft
}

func (p *fakePlanner) ResolveTaskAssignment(_ context.Context,
	request TaskAssignmentRequest) (TaskAssignment, error) {
	agentID := "developer-a"
	if request.Task.ID == "task-b" {
		agentID = "developer-b"
	}
	return TaskAssignment{AgentID: agentID}, nil
}

func (p *fakePlanner) PlanTask(_ context.Context, request TaskPlanRequest) ([]store.SubTaskDraft, error) {
	p.mu.Lock()
	p.taskPlans = append(p.taskPlans, request.Task.ID)
	configured := append([]store.SubTaskDraft(nil), p.taskSubTasks[request.Task.ID]...)
	p.mu.Unlock()
	if len(configured) > 0 {
		return configured, nil
	}
	return []store.SubTaskDraft{{ID: "sub-" + request.Task.ID, Kind: "agent_step", Goal: request.Task.Goal + "步骤"}}, nil
}

func (p *fakePlanner) PlanTaskWithResult(ctx context.Context,
	request TaskPlanRequest) (TaskPlanResult, error) {
	items, err := p.PlanTask(ctx, request)
	return TaskPlanResult{Summary: "已为「" + request.Task.Goal + "」规划可执行步骤。",
		SubTasks: items}, err
}

func TestPlanProposalDoesNotCreateWorkflowBeforeApproval(t *testing.T) {
	st, project, conversation, service, planner, _ := newWorkflowFixture(t)
	proposal, err := submitTestProposal(service, project, conversation, store.WorkflowDraft{Goal: "多 Agent 拆垛"})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Status != store.PlanProposalStatusReady || proposal.DocumentMarkdown == "" {
		t.Fatalf("Plan Proposal 未形成可审阅文档: %+v", proposal)
	}
	if workflows, err := st.ListWorkflows(project.ID, false); err != nil || len(workflows) != 0 {
		t.Fatalf("批准前数据库不得出现 Workflow: workflows=%+v err=%v", workflows, err)
	}
	planner.mu.Lock()
	taskPlans := append([]string(nil), planner.taskPlans...)
	planner.mu.Unlock()
	if len(taskPlans) != 0 {
		t.Fatalf("用户批准前不得批量运行 Task Agent 规划 SubTask: %+v", taskPlans)
	}
	var draft store.WorkflowDraft
	if err := json.Unmarshal(proposal.StructuredPlan, &draft); err != nil ||
		len(draft.Tasks) != 2 || len(draft.Tasks[0].SubTasks) != 0 {
		t.Fatalf("Proposal 只能包含 Leader Task DAG: draft=%+v err=%v", draft, err)
	}
}

func TestSubmitPlanProposalUsesSubmittedTasksWithoutSecondPlanningRun(t *testing.T) {
	st, project, conversation, service, _, executor := newWorkflowFixture(t)
	draft := store.WorkflowDraft{Goal: "搬运 box-17", Tasks: []store.TaskDraft{{
		ID: "task-move", RequiredRole: "robot",
		RequiredCapabilities: json.RawMessage(`["grasp-object","semantic-navigation","place-object"]`),
		ResourceRequirements: json.RawMessage(`{"robot_ids":["r1pro-fake-02"]}`),
		Goal:                 "将 box-17 搬运到目标槽位",
		CompletionCriteria:   json.RawMessage(`{"placement_stable":true,"gripper_empty":true}`),
	}}}
	proposal, err := service.SubmitPlanProposal(context.Background(), project.OwnerID, project.ID,
		conversation.ID, draft, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Status != store.PlanProposalStatusReady ||
		!strings.Contains(proposal.DocumentMarkdown, "将 box-17 搬运到目标槽位") {
		t.Fatalf("带 Task 的 plan.suggest 必须直接形成可审阅 Proposal: %+v", proposal)
	}
	if workflows, listErr := st.ListWorkflows(project.ID, false); listErr != nil || len(workflows) != 0 {
		t.Fatalf("用户批准前数据库不得出现 Workflow: %+v %v", workflows, listErr)
	}

	view, err := service.ApprovePlanProposal(context.Background(), project.OwnerID,
		project.ID, proposal.ID, proposal.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Tasks) != 1 || view.Tasks[0].Goal != "将 box-17 搬运到目标槽位" {
		t.Fatalf("批准必须原子保留 Proposal Task，而不是重新规划: %+v", view)
	}
	messages, err := st.ListChatMessages(conversation.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var started *store.ChatMessage
	for index := range messages {
		var metadata map[string]any
		if json.Unmarshal([]byte(messages[index].Metadata), &metadata) == nil &&
			metadata["message_kind"] == "workflow_started" {
			started = &messages[index]
			break
		}
	}
	if started == nil || started.AgentID != "leader" || started.Message == nil ||
		started.Message.Role != schema.Assistant {
		t.Fatalf("Workflow 启动活动必须由 Leader 以 Assistant 消息写入原 Conversation: %+v", messages)
	}
	execution := receiveExecution(t, executor)
	if execution.Task.ID != "task-move" {
		t.Fatalf("批准后应立即调度同一个 Task: %+v", execution.Task)
	}
	messages, err = st.ListChatMessages(conversation.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var planned *store.ChatMessage
	for index := range messages {
		var metadata map[string]any
		if json.Unmarshal([]byte(messages[index].Metadata), &metadata) == nil &&
			metadata["message_kind"] == "task_plan_ready" {
			planned = &messages[index]
			break
		}
	}
	if planned == nil || planned.AgentID != execution.Task.AssignedAgentID ||
		planned.Message == nil || !strings.Contains(planned.Message.Content, "规划可执行步骤") {
		t.Fatalf("Task Agent规划摘要必须作为真实Agent消息进入Conversation: %+v", messages)
	}
	current, err := st.GetWorkflow(view.Workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = service.StopWorkflow(context.Background(), project.OwnerID, project.ID,
		view.Workflow.ID, current.Revision)
	waitWorkflowStatus(t, st, view.Workflow.ID, store.WorkflowStatusStopped)
	if _, err := service.ApprovePlanProposal(context.Background(), project.OwnerID,
		project.ID, proposal.ID, proposal.Revision); err == nil {
		t.Fatal("已批准 Proposal 不得被同一句批准重复创建 Workflow")
	}
}

type controlledExecutor struct {
	started chan TaskExecution
	release chan struct{}
}

type multiAgentPlanner struct{}

func (p *multiAgentPlanner) ResolveTaskAssignment(_ context.Context,
	request TaskAssignmentRequest) (TaskAssignment, error) {
	switch request.Task.ID {
	case "task-map":
		return TaskAssignment{AgentID: "map-1"}, nil
	case "task-box-17":
		return TaskAssignment{AgentID: "robot:r1pro-a", RobotID: "r1pro-a"}, nil
	case "task-box-18":
		return TaskAssignment{AgentID: "robot:r1pro-b", RobotID: "r1pro-b"}, nil
	case "task-monitor":
		return TaskAssignment{AgentID: "monitor-1"}, nil
	default:
		return TaskAssignment{}, ErrTaskWaitingResource
	}
}

func (p *multiAgentPlanner) PlanTask(_ context.Context,
	request TaskPlanRequest) ([]store.SubTaskDraft, error) {
	kind := "agent_step"
	if request.Task.RequiredRole == "robot" {
		kind = "robot_skill"
	}
	return []store.SubTaskDraft{{ID: "sub-" + request.Task.ID, Kind: kind,
		Goal: request.Task.Goal}}, nil
}

func TestTaskDAGSchedulesTwoRobotTasksInParallelAfterMap(t *testing.T) {
	st := openWorkflowStore(t)
	project, conversation := createWorkflowProject(t, st)
	planner := &multiAgentPlanner{}
	executor := &controlledExecutor{started: make(chan TaskExecution, 8), release: make(chan struct{}, 8)}
	service, err := NewService(Deps{Store: st, Planner: planner, Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := submitTestProposal(service, project, conversation, multiAgentWorkflowDraft())
	if err != nil {
		t.Fatal(err)
	}
	view, err := service.ApprovePlanProposal(context.Background(), project.OwnerID,
		project.ID, proposal.ID, proposal.Revision)
	if err != nil {
		t.Fatal(err)
	}
	first := receiveExecution(t, executor)
	if first.Task.ID != "task-map" || first.Task.AssignedAgentID != "map-1" {
		t.Fatalf("首个 Task 应由 Map Agent 负责: %+v", first.Task)
	}
	executor.release <- struct{}{}
	started := map[string]TaskExecution{}
	for len(started) < 2 {
		next := receiveExecution(t, executor)
		started[next.Task.ID] = next
	}
	for taskID, robotID := range map[string]string{
		"task-box-17": "r1pro-a", "task-box-18": "r1pro-b",
	} {
		execution, ok := started[taskID]
		if !ok || execution.Task.AssignedRobotID != robotID ||
			execution.Task.AssignedAgentID != "robot:"+robotID {
			t.Fatalf("Robot Task 后绑定或并行启动错误: task=%s execution=%+v", taskID, execution)
		}
	}
	select {
	case unexpected := <-executor.started:
		t.Fatalf("两个 Robot Task 未完成前不得启动 Monitor: %+v", unexpected.Task)
	default:
	}
	messages, err := st.ListChatMessages(conversation.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	assignedMilestones := map[string]bool{}
	for _, message := range messages {
		var metadata map[string]any
		if json.Unmarshal([]byte(message.Metadata), &metadata) == nil && metadata["message_kind"] == "task_assigned" {
			if id, ok := metadata["task_id"].(string); ok {
				assignedMilestones[id] = true
			}
		}
	}
	if !assignedMilestones["task-map"] || !assignedMilestones["task-box-17"] || !assignedMilestones["task-box-18"] {
		t.Fatalf("主 Conversation 必须聚合多 Agent 分配里程碑: %+v", messages)
	}
	current, _ := st.GetWorkflow(view.Workflow.ID)
	if _, err := service.StopWorkflow(context.Background(), project.OwnerID, project.ID,
		view.Workflow.ID, current.Revision); err != nil {
		t.Fatal(err)
	}
	waitWorkflowStatus(t, st, view.Workflow.ID, store.WorkflowStatusStopped)
}

func (e *controlledExecutor) ExecuteTask(ctx context.Context, execution TaskExecution) (TaskResult, error) {
	e.started <- execution
	select {
	case <-ctx.Done():
		return TaskResult{}, ctx.Err()
	case <-e.release:
		return TaskResult{Summary: "完成 " + execution.Task.ID, Evidence: json.RawMessage(`["trace"]`)}, nil
	}
}

func openWorkflowStore(t *testing.T) *store.Store {
	t.Helper()
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
	return storetest.OpenMigrated(t, logger)
}

func createWorkflowProject(t *testing.T, st *store.Store) (store.Project, store.ChatSession) {
	t.Helper()
	project, err := st.EnsureDefaultProject("usr-workflow")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	conversation := store.ChatSession{ID: "cs-workflow", UserID: project.OwnerID,
		ProjectID: project.ID, Title: "Workflow", CreatedAt: now, UpdatedAt: now}
	if err := st.CreateChatSession(conversation); err != nil {
		t.Fatal(err)
	}
	return project, conversation
}

func submitTestProposal(service *Service, project store.Project, conversation store.ChatSession,
	draft store.WorkflowDraft) (store.PlanProposal, error) {
	if len(draft.Tasks) == 0 {
		draft.Tasks = []store.TaskDraft{
			{ID: "task-a", RequiredRole: "developer", Goal: "实现"},
			{ID: "task-b", RequiredRole: "developer", Goal: "测试"},
		}
		draft.Dependencies = []store.TaskDependency{{TaskID: "task-b", DependsOnTaskID: "task-a"}}
	}
	return service.SubmitPlanProposal(context.Background(), project.OwnerID, project.ID,
		conversation.ID, draft, "", nil)
}

func multiAgentWorkflowDraft() store.WorkflowDraft {
	return store.WorkflowDraft{Goal: "多机器人拆完整个托盘", Tasks: []store.TaskDraft{
		{ID: "task-map", RequiredRole: "map", Goal: "识别托盘与箱体"},
		{ID: "task-box-17", RequiredRole: "robot", Goal: "搬运 box-17"},
		{ID: "task-box-18", RequiredRole: "robot", Goal: "搬运 box-18"},
		{ID: "task-monitor", RequiredRole: "monitor", Goal: "验证拆垛结果"},
	}, Dependencies: []store.TaskDependency{
		{TaskID: "task-box-17", DependsOnTaskID: "task-map"},
		{TaskID: "task-box-18", DependsOnTaskID: "task-map"},
		{TaskID: "task-monitor", DependsOnTaskID: "task-box-17"},
		{TaskID: "task-monitor", DependsOnTaskID: "task-box-18"},
	}}
}

func newWorkflowFixture(t *testing.T) (*store.Store, store.Project, store.ChatSession, *Service, *fakePlanner, *controlledExecutor) {
	t.Helper()
	st := openWorkflowStore(t)
	project, conversation := createWorkflowProject(t, st)
	planner := &fakePlanner{}
	executor := &controlledExecutor{started: make(chan TaskExecution, 8), release: make(chan struct{}, 8)}
	service, err := NewService(Deps{Store: st, Planner: planner, Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	return st, project, conversation, service, planner, executor
}

// workflowEventWait 是这些用例里“等待某件事最终发生”的期限：Task 启动、
// 事件广播、Task Agent 开始规划都保证会发生，期限只用于避免测试挂死，不用来
// 断言调度速度。CI Runner 比本机慢一个数量级，共享 Runner 的负载波动又很大，
// 3 秒在满载 Runner 上曾把正常调度判成失败。
const workflowEventWait = 20 * time.Second

func receiveExecution(t *testing.T, executor *controlledExecutor) TaskExecution {
	t.Helper()
	select {
	case execution := <-executor.started:
		return execution
	case <-time.After(workflowEventWait):
		t.Fatalf("等待 Developer Task 启动超过 %s", workflowEventWait)
		return TaskExecution{}
	}
}

func waitWorkflowStatus(t *testing.T, st *store.Store, id, status string) store.WorkflowView {
	t.Helper()
	deadline := time.Now().Add(workflowEventWait)
	for time.Now().Before(deadline) {
		view, err := st.GetWorkflowView(id)
		if err == nil && view.Workflow.Status == status {
			return view
		}
		time.Sleep(10 * time.Millisecond)
	}
	view, _ := st.GetWorkflowView(id)
	t.Fatalf("Workflow 未进入 %s: %+v", status, view.Workflow)
	return store.WorkflowView{}
}

// approveTestPlan 只通过正式的 Proposal 批准入口创建 Workflow。测试不再
// 复活旧的 pending Workflow 夹具，否则会掩盖“用户批准前无 Workflow”这一边界。
func approveTestPlan(t *testing.T, service *Service, project store.Project,
	conversation store.ChatSession, draft store.WorkflowDraft) (store.PlanProposal, store.WorkflowView) {
	t.Helper()
	proposal, err := submitTestProposal(service, project, conversation, draft)
	if err != nil {
		t.Fatal(err)
	}
	view, err := service.ApprovePlanProposal(context.Background(), project.OwnerID,
		project.ID, proposal.ID, proposal.Revision)
	if err != nil {
		t.Fatal(err)
	}
	return proposal, view
}

// persistTestPlan 用于重启窗口测试：批准事务真实执行，但不调用 Service
// 调度器。随后显式写入 Task Agent 已生成的单个 SubTask，构造可恢复的持久状态。
func persistTestPlan(t *testing.T, st *store.Store, service *Service,
	project store.Project, conversation store.ChatSession, draft store.WorkflowDraft) store.WorkflowView {
	t.Helper()
	proposal, err := submitTestProposal(service, project, conversation, draft)
	if err != nil {
		t.Fatal(err)
	}
	view, err := st.ApprovePlanProposal(proposal.ID, proposal.Revision, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for index := range view.Tasks {
		task := view.Tasks[index]
		assigned, assignErr := st.AssignTask(task.ID, task.Revision, task.RequiredRole+"-test", "", time.Now().UTC())
		if assignErr != nil {
			t.Fatal(assignErr)
		}
		view.Tasks[index] = assigned
		task = assigned
		kind := "agent_step"
		if task.RequiredRole == "robot" {
			kind = "robot_skill"
		}
		_, _, err = st.SetTaskSubTasks(task.ID, task.Revision, []store.SubTaskDraft{{
			ID: "sub-" + task.ID, Kind: kind, Goal: task.Goal + "步骤",
		}}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
	}
	view, err = st.GetWorkflowView(view.Workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func TestExplicitPlanRequiresCurrentProposalRevisionAndPlansTaskAfterApproval(t *testing.T) {
	st, project, conversation, service, planner, executor := newWorkflowFixture(t)
	proposal, err := submitTestProposal(service, project, conversation, store.WorkflowDraft{Goal: "实现 v0.3"})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Status != store.PlanProposalStatusReady {
		t.Fatalf("Planning 结果不完整: %+v", proposal)
	}
	planner.mu.Lock()
	plannedTasks := len(planner.taskPlans)
	planner.mu.Unlock()
	if plannedTasks != 0 {
		t.Fatalf("批准前不得预跑 Task Agent，实际 %d", plannedTasks)
	}
	select {
	case execution := <-executor.started:
		t.Fatalf("确认前不应执行 Task: %+v", execution)
	default:
	}
	if _, err := service.ApprovePlanProposal(context.Background(), project.OwnerID, project.ID,
		proposal.ID, proposal.Revision-1); !errors.Is(err, store.ErrRevisionConflict) {
		t.Fatalf("旧 Proposal revision 批准必须失败: %v", err)
	}
	confirmed, err := service.ApprovePlanProposal(context.Background(), project.OwnerID, project.ID,
		proposal.ID, proposal.Revision)
	if err != nil || confirmed.Workflow.ConfirmedRevision != confirmed.Workflow.Revision {
		t.Fatalf("确认失败: view=%+v err=%v", confirmed.Workflow, err)
	}
	first := receiveExecution(t, executor)
	if first.Task.ID != "task-a" {
		t.Fatalf("依赖调度顺序错误: %s", first.Task.ID)
	}
	executor.release <- struct{}{}
	second := receiveExecution(t, executor)
	if second.Task.ID != "task-b" {
		t.Fatalf("第二个 Task 错误: %s", second.Task.ID)
	}
	executor.release <- struct{}{}
	completed := waitWorkflowStatus(t, st, confirmed.Workflow.ID, store.WorkflowStatusCompleted)
	for _, task := range completed.Tasks {
		if task.Status != store.TaskStatusCompleted || task.ResultSummary == "" {
			t.Fatalf("Task 结果未保存: %+v", task)
		}
	}
}

func TestEachSubTaskUsesIndependentAgentRun(t *testing.T) {
	st, project, conversation, service, planner, executor := newWorkflowFixture(t)
	planner.taskSubTasks = map[string][]store.SubTaskDraft{
		"task-a": {
			{ID: "sub-a-1", Kind: "agent_step", Goal: "先完成步骤一"},
			{ID: "sub-a-2", Kind: "agent_step", Goal: "再完成步骤二", DependsOn: []string{"sub-a-1"}},
		},
	}
	_, confirmed := approveTestPlan(t, service, project, conversation,
		store.WorkflowDraft{Goal: "逐项执行 SubTask"})
	first := receiveExecution(t, executor)
	if len(first.SubTasks) != 1 || first.SubTasks[0].ID != "sub-a-1" {
		t.Fatalf("首个 Agent Run 应只负责 sub-a-1: %+v", first.SubTasks)
	}
	executor.release <- struct{}{}
	second := receiveExecution(t, executor)
	if len(second.SubTasks) != 1 || second.SubTasks[0].ID != "sub-a-2" || second.Run.ID == first.Run.ID {
		t.Fatalf("第二个 SubTask 必须使用独立 Agent Run: first=%+v second=%+v", first, second)
	}
	executor.release <- struct{}{}
	third := receiveExecution(t, executor)
	if third.Task.ID != "task-b" {
		t.Fatalf("Task A 全部 SubTask 完成后才应推进 Task B: %+v", third.Task)
	}
	executor.release <- struct{}{}
	waitWorkflowStatus(t, st, confirmed.Workflow.ID, store.WorkflowStatusCompleted)
}

func TestPauseStopsNewSchedulingAndResumeContinues(t *testing.T) {
	st, project, conversation, service, _, executor := newWorkflowFixture(t)
	_, confirmed := approveTestPlan(t, service, project, conversation,
		store.WorkflowDraft{Goal: "暂停测试"})
	_ = receiveExecution(t, executor)
	paused, err := service.PauseWorkflow(context.Background(), project.OwnerID, project.ID,
		confirmed.Workflow.ID, confirmed.Workflow.Revision)
	if err != nil || paused.Workflow.Status != store.WorkflowStatusPaused {
		t.Fatalf("暂停失败: %+v %v", paused.Workflow, err)
	}
	executor.release <- struct{}{}
	time.Sleep(100 * time.Millisecond)
	select {
	case next := <-executor.started:
		t.Fatalf("暂停时不应启动后续 Task: %+v", next)
	default:
	}
	current, _ := st.GetWorkflow(confirmed.Workflow.ID)
	if _, err := service.ResumeWorkflow(context.Background(), project.OwnerID, project.ID,
		confirmed.Workflow.ID, current.Revision); err != nil {
		t.Fatal(err)
	}
	if execution := receiveExecution(t, executor); execution.Task.ID != "task-b" {
		t.Fatalf("恢复后未继续依赖 Task: %+v", execution.Task)
	}
	executor.release <- struct{}{}
	waitWorkflowStatus(t, st, confirmed.Workflow.ID, store.WorkflowStatusCompleted)
}

func TestStopCancelsExecutorAndConverges(t *testing.T) {
	st, project, conversation, service, _, executor := newWorkflowFixture(t)
	_, confirmed := approveTestPlan(t, service, project, conversation,
		store.WorkflowDraft{Goal: "停止测试"})
	_ = receiveExecution(t, executor)
	if _, err := service.StopWorkflow(context.Background(), project.OwnerID, project.ID,
		confirmed.Workflow.ID, confirmed.Workflow.Revision); err != nil {
		t.Fatal(err)
	}
	stopped := waitWorkflowStatus(t, st, confirmed.Workflow.ID, store.WorkflowStatusStopped)
	for _, task := range stopped.Tasks {
		if task.Status != store.TaskStatusStopped {
			t.Fatalf("停止后 Task 未收敛: %+v", task)
		}
	}
}

func TestRestartPausesWithoutReplayAndExplicitResumeCreatesNewRun(t *testing.T) {
	st, project, conversation, service, _, executor := newWorkflowFixture(t)
	confirmed := persistTestPlan(t, st, service, project, conversation,
		store.WorkflowDraft{Goal: "恢复测试"})
	var err error
	task := confirmed.Tasks[0]
	task, err = st.TransitionTask(task.ID, task.Revision, store.TaskStatusRunning, "", "", nil, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	subTasks, _ := st.ListSubTasks(task.ID)
	_, _ = st.TransitionSubTask(subTasks[0].ID, subTasks[0].Revision, store.TaskStatusRunning, "", nil, time.Now().UTC())
	if err := st.CreateRunSession(store.RunSession{ID: "run-before-restart", ProjectID: project.ID,
		ChatSessionID: conversation.ID, WorkflowID: confirmed.Workflow.ID, TaskID: task.ID,
		Kind: store.RunKindTaskExecution, ContextID: task.ContextID, AgentID: task.AssignedAgentID,
		AgentName: task.AssignedAgentID, Status: store.RunStatusRunning, StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	_, _, _ = st.MarkInterruptedRuns(time.Now().UTC())
	if err := service.RecoverInterruptedWorkflows(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	paused := waitWorkflowStatus(t, st, confirmed.Workflow.ID, store.WorkflowStatusPaused)
	if paused.Tasks[0].Status != store.TaskStatusPaused || paused.Tasks[0].WaitingReason != "server_restarted" {
		t.Fatalf("重启状态不符: %+v", paused.Tasks[0])
	}
	select {
	case execution := <-executor.started:
		t.Fatalf("重启恢复不得自动重放: %+v", execution)
	default:
	}
	if _, err := service.ResumeWorkflow(context.Background(), project.OwnerID, project.ID,
		confirmed.Workflow.ID, paused.Workflow.Revision); err != nil {
		t.Fatal(err)
	}
	if execution := receiveExecution(t, executor); execution.Task.ID != task.ID {
		t.Fatalf("显式恢复未启动被中断 Task: %+v", execution.Task)
	}
	executor.release <- struct{}{}
}

func TestWorkflowPublishesProjectResourceEvents(t *testing.T) {
	st := openWorkflowStore(t)
	project, conversation := createWorkflowProject(t, st)
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
	bus := event.NewBus(logger)
	ch := bus.Subscribe(event.TopicAgentEvents)
	defer bus.Unsubscribe(ch)
	service, _ := NewService(Deps{Store: st, Bus: bus, Planner: &fakePlanner{},
		Executor: &controlledExecutor{started: make(chan TaskExecution, 1), release: make(chan struct{}, 1)}})
	_, _ = approveTestPlan(t, service, project, conversation, store.WorkflowDraft{Goal: "事件"})
	want := map[string]bool{"workflow": false, "chat_message": false}
	deadline := time.After(workflowEventWait)
	for !(want["workflow"] && want["chat_message"]) {
		select {
		case value := <-ch:
			envelope, ok := value.Payload.(ws.Envelope)
			if ok && envelope.ProjectID == project.ID {
				if _, exists := want[envelope.ResourceType]; exists {
					want[envelope.ResourceType] = envelope.ResourceID != "" && envelope.Revision > 0
					if envelope.ResourceType == "workflow" {
						payload, payloadOK := envelope.Payload.(map[string]any)
						view, viewOK := payload["workflow_view"].(store.WorkflowView)
						want["workflow"] = want["workflow"] && payloadOK && viewOK &&
							len(view.Tasks) > 0 && len(view.SubTasks) > 0
					}
					if envelope.ResourceType == "chat_message" {
						payload, payloadOK := envelope.Payload.(map[string]any)
						metadata, metadataOK := payload["metadata"].(map[string]any)
						want["chat_message"] = envelope.Type == "message.done" &&
							payloadOK && metadataOK && metadata["message_type"] == "activity" &&
							envelope.Agent.ID != ""
					}
				}
				if envelope.ResourceType == "task" || envelope.ResourceType == "subtask" {
					t.Fatalf("完整 WorkflowView 不应再展开为重复资源事件: %+v", envelope)
				}
			}
		case <-deadline:
			t.Fatalf("缺少 Workflow Project 事件: %+v", want)
		}
	}
}

func TestMapGenerationChangePausesBeforeNextTask(t *testing.T) {
	st, project, conversation, service, _, executor := newWorkflowFixture(t)
	semanticMap, _ := st.GetSemanticMap(project.ID, store.MapSlotSimulation)
	snapshot, err := st.ApplyMapUpdate(project.ID, store.MapSlotSimulation, store.MapUpdate{
		Generation: semanticMap.Generation, ExpectedRevision: semanticMap.Revision,
		Source: store.MapSourceUser, Entities: []store.MapEntity{{ID: "box-plan", Type: "box",
			Name: "箱体", Pose: store.Pose{Orientation: store.Quaternion{W: 1}},
			Bounds: store.Bounds{Kind: "box", Size: &store.Position{X: 1, Y: 1, Z: 1}}}},
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	binding, _ := json.Marshal(map[string]any{"map_id": store.MapSlotSimulation,
		"generation": snapshot.Map.Generation, "selections": []map[string]any{{
			"kind": "entity", "entity_id": "box-plan",
		}}})
	_, view := approveTestPlan(t, service, project, conversation,
		store.WorkflowDraft{Goal: "地图过期", MapScope: binding})
	if first := receiveExecution(t, executor); first.Task.ID != "task-a" {
		t.Fatalf("首 Task 错误: %s", first.Task.ID)
	}
	if _, err := st.CreateMapGeneration(project.ID, store.MapSlotSimulation,
		snapshot.Map.Revision, "reset", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	executor.release <- struct{}{}
	paused := waitWorkflowStatus(t, st, view.Workflow.ID, store.WorkflowStatusPaused)
	if paused.Workflow.Reason != "map_generation_changed" || paused.Tasks[1].WaitingReason != "map_generation_changed" {
		t.Fatalf("旧地图引用未阻止后续 Task: %+v", paused)
	}
	select {
	case execution := <-executor.started:
		t.Fatalf("generation 已变化，不得启动后续 Task: %+v", execution)
	default:
	}
}

func TestWorkflowServiceRejectsInvalidMapBindingBeforePersistence(t *testing.T) {
	cases := []struct {
		name    string
		binding string
	}{
		{name: "legacy fields", binding: `{"slot":"simulation_map","entity_id":"box"}`},
		{name: "empty selections", binding: `{"map_id":"simulation_map","generation":1,"selections":[]}`},
		{name: "point missing frame", binding: `{"map_id":"simulation_map","generation":1,"selections":[{"kind":"point","position":[1,2,3]}]}`},
		{name: "point wrong dimension", binding: `{"map_id":"simulation_map","generation":1,"selections":[{"kind":"point","frame_id":"world","position":[1,2]}]}`},
		{name: "missing entity", binding: `{"map_id":"simulation_map","generation":1,"selections":[{"kind":"entity","entity_id":"other-project-only"}]}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			st, project, conversation, service, _, _ := newWorkflowFixture(t)
			if _, err := submitTestProposal(service, project, conversation, store.WorkflowDraft{Goal: "非法地图绑定", MapScope: json.RawMessage(test.binding)}); err == nil {
				t.Fatal("绕过 HTTP 的非法 map_binding 必须拒绝")
			}
			if workflows, err := st.ListWorkflows(project.ID, false); err != nil || len(workflows) != 0 {
				t.Fatalf("非法 map_binding 不得留下 Workflow: %+v %v", workflows, err)
			}
		})
	}
}

func TestPausedWorkflowRestartConvergesRunningTask(t *testing.T) {
	st, project, conversation, service, _, _ := newWorkflowFixture(t)
	confirmed := persistTestPlan(t, st, service, project, conversation,
		store.WorkflowDraft{Goal: "暂停窗口重启"})
	task, _ := st.TransitionTask(confirmed.Tasks[0].ID, confirmed.Tasks[0].Revision,
		store.TaskStatusRunning, "", "", nil, time.Now().UTC())
	subs, _ := st.ListSubTasks(task.ID)
	_, _ = st.TransitionSubTask(subs[0].ID, subs[0].Revision, store.TaskStatusRunning, "", nil, time.Now().UTC())
	paused, _ := st.TransitionWorkflow(confirmed.Workflow.ID, confirmed.Workflow.Revision,
		store.WorkflowStatusPaused, time.Now().UTC())
	if err := st.CreateRunSession(store.RunSession{ID: "run-paused-crash", ProjectID: project.ID,
		ChatSessionID: conversation.ID, WorkflowID: confirmed.Workflow.ID, TaskID: task.ID,
		Kind: store.RunKindTaskExecution, ContextID: task.ContextID, AgentID: task.AssignedAgentID,
		AgentName: task.AssignedAgentID, Status: store.RunStatusRunning, StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	_, _, _ = st.MarkInterruptedRuns(time.Now().UTC())
	if err := service.RecoverInterruptedWorkflows(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	recovered, _ := st.GetWorkflowView(paused.ID)
	if recovered.Workflow.Status != store.WorkflowStatusPaused || recovered.Tasks[0].Status != store.TaskStatusPaused {
		t.Fatalf("paused Workflow 崩溃窗口未收敛: %+v", recovered)
	}
}

func TestRestartSchedulesNeverStartedPendingTaskOnce(t *testing.T) {
	st, project, conversation, service, _, executor := newWorkflowFixture(t)
	confirmed := persistTestPlan(t, st, service, project, conversation,
		store.WorkflowDraft{Goal: "确认后重启"})
	if confirmed.Tasks[0].Status != store.TaskStatusPending {
		t.Fatalf("夹具应停在尚未启动的 pending Task: %+v", confirmed.Tasks[0])
	}
	if err := service.RecoverInterruptedWorkflows(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	first := receiveExecution(t, executor)
	if first.Task.ID != "task-a" {
		t.Fatalf("恢复后应按依赖顺序启动首个 Task: %+v", first.Task)
	}
	select {
	case duplicate := <-executor.started:
		t.Fatalf("同一个 pending Task 不得重复启动: %+v", duplicate)
	default:
	}
	executor.release <- struct{}{}
	second := receiveExecution(t, executor)
	if second.Task.ID != "task-b" {
		t.Fatalf("首个 Task 完成后应启动依赖 Task: %+v", second.Task)
	}
	executor.release <- struct{}{}
}

func TestTaskInteractionPausesBeforeCreateAndReservesRunBeforeExecutor(t *testing.T) {
	st, project, conversation, service, _, executor := newWorkflowFixture(t)
	confirmed := persistTestPlan(t, st, service, project, conversation,
		store.WorkflowDraft{Goal: "结构化输入"})
	task, _ := st.TransitionTask(confirmed.Tasks[0].ID, confirmed.Tasks[0].Revision,
		store.TaskStatusRunning, "", "", nil, time.Now().UTC())
	subs, _ := st.ListSubTasks(task.ID)
	_, _ = st.TransitionSubTask(subs[0].ID, subs[0].Revision, store.TaskStatusRunning, "", nil, time.Now().UTC())
	paused, err := service.PauseTaskForInteraction(project.OwnerID, project.ID, confirmed.Workflow.ID,
		task.ID, task.Revision)
	if err != nil || paused.Status != store.TaskStatusPaused {
		t.Fatalf("Task 输入安全边界失败: %+v %v", paused, err)
	}
	pausedSubs, _ := st.ListSubTasks(task.ID)
	if pausedSubs[0].Status != store.TaskStatusPaused {
		t.Fatalf("当前 SubTask 未同步暂停: %+v", pausedSubs)
	}
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
	interactions := interactionsvc.NewService(st, event.NewBus(logger), logger)
	interactions.SetAnswerRouter(service)
	created, err := interactions.CreateStructured(context.Background(), interactionsvc.StructuredRequest{
		ProjectID: project.ID, SessionID: conversation.ID, WorkflowID: confirmed.Workflow.ID,
		TaskID: task.ID, SourceAgentID: task.AssignedAgentID, SourceRevision: paused.Revision,
		Type: store.InteractionTypeInput, UIKind: store.InteractionUIForm, Prompt: "补充信息",
		ResponseSchema: json.RawMessage(`{"type":"object"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := interactions.ReplyStructured(context.Background(), project.OwnerID, project.ID,
		created.ID, json.RawMessage(`{"value":"ok"}`), paused.Revision); err != nil {
		t.Fatal(err)
	}
	execution := receiveExecution(t, executor)
	if execution.Answer == nil || execution.Run.SourceInteractionID != created.ID || execution.Run.Status != store.RunStatusRunning {
		t.Fatalf("Executor 启动前未持久预留幂等 Run: %+v", execution)
	}
	answered, _ := st.GetInteraction(created.ID)
	if err := service.RouteInteractionAnswer(context.Background(), answered); err != nil {
		t.Fatal(err)
	}
	select {
	case duplicate := <-executor.started:
		t.Fatalf("crash-window 重路由不得创建第二个 Run: %+v", duplicate)
	default:
	}
	executor.release <- struct{}{}
	if next := receiveExecution(t, executor); next.Task.ID == task.ID {
		t.Fatalf("同一 Task 被重复执行: %+v", next.Task)
	}
	executor.release <- struct{}{}
}

func TestRestartRoutesAnsweredWaitingInputWithoutReplayingInterruptedRun(t *testing.T) {
	st, project, conversation, service, _, executor := newWorkflowFixture(t)
	confirmed := persistTestPlan(t, st, service, project, conversation,
		store.WorkflowDraft{Goal: "回答落库后的重启窗口"})
	var err error
	task, err := st.TransitionTask(confirmed.Tasks[0].ID, confirmed.Tasks[0].Revision,
		store.TaskStatusRunning, "", "", nil, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	paused, err := service.PauseTaskForInteraction(project.OwnerID, project.ID, confirmed.Workflow.ID,
		task.ID, task.Revision)
	if err != nil {
		t.Fatal(err)
	}
	interactionID := "int-restart-waiting-input"
	if err := st.CreateInteraction(store.Interaction{ID: interactionID, ProjectID: project.ID,
		SessionID: conversation.ID, WorkflowID: confirmed.Workflow.ID, TaskID: task.ID,
		Agent: task.AssignedAgentID, Type: store.InteractionTypeInput, UIKind: store.InteractionUIForm,
		Status: store.InteractionStatusPending, Payload: `{}`, ResponseSchema: `{}`,
		SourceRevision: paused.Revision, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := st.AnswerInteractionAtRevision(interactionID, `{"value":"继续"}`,
		paused.Revision, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := service.RecoverInterruptedWorkflows(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	recovered, err := st.GetWorkflow(confirmed.Workflow.ID)
	if err != nil || recovered.Status != store.WorkflowStatusRunning {
		t.Fatalf("只有 waiting_input 且尚未预留 continuation Run 时应保持 running: %+v %v", recovered, err)
	}
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
	interactions := interactionsvc.NewService(st, event.NewBus(logger), logger)
	interactions.SetAnswerRouter(service)
	if err := interactions.RecoverAnsweredInteractions(context.Background()); err != nil {
		t.Fatal(err)
	}
	execution := receiveExecution(t, executor)
	if execution.Run.SourceInteractionID != interactionID {
		t.Fatalf("恢复后的新 Run 未关联回答: %+v", execution.Run)
	}
	executor.release <- struct{}{}
}

type availabilityPlanner struct {
	mu        sync.Mutex
	available bool
}

func (p *availabilityPlanner) ResolveTaskAssignment(_ context.Context,
	_ TaskAssignmentRequest) (TaskAssignment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.available {
		return TaskAssignment{}, ErrTaskWaitingResource
	}
	return TaskAssignment{AgentID: "robot:r1pro-available", RobotID: "r1pro-available"}, nil
}

func (p *availabilityPlanner) PlanTask(_ context.Context,
	request TaskPlanRequest) ([]store.SubTaskDraft, error) {
	return []store.SubTaskDraft{{ID: "sub-" + request.Task.ID, Kind: "robot_skill",
		Goal: request.Task.Goal}}, nil
}

func (p *availabilityPlanner) setAvailable() {
	p.mu.Lock()
	p.available = true
	p.mu.Unlock()
}

type blockingTaskPlanner struct {
	started chan struct{}
	release chan struct{}
}

type failingTaskPlanner struct{ cause error }

func (p *failingTaskPlanner) ResolveTaskAssignment(_ context.Context,
	_ TaskAssignmentRequest) (TaskAssignment, error) {
	return TaskAssignment{AgentID: "developer-planning-failure"}, nil
}

func (p *failingTaskPlanner) PlanTask(_ context.Context,
	_ TaskPlanRequest) ([]store.SubTaskDraft, error) {
	return nil, p.cause
}

func TestTaskPreparationFailureConvergesWorkflowInsteadOfStayingPending(t *testing.T) {
	st := openWorkflowStore(t)
	project, conversation := createWorkflowProject(t, st)
	planningErr := errors.New("Task Planning 返回非法 JSON")
	service, err := NewService(Deps{
		Store: st, Planner: &failingTaskPlanner{cause: planningErr},
		Executor: &controlledExecutor{
			started: make(chan TaskExecution, 1), release: make(chan struct{}, 1),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := submitTestProposal(service, project, conversation, store.WorkflowDraft{
		Goal: "验证规划失败收敛", Tasks: []store.TaskDraft{{
			ID: "task-invalid-plan", RequiredRole: "developer", Goal: "生成执行步骤",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := service.ApprovePlanProposal(context.Background(), project.OwnerID,
		project.ID, proposal.ID, proposal.Revision)
	if err != nil {
		t.Fatal(err)
	}
	terminal := waitWorkflowStatus(t, st, view.Workflow.ID, store.WorkflowStatusFailed)
	if len(terminal.Tasks) != 1 || terminal.Tasks[0].Status != store.TaskStatusFailed ||
		!strings.Contains(terminal.Tasks[0].WaitingReason, planningErr.Error()) {
		t.Fatalf("准备错误没有保留为通用失败状态和诊断: %+v", terminal.Tasks)
	}
}

func (p *blockingTaskPlanner) ResolveTaskAssignment(_ context.Context,
	_ TaskAssignmentRequest) (TaskAssignment, error) {
	return TaskAssignment{AgentID: "robot:r1pro-planning", RobotID: "r1pro-planning"}, nil
}

func (p *blockingTaskPlanner) PlanTask(ctx context.Context,
	request TaskPlanRequest) ([]store.SubTaskDraft, error) {
	p.started <- struct{}{}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.release:
		return []store.SubTaskDraft{{ID: "sub-" + request.Task.ID,
			Kind: "robot_skill", Goal: "执行 Robot Skill"}}, nil
	}
}

func TestAssignedTaskPublishesRobotAgentPlanningState(t *testing.T) {
	st := openWorkflowStore(t)
	project, conversation := createWorkflowProject(t, st)
	planner := &blockingTaskPlanner{started: make(chan struct{}, 1), release: make(chan struct{}, 1)}
	executor := &controlledExecutor{started: make(chan TaskExecution, 1), release: make(chan struct{}, 1)}
	service, err := NewService(Deps{Store: st, Planner: planner, Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := submitTestProposal(service, project, conversation, store.WorkflowDraft{
		Goal: "显示规划过程", Tasks: []store.TaskDraft{{
			ID: "task-planning", RequiredRole: "robot", Goal: "搬运一个箱子",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := service.ApprovePlanProposal(context.Background(), project.OwnerID,
		project.ID, proposal.ID, proposal.Revision)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-planner.started:
	case <-time.After(workflowEventWait):
		t.Fatalf("Task Agent 规划未在 %s 内启动", workflowEventWait)
	}
	planning, err := st.GetTask(view.Tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if planning.Status != store.TaskStatusPending ||
		planning.WaitingReason != "planning_subtasks" || planning.AssignedRobotID == "" {
		t.Fatalf("分配后必须公开 Robot Agent 规划中状态: %+v", planning)
	}
	planner.release <- struct{}{}
	execution := receiveExecution(t, executor)
	executor.release <- struct{}{}
	if execution.Task.ID != planning.ID {
		t.Fatalf("规划完成后没有推进原 Task: %+v", execution.Task)
	}
}

func TestRobotAvailabilityEventWakesWaitingTask(t *testing.T) {
	st := openWorkflowStore(t)
	project, conversation := createWorkflowProject(t, st)
	planner := &availabilityPlanner{}
	executor := &controlledExecutor{started: make(chan TaskExecution, 2), release: make(chan struct{}, 2)}
	service, err := NewService(Deps{Store: st, Planner: planner, Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := submitTestProposal(service, project, conversation, store.WorkflowDraft{
		Goal: "等待 Robot", Tasks: []store.TaskDraft{{
			ID: "task-wait-robot", RequiredRole: "robot", Goal: "Robot 可用后执行",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := service.ApprovePlanProposal(context.Background(), project.OwnerID,
		project.ID, proposal.ID, proposal.Revision)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		task, getErr := st.GetTask(view.Tasks[0].ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if task.Status == store.TaskStatusPending && task.WaitingReason == "waiting_resource" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Robot 不可用时 Task 未进入 waiting_resource: %+v", task)
		}
		time.Sleep(10 * time.Millisecond)
	}

	planner.setAvailable()
	service.OnRobotAvailabilityChanged(context.Background(), "r1pro-available")
	execution := receiveExecution(t, executor)
	if execution.Task.AssignedRobotID != "r1pro-available" {
		t.Fatalf("Robot 可用事件没有唤醒并后绑定 Task: %+v", execution.Task)
	}
	current, _ := st.GetWorkflow(view.Workflow.ID)
	_, _ = service.StopWorkflow(context.Background(), project.OwnerID, project.ID,
		view.Workflow.ID, current.Revision)
	waitWorkflowStatus(t, st, view.Workflow.ID, store.WorkflowStatusStopped)
}

func TestResumeWorkflowRejectsDecisionFailureAndUnknownState(t *testing.T) {
	for _, reason := range []string{"robot_agent_decision_failed", "execution_state_unknown"} {
		t.Run(reason, func(t *testing.T) {
			st, project, conversation, service, _, _ := newWorkflowFixture(t)
			view, _, _ := prepareRunningRobotTask(t, st, project, conversation,
				[]store.SubTaskDraft{{ID: "sub-1", Kind: "robot_skill", Goal: "抓取"}})
			current, _ := st.GetWorkflow(view.Workflow.ID)
			paused, err := st.TransitionWorkflowWithReason(current.ID, current.Revision,
				store.WorkflowStatusPaused, reason, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.ResumeWorkflow(context.Background(), project.OwnerID, project.ID,
				paused.ID, paused.Revision); !errors.Is(err, store.ErrInvalidState) {
				t.Fatalf("Resume 必须拒绝 %s: %v", reason, err)
			}
		})
	}
}
