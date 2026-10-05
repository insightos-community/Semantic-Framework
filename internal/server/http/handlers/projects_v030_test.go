package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/log"
)

type fakeWorkflowApplication struct {
	st      *store.Store
	actions []string
	// stopErr 让测试驱动应用服务返回特定错误，验证 HTTP 层的错误码映射。
	stopErr error
}

func (f *fakeWorkflowApplication) ApprovePlanProposal(_ context.Context, _, projectID,
	proposalID string, revision int64) (store.WorkflowView, error) {
	proposal, err := f.st.GetPlanProposal(proposalID)
	if err != nil || proposal.ProjectID != projectID {
		return store.WorkflowView{}, store.ErrNotFound
	}
	return f.st.ApprovePlanProposal(proposalID, revision, time.Now().UTC())
}

func (f *fakeWorkflowApplication) DiscardPlanProposal(_, projectID, proposalID string,
	revision int64) (store.PlanProposal, error) {
	proposal, err := f.st.GetPlanProposal(proposalID)
	if err != nil || proposal.ProjectID != projectID {
		return store.PlanProposal{}, store.ErrNotFound
	}
	return f.st.DiscardPlanProposal(proposalID, revision, time.Now().UTC())
}

func (f *fakeWorkflowApplication) transition(projectID, workflowID string, revision int64,
	status, action string) (store.WorkflowView, error) {
	f.actions = append(f.actions, action)
	workflow, err := f.st.GetWorkflow(workflowID)
	if err != nil || workflow.ProjectID != projectID {
		return store.WorkflowView{}, store.ErrNotFound
	}
	updated, err := f.st.TransitionWorkflow(workflowID, revision, status, time.Now().UTC())
	if err != nil {
		return store.WorkflowView{}, err
	}
	return f.st.GetWorkflowView(updated.ID)
}

func (f *fakeWorkflowApplication) PauseWorkflow(_ context.Context, _, projectID, workflowID string,
	revision int64) (store.WorkflowView, error) {
	return f.transition(projectID, workflowID, revision, store.WorkflowStatusPaused, "pause")
}

func (f *fakeWorkflowApplication) ResumeWorkflow(_ context.Context, _, projectID, workflowID string,
	revision int64) (store.WorkflowView, error) {
	return f.transition(projectID, workflowID, revision, store.WorkflowStatusRunning, "resume")
}

func (f *fakeWorkflowApplication) RetryRobotAgentDecision(_ context.Context, _, projectID, workflowID string,
	revision int64) (store.WorkflowView, error) {
	return f.transition(projectID, workflowID, revision, store.WorkflowStatusRunning, "retry-decision")
}

func (f *fakeWorkflowApplication) StopWorkflow(_ context.Context, _, projectID, workflowID string,
	revision int64) (store.WorkflowView, error) {
	if f.stopErr != nil {
		f.actions = append(f.actions, "stop")
		return store.WorkflowView{}, f.stopErr
	}
	workflow, err := f.st.GetWorkflow(workflowID)
	if err != nil {
		return store.WorkflowView{}, err
	}
	to := store.WorkflowStatusStopping
	if workflow.Status == store.WorkflowStatusPending {
		to = store.WorkflowStatusStopped
	}
	view, err := f.transition(projectID, workflowID, revision, to, "stop")
	if err != nil || to == store.WorkflowStatusStopped {
		return view, err
	}
	updated, err := f.st.TransitionWorkflow(workflowID, view.Workflow.Revision,
		store.WorkflowStatusStopped, time.Now().UTC())
	if err != nil {
		return store.WorkflowView{}, err
	}
	return f.st.GetWorkflowView(updated.ID)
}

func (f *fakeWorkflowApplication) ConfirmWorkflowStop(
	_ context.Context, _, projectID, workflowID string, revision int64,
	physicalStateConfirmed bool, reason string,
) (store.WorkflowView, error) {
	if !physicalStateConfirmed || reason == "" {
		return store.WorkflowView{}, store.ErrInvalidState
	}
	view, err := f.transition(projectID, workflowID, revision,
		store.WorkflowStatusStopping, "confirm-stop")
	if err != nil {
		return store.WorkflowView{}, err
	}
	updated, err := f.st.TransitionWorkflow(workflowID, view.Workflow.Revision,
		store.WorkflowStatusStopped, time.Now().UTC())
	if err != nil {
		return store.WorkflowView{}, err
	}
	return f.st.GetWorkflowView(updated.ID)
}

func newV030HTTPTest(t *testing.T, withApplication bool) (*store.Store, *fakeWorkflowApplication,
	http.Handler, store.Project, store.ChatSession) {
	t.Helper()
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
	st := storetest.OpenMigrated(t, logger)
	project, err := st.CreateProject("usr-v030-http", "v0.3 HTTP")
	if err != nil {
		t.Fatalf("创建 Project 失败: %v", err)
	}
	now := time.Now().UTC()
	session := store.ChatSession{ID: "cs-v030-http", UserID: project.OwnerID, ProjectID: project.ID,
		Title: "Plan", CreatedAt: now, UpdatedAt: now}
	if err := st.CreateChatSession(session); err != nil {
		t.Fatalf("创建 Conversation 失败: %v", err)
	}
	application := &fakeWorkflowApplication{st: st}
	handler := NewProjectsHandler(st, nil, logger)
	if withApplication {
		handler.SetWorkflowApplication(application)
	}
	router := chi.NewRouter()
	router.Route("/api/v1/projects", func(router chi.Router) {
		router.Get("/{id}/studio/snapshot", handler.HandleStudioSnapshotV030)
		router.Get("/{id}/plan-proposals/active", handler.HandleGetActivePlanProposal)
		router.Get("/{id}/plan-proposals/{proposal_id}", handler.HandleGetPlanProposal)
		router.Post("/{id}/plan-proposals/{proposal_id}/{action}",
			handler.HandlePlanProposalAction)
		router.Get("/{id}/workflows", handler.HandleListWorkflows)
		router.Get("/{id}/workflows/active", handler.HandleGetActiveWorkflow)
		router.Get("/{id}/workflows/{workflow_id}/view", handler.HandleGetWorkflowView)
		router.Post("/{id}/workflows/{workflow_id}/{action}", handler.HandleWorkflowAction)
		router.Get("/{id}/maps/{map_id}", handler.HandleGetSemanticMap)
		router.Post("/{id}/maps/{map_id}/query", handler.HandleQuerySemanticMap)
		router.Post("/{id}/maps/{map_id}/generations", handler.HandleCreateMapGeneration)
		router.Post("/{id}/maps/{map_id}/updates", handler.HandleUpdateSemanticMap)
	})
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		ctx := auth.ContextWithUserID(request.Context(), project.OwnerID)
		router.ServeHTTP(w, request.WithContext(ctx))
	})
	return st, application, wrapped, project, session
}

func workflowURL(projectID string) string {
	return "/api/v1/projects/" + projectID + "/workflows"
}

func TestV030WorkflowHTTPUsesApplicationAndProjectBoundary(t *testing.T) {
	unavailableStore, _, unavailable, project, session := newV030HTTPTest(t, false)
	var code int
	unavailableDraft := store.WorkflowDraft{Goal: "等待服务", Tasks: []store.TaskDraft{{
		ID: "task-unavailable", RequiredRole: "developer", Goal: "等待",
		SubTasks: []store.SubTaskDraft{{ID: "subtask-unavailable", Kind: "agent_step", Goal: "等待"}},
	}}}
	unavailableReady, err := unavailableStore.SubmitPlanProposal(project.ID, session.ID,
		unavailableDraft, "", nil, "# 等待服务", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	unavailableView, err := unavailableStore.ApprovePlanProposal(unavailableReady.ID,
		unavailableReady.Revision, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"pause", "resume", "stop"} {
		code, _ = doJSON(t, unavailable, http.MethodPost,
			workflowURL(project.ID)+"/"+unavailableView.Workflow.ID+"/"+action, "",
			fmt.Sprintf(`{"revision":%d}`, unavailableView.Workflow.Revision))
		if code != http.StatusServiceUnavailable {
			t.Fatalf("未装配 Workflow 服务时 %s 应返回 503，实际 %d", action, code)
		}
	}

	st, application, router, project, session := newV030HTTPTest(t, true)
	ready, err := st.SubmitPlanProposal(project.ID, session.ID, store.WorkflowDraft{
		Goal:  "实现功能",
		Tasks: []store.TaskDraft{{ID: "task-http", RequiredRole: "developer", Goal: "实现功能"}},
	}, "", nil, "# 计划", time.Now().UTC())
	if err != nil {
		t.Fatalf("准备 ready Proposal 失败: %v", err)
	}
	if workflows, err := st.ListWorkflows(project.ID, false); err != nil || len(workflows) != 0 {
		t.Fatalf("批准前不得存在 Workflow: workflows=%+v err=%v", workflows, err)
	}
	code, body := doJSON(t, router, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/plan-proposals/"+ready.ID+"/approve", "",
		fmt.Sprintf(`{"revision":%d}`, ready.Revision))
	if code != http.StatusOK {
		t.Fatalf("批准 Proposal 应成功: status=%d body=%+v", code, body)
	}
	view := body["workflow_view"].(map[string]any)
	workflow := view["workflow"].(map[string]any)
	workflowID := workflow["id"].(string)
	revision := int64(workflow["revision"].(float64))
	taskView := view["tasks"].([]any)[0].(map[string]any)
	sourceRevision := int64(taskView["revision"].(float64))

	other, err := st.CreateProject("usr-other-v030", "other")
	if err != nil {
		t.Fatalf("创建其他 Project 失败: %v", err)
	}
	code, _ = doJSON(t, router, http.MethodGet,
		workflowURL(other.ID)+"/"+workflowID+"/view", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("跨 Project 读取 Workflow 应为 404，实际 %d", code)
	}
	future := time.Now().UTC().Add(time.Hour)
	if err := st.CreateInteraction(store.Interaction{
		ID: "int-v030-http", ProjectID: project.ID, SessionID: session.ID,
		WorkflowID: workflowID, TaskID: "task-http", UIKind: store.InteractionUIForm,
		SourceRevision: sourceRevision, TargetAgentID: "developer", Type: store.InteractionTypeInput,
		Status: store.InteractionStatusPending, Agent: "developer",
		Payload: `{"prompt":"填写参数","data":{}}`, ResponseSchema: `{"type":"object"}`,
		MapID: store.MapSlotSimulation, MapGeneration: 1, CreatedAt: time.Now().UTC(),
		ExpiresAt: &future,
	}); err != nil {
		t.Fatalf("准备结构化 Interaction 失败: %v", err)
	}
	if err := st.SaveRobotExecution(store.RobotExecution{
		ID: "rex-v050-snapshot", ProjectID: project.ID, RobotID: "r1pro-test",
		PilotInstanceID: "pilot-test", SkillName: "grasp-object", SkillVersion: "0.1.0",
		RequestKey: "snapshot-robot", Status: "completed", Revision: 1,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("准备 Robot Execution 失败: %v", err)
	}
	code, body = doJSON(t, router, http.MethodGet,
		"/api/v1/projects/"+project.ID+"/studio/snapshot", "", "")
	if code != http.StatusOK {
		t.Fatalf("读取 v0.3 Snapshot 失败: %d %+v", code, body)
	}
	snapshot := body["snapshot"].(map[string]any)
	workflows := snapshot["workflows"].([]any)
	if len(workflows) != 1 || workflows[0].(map[string]any)["id"] != workflowID {
		t.Fatalf("Snapshot 应恢复未结束 Workflow 摘要: %+v", workflows)
	}
	if int(snapshot["snapshot_version"].(float64)) != 3 || len(snapshot["robot_executions"].([]any)) != 1 {
		t.Fatalf("Snapshot 应恢复 Robot Execution: %+v", snapshot)
	}
	pending := snapshot["pending_interactions"].([]any)
	if len(pending) != 1 {
		t.Fatalf("Snapshot 应恢复 pending Interaction: %+v", snapshot)
	}
	interaction := pending[0].(map[string]any)
	if interaction["workflow_id"] != workflowID || interaction["task_id"] != "task-http" ||
		interaction["ui_kind"] != store.InteractionUIForm ||
		int64(interaction["source_revision"].(float64)) != sourceRevision ||
		interaction["map_id"] != store.MapSlotSimulation {
		t.Fatalf("Snapshot 结构化 Interaction 字段不完整: %+v", interaction)
	}
	if err := st.AnswerInteractionAtRevision("int-v030-http", `{"value":"ok"}`, sourceRevision,
		time.Now().UTC()); err != nil {
		t.Fatalf("结束测试 Interaction 失败: %v", err)
	}

	for _, action := range []string{"pause", "resume", "stop"} {
		code, body = doJSON(t, router, http.MethodPost,
			workflowURL(project.ID)+"/"+workflowID+"/"+action, "",
			fmt.Sprintf(`{"revision":%d}`, revision))
		if code != http.StatusOK {
			t.Fatalf("%s 应通过应用服务: status=%d body=%+v", action, code, body)
		}
		view = body["workflow_view"].(map[string]any)
		revision = int64(view["workflow"].(map[string]any)["revision"].(float64))
	}
	if fmt.Sprint(application.actions) != "[pause resume stop]" {
		t.Fatalf("Workflow 操作未完整进入应用服务: %+v", application.actions)
	}
	nextProject, err := st.CreateProject(project.OwnerID, "next active")
	if err != nil {
		t.Fatalf("创建下一个 Project 失败: %v", err)
	}
	if _, err := st.ActivateProject(project.OwnerID, nextProject.ID); err != nil {
		t.Fatalf("切换活动 Project 失败: %v", err)
	}
	code, _ = doJSON(t, router, http.MethodPost,
		workflowURL(project.ID)+"/"+workflowID+"/pause", "",
		fmt.Sprintf(`{"revision":%d}`, revision))
	if code != http.StatusConflict {
		t.Fatalf("非活动 Project 的 Workflow 更新应为 409，实际 %d", code)
	}
}

func TestV030StopWorkflowSurfacesOperatorConfirmationRequirement(t *testing.T) {
	st, application, router, project, session := newV030HTTPTest(t, true)
	ready, err := st.SubmitPlanProposal(project.ID, session.ID, store.WorkflowDraft{
		Goal: "物理状态未知", Tasks: []store.TaskDraft{{
			ID: "task-unknown-http", RequiredRole: "robot", Goal: "停止未知执行",
		}},
	}, "", nil, "# 物理状态未知", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	view, err := st.ApprovePlanProposal(ready.ID, ready.Revision, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	// 应用服务在"物理状态未知"时返回明确哨兵错误；HTTP 层必须把它映射成可分支
	// 的错误码，前端才能打开人工安全确认，而不是把停止被回退当成成功。
	application.stopErr = store.ErrOperatorConfirmationRequired
	url := workflowURL(project.ID) + "/" + view.Workflow.ID + "/stop"
	code, body := doJSON(t, router, http.MethodPost, url, "",
		fmt.Sprintf(`{"revision":%d}`, view.Workflow.Revision))
	if code != http.StatusConflict {
		t.Fatalf("物理状态未知的停止应返回 409，实际 %d body=%+v", code, body)
	}
	errBody, _ := body["error"].(map[string]any)
	if errBody["code"] != "OPERATOR_CONFIRMATION_REQUIRED" {
		t.Fatalf("停止错误码必须可被前端分支识别: %+v", body)
	}
}

func TestV030ConfirmWorkflowStopValidatesSafetyRevisionAndProject(t *testing.T) {
	st, application, router, project, session := newV030HTTPTest(t, true)
	ready, err := st.SubmitPlanProposal(project.ID, session.ID, store.WorkflowDraft{
		Goal: "恢复异常停止", Tasks: []store.TaskDraft{{
			ID: "task-confirm-http", RequiredRole: "robot", Goal: "停止机器人",
		}},
	}, "", nil, "# 恢复异常停止", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	view, err := st.ApprovePlanProposal(ready.ID, ready.Revision, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	url := workflowURL(project.ID) + "/" + view.Workflow.ID + "/confirm-stop"
	code, _ := doJSON(t, router, http.MethodPost, url, "",
		fmt.Sprintf(`{"revision":%d,"physical_state_confirmed":false,"reason":"已检查"}`,
			view.Workflow.Revision))
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("缺少现场安全确认应返回 422，实际 %d", code)
	}
	code, _ = doJSON(t, router, http.MethodPost, url, "",
		fmt.Sprintf(`{"revision":%d,"physical_state_confirmed":true,"reason":"现场已安全"}`,
			view.Workflow.Revision+1))
	if code != http.StatusConflict {
		t.Fatalf("错误 revision 应返回 409，实际 %d", code)
	}
	code, body := doJSON(t, router, http.MethodPost, url, "",
		fmt.Sprintf(`{"revision":%d,"physical_state_confirmed":true,"reason":"现场已安全"}`,
			view.Workflow.Revision))
	if code != http.StatusOK ||
		body["workflow_view"].(map[string]any)["workflow"].(map[string]any)["status"] !=
			store.WorkflowStatusStopped {
		t.Fatalf("合法人工确认没有进入应用服务: status=%d body=%+v", code, body)
	}
	if len(application.actions) != 2 || application.actions[0] != "confirm-stop" ||
		application.actions[1] != "confirm-stop" {
		// revision 冲突与成功请求都会进入应用服务；缺少确认的请求不会进入。
		t.Fatalf("confirm-stop 路由次数错误: %+v", application.actions)
	}

	other, err := st.CreateProject(project.OwnerID, "另一个 Project")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ActivateProject(project.OwnerID, other.ID); err != nil {
		t.Fatal(err)
	}
	crossURL := workflowURL(other.ID) + "/" + view.Workflow.ID + "/confirm-stop"
	code, _ = doJSON(t, router, http.MethodPost, crossURL, "",
		fmt.Sprintf(`{"revision":%d,"physical_state_confirmed":true,"reason":"现场已安全"}`,
			view.Workflow.Revision))
	if code != http.StatusNotFound {
		t.Fatalf("跨 Project 人工确认应返回 404，实际 %d", code)
	}
}

func TestV030MapHTTPIsolationRevisionGenerationAndSubset(t *testing.T) {
	st, _, router, project, session := newV030HTTPTest(t, true)
	base := "/api/v1/projects/" + project.ID + "/maps/simulation_map"
	code, body := doJSON(t, router, http.MethodPost, base+"/updates", "", `{
		"generation":1,"expected_revision":0,
		"entities":[
			{"id":"entity-a","type":"box","name":"A","status":"active","frame_id":"world","pose":{"position":{"x":0,"y":0,"z":0},"orientation":{"x":0,"y":0,"z":0,"w":1}},"bounds":{"kind":"box","size":{"x":1,"y":1,"z":1}}},
			{"id":"entity-b","type":"box","name":"B","status":"active","frame_id":"world","pose":{"position":{"x":1,"y":0,"z":0},"orientation":{"x":0,"y":0,"z":0,"w":1}},"bounds":{"kind":"box","size":{"x":1,"y":1,"z":1}}},
			{"id":"entity-c","type":"marker","name":"C","status":"active","frame_id":"world","pose":{"position":{"x":2,"y":0,"z":0},"orientation":{"x":0,"y":0,"z":0,"w":1}},"bounds":{"kind":"box","size":{"x":1,"y":1,"z":1}}}
		],
		"relations":[
			{"id":"relation-ab","subject_id":"entity-a","predicate":"inside","object_id":"entity-b"},
			{"id":"relation-bc","subject_id":"entity-b","predicate":"on","object_id":"entity-c"}
		]}`)
	if code != http.StatusOK {
		t.Fatalf("地图更新应成功: status=%d body=%+v", code, body)
	}
	bound, err := st.SubmitPlanProposal(project.ID, session.ID, store.WorkflowDraft{
		Goal: "使用实体 A", MapScope: json.RawMessage(`{"map_id":"simulation_map","generation":1,"selections":[{"kind":"entity","entity_id":"entity-a"}]}`),
		Tasks: []store.TaskDraft{{ID: "task-map", RequiredRole: "map", Goal: "使用实体 A"}},
	}, "", nil, "# 使用实体 A", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	boundProposalID := bound.ID
	boundRevision := bound.Revision
	code, body = doJSON(t, router, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/plan-proposals/"+boundProposalID+"/discard", "",
		fmt.Sprintf(`{"revision":%d}`, boundRevision))
	if code != http.StatusOK {
		t.Fatalf("丢弃地图 Proposal 失败: %d %+v", code, body)
	}

	code, body = doJSON(t, router, http.MethodPost, base+"/query", "",
		`{"generation":1,"entity_ids":["entity-a"]}`)
	if code != http.StatusOK {
		t.Fatalf("地图子集查询应成功: %d %+v", code, body)
	}
	view := body["map_view"].(map[string]any)
	if len(view["entities"].([]any)) != 1 || len(view["relations"].([]any)) != 0 {
		t.Fatalf("entity_ids 查询不能泄漏无关关系: %+v", view)
	}
	code, body = doJSON(t, router, http.MethodPost, base+"/query", "", `{"type":"box"}`)
	view = body["map_view"].(map[string]any)
	if code != http.StatusOK || len(view["entities"].([]any)) != 3 || len(view["relations"].([]any)) != 2 {
		t.Fatalf("类型查询应补齐相连关系端点: status=%d view=%+v", code, view)
	}

	code, _ = doJSON(t, router, http.MethodPost, base+"/updates", "",
		`{"generation":1,"expected_revision":0,"remove_entity_ids":["entity-a"]}`)
	if code != http.StatusConflict {
		t.Fatalf("过期地图 revision 应为 409，实际 %d", code)
	}
	code, _ = doJSON(t, router, http.MethodPost, base+"/updates", "",
		`{"generation":2,"expected_revision":1,"remove_entity_ids":["entity-a"]}`)
	if code != http.StatusConflict {
		t.Fatalf("错误 generation 应为 409，实际 %d", code)
	}

	other, err := st.CreateProject("usr-map-other", "other map")
	if err != nil {
		t.Fatalf("创建其他 Project 失败: %v", err)
	}
	code, _ = doJSON(t, router, http.MethodGet,
		"/api/v1/projects/"+other.ID+"/maps/simulation_map", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("跨 Project 地图读取应为 404，实际 %d", code)
	}
	code, _ = doJSON(t, router, http.MethodGet,
		"/api/v1/projects/"+project.ID+"/maps/unknown", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("未知 map_id 应为 404，实际 %d", code)
	}
	inactive, err := st.CreateProject(project.OwnerID, "inactive map")
	if err != nil {
		t.Fatalf("创建非活动 Project 失败: %v", err)
	}
	inactiveBase := "/api/v1/projects/" + inactive.ID + "/maps/simulation_map"
	code, _ = doJSON(t, router, http.MethodGet, inactiveBase, "", "")
	if code != http.StatusOK {
		t.Fatalf("非活动 Project 地图仍应可只读，实际 %d", code)
	}
	code, _ = doJSON(t, router, http.MethodPost, inactiveBase+"/updates", "",
		`{"generation":1,"expected_revision":0,"entities":[{"id":"blocked","type":"box","name":"blocked","status":"active","frame_id":"world","pose":{"orientation":{"w":1}},"bounds":{"kind":"box","size":{"x":1,"y":1,"z":1}}}]}`)
	if code != http.StatusConflict {
		t.Fatalf("非活动 Project 地图写入应为 409，实际 %d", code)
	}
	archived, err := st.ArchiveProject(project.OwnerID, inactive.ID, inactive.Revision)
	if err != nil || archived.ArchivedAt == nil {
		t.Fatalf("准备归档 Project 失败: %+v %v", archived, err)
	}
	code, _ = doJSON(t, router, http.MethodPost, inactiveBase+"/generations", "",
		`{"expected_revision":0,"reason":"blocked"}`)
	if code != http.StatusGone {
		t.Fatalf("归档 Project 地图写入应为 410，实际 %d", code)
	}

	code, body = doJSON(t, router, http.MethodPost, base+"/generations", "",
		`{"expected_revision":1,"reason":"reset"}`)
	if code != http.StatusCreated {
		t.Fatalf("新 generation 应创建成功: %d %+v", code, body)
	}
	code, _ = doJSON(t, router, http.MethodPost, base+"/updates", "",
		`{"generation":1,"expected_revision":1,"remove_entity_ids":["entity-a"]}`)
	if code != http.StatusConflict {
		t.Fatalf("旧 generation 更新应为 409，实际 %d", code)
	}
	code, _ = doJSON(t, router, http.MethodPost, base+"/updates", "",
		`{"generation":2,"expected_revision":0}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("空地图更新应为 422，实际 %d", code)
	}
}
