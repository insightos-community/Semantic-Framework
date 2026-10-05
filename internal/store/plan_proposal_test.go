package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestPlanProposalApprovalCreatesWorkflowAtomically(t *testing.T) {
	st := openTestStore(t)
	project, session := v030ProjectConversation(t, st)
	now := time.Now().UTC()
	draft := WorkflowDraft{Goal: "拆完整个托盘", Tasks: []TaskDraft{
		{ID: "task-detect", RequiredRole: "map", Goal: "识别箱体和槽位"},
		{ID: "task-move", RequiredRole: "robot",
			RequiredCapabilities: json.RawMessage(`["grasp","navigation","place"]`),
			Goal:                 "搬运 box-17"},
		{ID: "task-verify", RequiredRole: "monitor", Goal: "验证结果"},
	}, Dependencies: []TaskDependency{
		{TaskID: "task-move", DependsOnTaskID: "task-detect"},
		{TaskID: "task-verify", DependsOnTaskID: "task-move"},
	}}
	proposal, err := st.SubmitPlanProposal(project.ID, session.ID, draft, "识别、搬运并验证托盘",
		json.RawMessage(`{"robot_models":["r1pro"]}`),
		"# 拆垛计划\n\n- [ ] 识别\n- [ ] 搬运\n- [ ] 验证", now)
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Status != PlanProposalStatusReady || proposal.Revision != 1 ||
		proposal.Summary != "识别、搬运并验证托盘" {
		t.Fatalf("ready Proposal 不符: %+v", proposal)
	}
	if workflows, listErr := st.ListWorkflows(project.ID, false); listErr != nil || len(workflows) != 0 {
		t.Fatalf("用户批准前不能创建 Workflow: workflows=%+v err=%v", workflows, listErr)
	}
	if _, err := st.ApprovePlanProposal(proposal.ID, proposal.Revision+1,
		now.Add(time.Second)); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("错误 revision 必须拒绝: %v", err)
	}

	view, err := st.ApprovePlanProposal(proposal.ID, proposal.Revision, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if view.Workflow.Status != WorkflowStatusRunning ||
		string(view.Workflow.ApprovedScope) != `{"robot_models":["r1pro"]}` || len(view.Tasks) != 3 ||
		len(view.SubTasks) != 0 || len(view.Dependencies) != 2 {
		t.Fatalf("批准结果不符: %+v", view)
	}
	if view.Tasks[1].RequiredRole != "robot" || view.Tasks[1].AssignedRobotID != "" ||
		view.Tasks[1].AssignedAgentID != "" {
		t.Fatalf("Task 必须按角色后绑定: %+v", view.Tasks[1])
	}
	if _, err := st.ApprovePlanProposal(proposal.ID, proposal.Revision,
		now.Add(3*time.Second)); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("同一 revision 只能批准一次: %v", err)
	}
	approved, err := st.GetPlanProposal(proposal.ID)
	if err != nil || approved.Status != PlanProposalStatusApproved ||
		approved.Revision != proposal.Revision+1 {
		t.Fatalf("Proposal 终态不符: proposal=%+v err=%v", approved, err)
	}
}

func TestPlanProposalSubmissionIsIdempotentWithinRun(t *testing.T) {
	st := openTestStore(t)
	project, session := v030ProjectConversation(t, st)
	now := time.Now().UTC()
	draft := WorkflowDraft{Goal: "move box", Tasks: []TaskDraft{{ID: "one", RequiredRole: "robot", Goal: "move box"}}}
	first, reused, err := st.SubmitPlanProposalForRun("run-submit-1", project.ID, session.ID,
		draft, "first", nil, "# first", now)
	if err != nil || reused || first.Revision != 1 {
		t.Fatalf("first submission: proposal=%+v reused=%v err=%v", first, reused, err)
	}
	draft.Goal = "changed by duplicate model call"
	second, reused, err := st.SubmitPlanProposalForRun("run-submit-1", project.ID, session.ID,
		draft, "second", nil, "# second", now.Add(time.Second))
	if err != nil || !reused || second.ID != first.ID || second.Revision != first.Revision {
		t.Fatalf("same run must not revise: proposal=%+v reused=%v err=%v", second, reused, err)
	}
	current, err := st.GetPlanProposal(first.ID)
	if err != nil || current.Goal != "move box" || current.DocumentMarkdown != "# first" {
		t.Fatalf("duplicate changed stored plan: %+v err=%v", current, err)
	}
	third, reused, err := st.SubmitPlanProposalForRun("run-submit-2", project.ID, session.ID,
		draft, "second", nil, "# second", now.Add(2*time.Second))
	if err != nil || reused || third.ID != first.ID || third.Revision != 2 {
		t.Fatalf("different run may intentionally revise ready proposal: %+v reused=%v err=%v", third, reused, err)
	}
}

// TestPlanProposalReplayReflectsCurrentRevision 锁定重放的语义：返回值必须整体
// 反映库中当前版本。若把该 Run 当初提交的 revision 贴到已变更的内容或已批准的
// 状态上，调用方会拿到自相矛盾的对象，并可能 revision 冲突。
func TestPlanProposalReplayReflectsCurrentRevision(t *testing.T) {
	st := openTestStore(t)
	project, session := v030ProjectConversation(t, st)
	now := time.Now().UTC()
	draft := WorkflowDraft{Goal: "move box", Tasks: []TaskDraft{{ID: "one", RequiredRole: "robot", Goal: "move box"}}}
	first, reused, err := st.SubmitPlanProposalForRun("run-replay-a", project.ID, session.ID,
		draft, "v1", nil, "# v1", now)
	if err != nil || reused || first.Revision != 1 {
		t.Fatalf("首次提交: proposal=%+v reused=%v err=%v", first, reused, err)
	}
	if _, _, err := st.SubmitPlanProposalForRun("run-replay-b", project.ID, session.ID,
		draft, "v2", nil, "# v2", now.Add(time.Second)); err != nil {
		t.Fatalf("另一 Run 修订失败: %v", err)
	}

	replay, reused, err := st.SubmitPlanProposalForRun("run-replay-a", project.ID, session.ID,
		draft, "v3", nil, "# v3", now.Add(2*time.Second))
	if err != nil || !reused {
		t.Fatalf("重放应复用同一提案: reused=%v err=%v", reused, err)
	}
	if replay.Revision != 2 || replay.DocumentMarkdown != "# v2" || replay.Summary != "v2" {
		t.Fatalf("重放应返回库中当前版本: revision=%d summary=%q doc=%q",
			replay.Revision, replay.Summary, replay.DocumentMarkdown)
	}
	// 返回的 revision 与内容同版本，因此可以直接用于批准。
	if _, err := st.ApprovePlanProposal(replay.ID, replay.Revision, now.Add(3*time.Second)); err != nil {
		t.Fatalf("重放返回的 revision 必须可直接批准: %v", err)
	}

	approved, reused, err := st.SubmitPlanProposalForRun("run-replay-a", project.ID, session.ID,
		draft, "v4", nil, "# v4", now.Add(4*time.Second))
	if err != nil || !reused {
		t.Fatalf("批准后重放: reused=%v err=%v", reused, err)
	}
	if approved.Status != PlanProposalStatusApproved || approved.Revision != 3 {
		t.Fatalf("批准后重放应反映当前状态而非旧 revision: %+v", approved)
	}
}

func TestPlanProposalApprovalRemapsIDsAlreadyUsedByAnotherWorkflow(t *testing.T) {
	st := openTestStore(t)
	project, session := v030ProjectConversation(t, st)
	now := time.Now().UTC()
	draft := WorkflowDraft{
		Goal: "重复执行同一业务计划",
		Tasks: []TaskDraft{
			{ID: "task-local-a", RequiredRole: "developer", Goal: "步骤 A", SubTasks: []SubTaskDraft{
				{ID: "subtask-local-a", Kind: "agent_step", Goal: "执行 A"},
			}},
			{ID: "task-local-b", RequiredRole: "monitor", Goal: "步骤 B"},
		},
		Dependencies: []TaskDependency{{TaskID: "task-local-b", DependsOnTaskID: "task-local-a"}},
	}
	firstProposal, err := st.SubmitPlanProposal(project.ID, session.ID, draft, "", nil,
		"# 第一次", now)
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.ApprovePlanProposal(firstProposal.ID, firstProposal.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.TransitionWorkflow(first.Workflow.ID, first.Workflow.Revision,
		WorkflowStatusFailed, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	secondProposal, err := st.SubmitPlanProposal(project.ID, session.ID, draft, "", nil,
		"# 第二次", now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.ApprovePlanProposal(secondProposal.ID, secondProposal.Revision,
		now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("相同局部 ID 的计划应能再次批准: %v", err)
	}
	if second.Tasks[0].ID == first.Tasks[0].ID || second.SubTasks[0].ID == first.SubTasks[0].ID {
		t.Fatalf("已占用的局部 ID 必须映射为新的正式 ID: first=%+v second=%+v", first, second)
	}
	if len(second.Dependencies) != 1 ||
		second.Dependencies[0].TaskID != second.Tasks[1].ID ||
		second.Dependencies[0].DependsOnTaskID != second.Tasks[0].ID {
		t.Fatalf("正式 ID 映射后必须保持 Task 依赖: %+v", second.Dependencies)
	}
}

func TestPlanProposalRevisionReplacesReadyProposalAndPreservesLastValidRevision(t *testing.T) {
	st := openTestStore(t)
	project, session := v030ProjectConversation(t, st)
	now := time.Now().UTC()
	first := WorkflowDraft{Goal: "搬运 box-17", Tasks: []TaskDraft{{
		ID: "task-move", RequiredRole: "robot", Goal: "搬运 box-17",
	}}}
	proposal, err := st.SubmitPlanProposal(project.ID, session.ID, first, "", nil,
		"# 第一版", now)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.Goal = "搬运 box-17 并验证"
	updated, err := st.SubmitPlanProposal(project.ID, session.ID, second, "", nil,
		"# 第二版", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != proposal.ID || updated.Revision != proposal.Revision+1 ||
		updated.Goal != second.Goal {
		t.Fatalf("同一 Conversation 应原位更新 Proposal: before=%+v after=%+v", proposal, updated)
	}

	cyclic := WorkflowDraft{Goal: "非法版本", Tasks: []TaskDraft{
		{ID: "task-a", RequiredRole: "developer", Goal: "A"},
		{ID: "task-b", RequiredRole: "developer", Goal: "B"},
	}, Dependencies: []TaskDependency{
		{TaskID: "task-a", DependsOnTaskID: "task-b"},
		{TaskID: "task-b", DependsOnTaskID: "task-a"},
	}}
	if _, err := st.SubmitPlanProposal(project.ID, session.ID, cyclic, "", nil,
		"# 非法版本", now.Add(2*time.Second)); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("循环依赖必须在提交前拒绝: %v", err)
	}
	current, err := st.GetActivePlanProposal(project.ID)
	if err != nil || current.Revision != updated.Revision || current.DocumentMarkdown != "# 第二版" {
		t.Fatalf("失败提交不得损坏上一 revision: proposal=%+v err=%v", current, err)
	}
}

func approveSingleTaskProposal(t *testing.T, st *Store, project Project,
	session ChatSession, goal, role string, now time.Time) WorkflowView {
	t.Helper()
	draft := WorkflowDraft{Goal: goal, Tasks: []TaskDraft{{
		ID: "task-main", RequiredRole: role, Goal: goal,
	}}}
	proposal, err := st.SubmitPlanProposal(project.ID, session.ID, draft, "", nil,
		"# "+goal, now)
	if err != nil {
		t.Fatal(err)
	}
	view, err := st.ApprovePlanProposal(proposal.ID, proposal.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func TestTaskAgentPlansTypedSubTasksOnceAndDependenciesGateExecution(t *testing.T) {
	st := openTestStore(t)
	project, session := v030ProjectConversation(t, st)
	now := time.Now().UTC()
	view := approveSingleTaskProposal(t, st, project, session,
		"搬运一个箱子", "robot", now)
	task := view.Tasks[0]
	plannedTask, subtasks, err := st.SetTaskSubTasks(task.ID, task.Revision, []SubTaskDraft{
		{ID: "subtask-reach", Kind: "robot_skill", Goal: "到达抓取区域",
			Spec: json.RawMessage(`{"skill_name":"semantic-navigation","skill_version":"0.1.0","input":{}}`)},
		{ID: "subtask-grasp", Kind: "robot_skill", Goal: "抓取并确认持物",
			Spec:      json.RawMessage(`{"skill_name":"grasp-object","skill_version":"0.1.0","input":{}}`),
			DependsOn: []string{"subtask-reach"}},
	}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if plannedTask.Revision != task.Revision+1 || len(subtasks) != 2 ||
		subtasks[1].Kind != "robot_skill" {
		t.Fatalf("SubTask 计划未按类型持久化: task=%+v subtasks=%+v", plannedTask, subtasks)
	}
	if _, _, err := st.SetTaskSubTasks(task.ID, plannedTask.Revision,
		[]SubTaskDraft{{Goal: "重复规划"}}, now.Add(2*time.Second)); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("一个 Task 只能写入一次初始 SubTask 计划: %v", err)
	}
	runnable, err := st.ListRunnableSubTasks(task.ID)
	if err != nil || len(runnable) != 1 || runnable[0].ID != "subtask-reach" {
		t.Fatalf("依赖未满足时只能运行首个 SubTask: runnable=%+v err=%v", runnable, err)
	}
	running, err := st.TransitionSubTask(runnable[0].ID, runnable[0].Revision,
		TaskStatusRunning, "", nil, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionSubTask(running.ID, running.Revision,
		TaskStatusCompleted, "", json.RawMessage(`{"arrived":true}`), now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	runnable, err = st.ListRunnableSubTasks(task.ID)
	if err != nil || len(runnable) != 1 || runnable[0].ID != "subtask-grasp" {
		t.Fatalf("前置完成后应释放后继 SubTask: runnable=%+v err=%v", runnable, err)
	}
}

func TestTaskAgentSubTaskLocalIDsCanBeReusedByAnotherWorkflow(t *testing.T) {
	st := openTestStore(t)
	project, session := v030ProjectConversation(t, st)
	now := time.Now().UTC()
	drafts := []SubTaskDraft{
		{ID: "subtask-local-observe", Kind: "robot_skill", Goal: "观察目标",
			Spec: json.RawMessage(`{"skill_name":"observe","input":{}}`)},
		{ID: "subtask-local-move", Kind: "robot_skill", Goal: "搬运目标",
			Spec:      json.RawMessage(`{"skill_name":"move","input":{}}`),
			DependsOn: []string{"subtask-local-observe"}},
	}

	first := approveSingleTaskProposal(t, st, project, session,
		"第一次搬运", "robot", now)
	_, firstSubTasks, err := st.SetTaskSubTasks(first.Tasks[0].ID,
		first.Tasks[0].Revision, drafts, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.TransitionWorkflow(first.Workflow.ID, first.Workflow.Revision,
		WorkflowStatusFailed, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	second := approveSingleTaskProposal(t, st, project, session,
		"第二次搬运", "robot", now.Add(3*time.Second))
	_, secondSubTasks, err := st.SetTaskSubTasks(second.Tasks[0].ID,
		second.Tasks[0].Revision, drafts, now.Add(4*time.Second))
	if err != nil {
		t.Fatalf("不同 Workflow 应能复用 Task Agent 输出的局部 SubTask ID: %v", err)
	}
	if secondSubTasks[0].ID == firstSubTasks[0].ID || secondSubTasks[1].ID == firstSubTasks[1].ID {
		t.Fatalf("局部 SubTask ID 必须映射为新的正式 ID: first=%+v second=%+v",
			firstSubTasks, secondSubTasks)
	}
	secondView, err := st.GetWorkflowView(second.Workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(secondView.SubTaskDependencies) != 1 ||
		secondView.SubTaskDependencies[0].SubTaskID != secondSubTasks[1].ID ||
		secondView.SubTaskDependencies[0].DependsOnSubTaskID != secondSubTasks[0].ID {
		t.Fatalf("正式 ID 映射后必须保持当前 Workflow 内的 SubTask 依赖: %+v",
			secondView.SubTaskDependencies)
	}
}

func TestTaskAgentRejectsSubTaskDependencyCycle(t *testing.T) {
	st := openTestStore(t)
	project, session := v030ProjectConversation(t, st)
	view := approveSingleTaskProposal(t, st, project, session,
		"循环 SubTask", "developer", time.Now().UTC())
	_, _, err := st.SetTaskSubTasks(view.Tasks[0].ID, view.Tasks[0].Revision, []SubTaskDraft{
		{ID: "sub-a", Goal: "A", DependsOn: []string{"sub-b"}},
		{ID: "sub-b", Goal: "B", DependsOn: []string{"sub-a"}},
	}, time.Now().UTC())
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("SubTask 循环依赖必须拒绝: %v", err)
	}
}

func TestActiveRobotReservationPersistsInTaskStore(t *testing.T) {
	st := openTestStore(t)
	project, session := v030ProjectConversation(t, st)
	now := time.Now().UTC()
	ready, err := st.SubmitPlanProposal(project.ID, session.ID, WorkflowDraft{
		Goal: "并行搬运", Tasks: []TaskDraft{
			{ID: "task-reserve-a", RequiredRole: "robot", Goal: "搬运 A"},
			{ID: "task-reserve-b", RequiredRole: "robot", Goal: "搬运 B"},
		},
	}, "", nil, "# 并行搬运", now)
	if err != nil {
		t.Fatal(err)
	}
	view, err := st.ApprovePlanProposal(ready.ID, ready.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	first, second := view.Tasks[0], view.Tasks[1]
	first, err = st.AssignTask(first.ID, first.Revision,
		"robot:r1pro-shared", "r1pro-shared", now)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := st.IsRobotTaskReserved("r1pro-shared", second.ID)
	if err != nil || !reserved {
		t.Fatalf("非终态 Task 的 Robot 保留未持久化: reserved=%v err=%v", reserved, err)
	}
	if _, err := st.AssignTask(second.ID, second.Revision,
		"robot:r1pro-shared", "r1pro-shared", now); !errors.Is(err, ErrRobotReserved) {
		t.Fatalf("同一 Robot 不得分配给第二条非终态 Task: %v", err)
	}
	first, err = st.TransitionTask(first.ID, first.Revision, TaskStatusStopped,
		"test_finished", "", nil, now.Add(time.Second))
	if err != nil || first.Status != TaskStatusStopped {
		t.Fatalf("结束首条 Task 失败: %+v %v", first, err)
	}
	second, err = st.AssignTask(second.ID, second.Revision,
		"robot:r1pro-shared", "r1pro-shared", now.Add(2*time.Second))
	if err != nil || second.AssignedRobotID != "r1pro-shared" {
		t.Fatalf("Task 终态后应释放 Robot 保留: %+v %v", second, err)
	}
}
