package handlers

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/log"
)

type fakeProjectRuntime struct {
	st          *store.Store
	initialized []string
}

func (f *fakeProjectRuntime) InitializeSessionModels(sessionID string) error {
	f.initialized = append(f.initialized, sessionID)
	return nil
}

func (f *fakeProjectRuntime) CancelRunByID(_ context.Context, _, projectID, runID string) error {
	if err := f.st.RunBelongsToProject("usr-project", projectID, runID); err != nil {
		return err
	}
	_, err := f.st.TransitionRunStatus(runID,
		[]string{store.RunStatusQueued, store.RunStatusRunning,
			store.RunStatusWaitingInput},
		store.RunStatusCancelling, time.Now().UTC())
	return err
}

func newProjectsTestRouter(t *testing.T, lifecycles ...ProjectSimulationLifecycle) (*store.Store, *fakeProjectRuntime,
	http.Handler, <-chan event.Event) {
	t.Helper()
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
	st := storetest.OpenMigrated(t, logger)
	runtime := &fakeProjectRuntime{st: st}
	bus := event.NewBus(logger)
	events := bus.Subscribe(event.TopicAgentEvents)
	handler := NewProjectsHandler(st, runtime, logger, bus)
	if len(lifecycles) > 0 {
		handler.SetSimulationLifecycle(lifecycles[0])
	}
	router := chi.NewRouter()
	router.Route("/api/v1", func(router chi.Router) {
		router.Route("/projects", func(router chi.Router) {
			router.Get("/", handler.HandleListProjects)
			router.Post("/", handler.HandleCreateProject)
			router.Get("/{id}", handler.HandleGetProject)
			router.Patch("/{id}", handler.HandleUpdateProject)
			router.Delete("/{id}", handler.HandleArchiveProject)
			router.Post("/{id}/activate", handler.HandleActivateProject)
			router.Get("/{id}/bindings", handler.HandleGetProjectBindings)
			router.Put("/{id}/bindings", handler.HandleReplaceProjectBindings)
			router.Get("/{id}/memory", handler.HandleGetMemory)
			router.Put("/{id}/memory", handler.HandleSaveMemory)
			router.Get("/{id}/conversations", handler.HandleListConversations)
			router.Post("/{id}/conversations", handler.HandleCreateConversation)
			router.Delete("/{id}/conversations/{conversation_id}",
				handler.HandleArchiveConversation)
			router.Get("/{id}/runs", handler.HandleListRuns)
			router.Get("/{id}/studio/snapshot", handler.HandleStudioSnapshot)
		})
		router.Get("/runs/{id}", handler.HandleGetRun)
		router.Get("/runs/{id}/events", handler.HandleRunEvents)
		router.Post("/runs/{id}/cancel", handler.HandleCancelRun)
	})
	return st, runtime, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		ctx := auth.ContextWithUserID(request.Context(), "usr-project")
		router.ServeHTTP(w, request.WithContext(ctx))
	}), events
}

type recordingProjectLifecycle struct {
	released []string
}

func (l *recordingProjectLifecycle) ValidateRuntimePreference(_, _ string) error { return nil }

func (l *recordingProjectLifecycle) ReleaseProject(_ context.Context, projectID string) error {
	l.released = append(l.released, projectID)
	return nil
}

func TestActivateProjectKeepsBackgroundRuntime(t *testing.T) {
	lifecycle := &recordingProjectLifecycle{}
	st, _, router, _ := newProjectsTestRouter(t, lifecycle)
	first, err := st.CreateProject("usr-project", "后台运行")
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.CreateProject("usr-project", "当前查看")
	if err != nil {
		t.Fatal(err)
	}
	code, body := doJSON(t, router, http.MethodPost, "/api/v1/projects/"+second.ID+"/activate", "", "{}")
	if code != http.StatusOK {
		t.Fatalf("切换 Project 失败: %d %+v", code, body)
	}
	if len(lifecycle.released) != 0 {
		t.Fatalf("切换查看不应停止后台 Runtime: %v", lifecycle.released)
	}
	active, err := st.GetActiveProject("usr-project")
	if err != nil || active.ID != second.ID {
		t.Fatalf("活动 Project 未切换: %+v %v", active, err)
	}
	// 显式归档仍负责清理对应 Project，保留独立于工作区导航的资源生命周期。
	previous, err := st.GetProject(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	code, body = doJSON(t, router, http.MethodDelete, "/api/v1/projects/"+first.ID, "", `{"revision":`+strconv.FormatInt(previous.Revision, 10)+`}`)
	if code != http.StatusOK {
		t.Fatalf("归档失败: %d %+v", code, body)
	}
	if len(lifecycle.released) != 1 || lifecycle.released[0] != first.ID {
		t.Fatalf("归档应清理对应 Runtime: %v", lifecycle.released)
	}
}

func readProjectEvent(t *testing.T, events <-chan event.Event, eventType string) ws.Envelope {
	t.Helper()
	select {
	case value := <-events:
		envelope, ok := value.Payload.(ws.Envelope)
		if !ok || envelope.Type != eventType {
			t.Fatalf("期望 %s 资源事件，实际: %#v", eventType, value.Payload)
		}
		if envelope.ProjectID == "" || envelope.ResourceType == "" ||
			envelope.ResourceID == "" || envelope.Revision < 1 {
			t.Fatalf("资源事件定位字段不完整: %+v", envelope)
		}
		return envelope
	case <-time.After(2 * time.Second):
		t.Fatalf("2s 内未收到 %s 资源事件", eventType)
	}
	return ws.Envelope{}
}

func TestProjectsHTTPFlow(t *testing.T) {
	st, runtime, router, events := newProjectsTestRouter(t)

	code, body := doJSON(t, router, http.MethodPost, "/api/v1/projects",
		"", `{"name":"拆码垛"}`)
	if code != http.StatusCreated {
		t.Fatalf("创建 Project 应返回 201，实际: %d %+v", code, body)
	}
	projectBody := body["project"].(map[string]any)
	projectID := projectBody["id"].(string)
	revision := int64(projectBody["revision"].(float64))
	if active, _ := projectBody["is_active"].(bool); !active {
		t.Fatalf("首个用户 Project 应自动激活: %+v", projectBody)
	}
	createdEvent := readProjectEvent(t, events, "project.created")
	if createdEvent.ResourceType != "project" || createdEvent.ResourceID != projectID {
		t.Fatalf("Project 创建事件定位错误: %+v", createdEvent)
	}

	code, _ = doJSON(t, router, http.MethodPut,
		"/api/v1/projects/"+projectID+"/memory", "",
		`{"content":"# Memory\n\n- 使用箱体 A","revision":0}`)
	if code != http.StatusOK {
		t.Fatalf("保存 Memory 应返回 200，实际: %d", code)
	}
	if memoryEvent := readProjectEvent(t, events, "memory.updated"); memoryEvent.ResourceType != "project_memory" || memoryEvent.ResourceID != projectID {
		t.Fatalf("Memory 事件定位错误: %+v", memoryEvent)
	}
	code, body = doJSON(t, router, http.MethodPost,
		"/api/v1/projects/"+projectID+"/conversations", "",
		`{"title":"验证会话"}`)
	if code != http.StatusCreated {
		t.Fatalf("创建 Conversation 应返回 201，实际: %d %+v", code, body)
	}
	conversation := body["conversation"].(map[string]any)
	conversationID := conversation["id"].(string)
	if len(runtime.initialized) != 1 || runtime.initialized[0] != conversationID {
		t.Fatalf("应初始化 Conversation 模型: %+v", runtime.initialized)
	}
	conversationEvent := readProjectEvent(t, events, "conversation.created")
	if conversationEvent.SessionID != conversationID ||
		conversationEvent.ResourceID != conversationID {
		t.Fatalf("Conversation 事件定位错误: %+v", conversationEvent)
	}

	now := time.Now().UTC()
	if err := st.CreateRunSession(store.RunSession{
		ID: "run-http-v020", ChatSessionID: conversationID,
		AgentName: "leader", TraceID: "trace-http-v020",
		Status: store.RunStatusRunning, StartedAt: now,
	}); err != nil {
		t.Fatalf("准备 Run 失败: %v", err)
	}
	if err := st.CreateInteraction(store.Interaction{
		ID: "int-http-v020", SessionID: conversationID, RunID: "run-http-v020",
		Agent: "leader", Type: store.InteractionTypeConfirm,
		Status: store.InteractionStatusPending, Payload: `{"question":"继续？"}`,
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("准备 Interaction 失败: %v", err)
	}

	code, body = doJSON(t, router, http.MethodPost,
		"/api/v1/runs/run-http-v020/cancel", "", `{}`)
	if code != http.StatusAccepted {
		t.Fatalf("精确取消 Run 应返回 202，实际: %d %+v", code, body)
	}
	cancelled := body["run"].(map[string]any)
	if cancelled["status"] != store.RunStatusCancelling {
		t.Fatalf("Run 应进入 cancelling，实际: %+v", cancelled)
	}

	code, body = doJSON(t, router, http.MethodGet,
		"/api/v1/projects/"+projectID+"/studio/snapshot", "", "")
	if code != http.StatusOK {
		t.Fatalf("读取 Snapshot 应返回 200，实际: %d %+v", code, body)
	}
	snapshot := body["snapshot"].(map[string]any)
	if snapshot["snapshot_version"] != float64(1) {
		t.Fatalf("Snapshot 必须携带版本 1: %+v", snapshot)
	}
	if len(snapshot["conversations"].([]any)) != 1 ||
		len(snapshot["runs"].([]any)) != 1 ||
		len(snapshot["pending_interactions"].([]any)) != 1 {
		t.Fatalf("Snapshot 内容不完整: %+v", snapshot)
	}

	code, body = doJSON(t, router, http.MethodPatch,
		"/api/v1/projects/"+projectID, "",
		`{"name":"拆码垛 Studio","revision":`+
			strconv.FormatInt(revision, 10)+`}`)
	if code != http.StatusOK {
		t.Fatalf("更新 Project 应返回 200，实际: %d %+v", code, body)
	}
	projectBody = body["project"].(map[string]any)
	newRevision := int64(projectBody["revision"].(float64))
	readProjectEvent(t, events, "project.updated")

	code, _ = doJSON(t, router, http.MethodPatch,
		"/api/v1/projects/"+projectID, "",
		`{"name":"过期修改","revision":`+
			strconv.FormatInt(revision, 10)+`}`)
	if code != http.StatusConflict {
		t.Fatalf("旧 revision 应返回 409，实际: %d", code)
	}

	code, body = doJSON(t, router, http.MethodDelete,
		"/api/v1/projects/"+projectID+"?revision="+
			strconv.FormatInt(newRevision, 10), "", "")
	if code != http.StatusConflict ||
		body["error"].(map[string]any)["code"] != "PROJECT_HAS_ACTIVE_WORK" {
		t.Fatalf("未结束 Run/Interaction 应阻止归档，实际: %d %+v", code, body)
	}
	stillActive, err := st.GetProject(projectID)
	if err != nil || !stillActive.IsActive || stillActive.ArchivedAt != nil {
		t.Fatalf("归档被拒后 Project 必须保持可操作: %+v err=%v", stillActive, err)
	}
	// 归档冲突不能破坏恢复入口：原 Run 仍可完成取消，pending Interaction
	// 仍可应答。两类工作都收敛后，使用原 revision 重试归档应成功。
	if _, err := st.FinishRunSession("run-http-v020",
		[]string{store.RunStatusCancelling}, store.RunStatusCancelled, "用户取消",
		time.Now().UTC()); err != nil {
		t.Fatalf("归档冲突后 Run 仍应可取消: %v", err)
	}
	if err := st.AnswerInteraction("int-http-v020", `{"approved":false}`,
		time.Now().UTC()); err != nil {
		t.Fatalf("归档冲突后 Interaction 仍应可应答: %v", err)
	}
	code, body = doJSON(t, router, http.MethodDelete,
		"/api/v1/projects/"+projectID+"/conversations/"+conversationID, "", "")
	if code != http.StatusOK ||
		body["conversation"].(map[string]any)["archived_at"] == nil {
		t.Fatalf("归档 Conversation 应返回归档资源，实际: %d %+v", code, body)
	}
	readProjectEvent(t, events, "conversation.archived")
	code, body = doJSON(t, router, http.MethodGet,
		"/api/v1/projects/"+projectID+"/conversations", "", "")
	if code != http.StatusOK || len(body["conversations"].([]any)) != 0 {
		t.Fatalf("活动列表不应返回已归档 Conversation: %d %+v", code, body)
	}
	code, body = doJSON(t, router, http.MethodGet,
		"/api/v1/projects/"+projectID+"/conversations?include_archived=true", "", "")
	if code != http.StatusOK || len(body["conversations"].([]any)) != 1 {
		t.Fatalf("归档列表应返回历史 Conversation: %d %+v", code, body)
	}
	code, body = doJSON(t, router, http.MethodDelete,
		"/api/v1/projects/"+projectID+"?revision="+
			strconv.FormatInt(newRevision, 10), "", "")
	if code != http.StatusOK {
		t.Fatalf("归档 Project 应返回 200，实际: %d %+v", code, body)
	}
	projectBody = body["project"].(map[string]any)
	if projectBody["archived_at"] == nil {
		t.Fatalf("归档响应应包含 archived_at: %+v", projectBody)
	}
	readProjectEvent(t, events, "project.archived")
	readProjectEvent(t, events, "project.activated")
}

func TestProjectBindingsHTTPFlow(t *testing.T) {
	_, _, router, events := newProjectsTestRouter(t)
	code, body := doJSON(t, router, http.MethodPost, "/api/v1/projects", "",
		`{"name":"拆码垛技能绑定"}`)
	if code != http.StatusCreated {
		t.Fatalf("创建 Project 应返回 201，实际: %d %+v", code, body)
	}
	projectID := body["project"].(map[string]any)["id"].(string)
	readProjectEvent(t, events, "project.created")

	code, body = doJSON(t, router, http.MethodPut,
		"/api/v1/projects/"+projectID+"/bindings", "", `{
			"skill_names":["depalletizing-robot-task","depalletizing-workflow-planning",
				"depalletizing-robot-task"],
			"agent_ids":[]
		}`)
	if code != http.StatusOK {
		t.Fatalf("保存 Project Skill 绑定应返回 200，实际: %d %+v", code, body)
	}
	bindings := body["bindings"].(map[string]any)
	if got := bindings["skill_names"].([]any); len(got) != 2 ||
		got[0] != "depalletizing-robot-task" || got[1] != "depalletizing-workflow-planning" {
		t.Fatalf("Skill 绑定应去重并稳定排序: %+v", bindings)
	}
	readProjectEvent(t, events, "project.bindings.updated")

	code, body = doJSON(t, router, http.MethodGet,
		"/api/v1/projects/"+projectID+"/bindings", "", "")
	if code != http.StatusOK || len(body["bindings"].(map[string]any)["skill_names"].([]any)) != 2 {
		t.Fatalf("读取 Project Skill 绑定失败: %d %+v", code, body)
	}
}
