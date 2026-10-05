package interaction

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/log"
)

// testFixture 是交互服务单测的公共装配：临时库 + 事件总线 + 服务。
type testFixture struct {
	st     *store.Store
	svc    *Service
	events <-chan event.Event
}

// newTestFixture 创建公共装配并订阅 agent.events topic。
func newTestFixture(t *testing.T) *testFixture {
	t.Helper()
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
	st := storetest.OpenMigrated(t, logger)
	now := time.Now().UTC()
	if err := st.CreateChatSession(store.ChatSession{
		ID: "cs-1", Title: "交互测试", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("准备 Conversation 失败: %v", err)
	}
	if err := st.CreateRunSession(store.RunSession{
		ID: "run-1", ChatSessionID: "cs-1", AgentName: "leader",
		TraceID: "trace-1", Status: store.RunStatusRunning, StartedAt: now,
	}); err != nil {
		t.Fatalf("准备 Run 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	bus := event.NewBus(logger)
	return &testFixture{
		st:     st,
		svc:    NewService(st, bus, logger),
		events: bus.Subscribe(event.TopicAgentEvents),
	}
}

// readRequestEvent 从总线读一条 interaction.request 事件并解析负载。
func readRequestEvent(t *testing.T, ch <-chan event.Event) (ws.Envelope, RequestPayload) {
	t.Helper()
	env := readInteractionEvent(t, ch, EventTypeInteractionRequest)
	payload, ok := env.Payload.(RequestPayload)
	if !ok {
		t.Fatalf("事件负载应为 RequestPayload，实际: %T", env.Payload)
	}
	return env, payload
}

func readInteractionEvent(t *testing.T, ch <-chan event.Event, eventType string) ws.Envelope {
	t.Helper()
	select {
	case ev := <-ch:
		env, ok := ev.Payload.(ws.Envelope)
		if !ok {
			t.Fatalf("事件负载应为 ws.Envelope，实际: %T", ev.Payload)
		}
		if env.Channel != ws.ChannelInteraction || env.Type != eventType {
			t.Fatalf("应为 %s 事件，实际: %+v", eventType, env)
		}
		return env
	case <-time.After(10 * time.Second):
		t.Fatalf("10s 内未收到 %s 事件", eventType)
	}
	return ws.Envelope{}
}

// sampleRequest 构造测试请求参数。
func sampleRequest(sessionID string) Request {
	return Request{
		SessionID: sessionID, Agent: "leader",
		Question: "是否批准执行 artifact.put？", Risk: "high",
		RunID: "run-1", CheckpointID: "run-1",
	}
}

// TestRequestAnswered 验证请求-应答闭环：Pending 落库 → 事件下行 →
// 应答先落库再唤醒 → Request 返回批准结果。
func TestRequestAnswered(t *testing.T) {
	fx := newTestFixture(t)

	resultCh := make(chan bool, 1)
	errCh := make(chan error, 1)
	go func() {
		approved, err := fx.svc.Request(context.Background(), sampleRequest("cs-1"))
		resultCh <- approved
		errCh <- err
	}()

	// ① 下行事件：confirm schema 字段齐备。
	env, payload := readRequestEvent(t, fx.events)
	if payload.InteractionID == "" || payload.Type != "confirm" ||
		payload.Question != "是否批准执行 artifact.put？" || payload.Risk != "high" || payload.TimeoutTS == 0 {
		t.Errorf("下行负载不符: %+v", payload)
	}
	if env.SessionID != "cs-1" || env.Importance != ws.ImportanceCritical ||
		env.Agent.ID != "leader" || env.ProjectID == "" ||
		env.ResourceType != "interaction" || env.ResourceID != payload.InteractionID ||
		env.Revision != 1 || env.Parent.RunID != "run-1" ||
		env.Parent.TraceID != "trace-1" {
		t.Errorf("envelope 归因不符: %+v", env)
	}

	// ② Pending 记录已落库。
	it, err := fx.st.GetInteraction(payload.InteractionID)
	if err != nil {
		t.Fatalf("GetInteraction 失败: %v", err)
	}
	if it.Status != store.InteractionStatusPending || it.RunID != "run-1" || it.CheckpointID != "run-1" {
		t.Errorf("交互记录不符: %+v", it)
	}

	// ③ 应答：先落库再唤醒（Reply 返回时应答已是持久事实）。
	if err := fx.svc.Reply(context.Background(), payload.InteractionID, true); err != nil {
		t.Fatalf("Reply 失败: %v", err)
	}
	it, _ = fx.st.GetInteraction(payload.InteractionID)
	if it.Status != store.InteractionStatusAnswered || it.Reply != `{"approved":true}` || it.AnsweredAt == nil {
		t.Errorf("应答落库不符: %+v", it)
	}
	resolved := readInteractionEvent(t, fx.events, EventTypeInteractionResolved)
	if resolved.ProjectID != it.ProjectID || resolved.ResourceID != it.ID ||
		resolved.Revision != it.Revision || resolved.Parent.RunID != it.RunID ||
		resolved.Parent.TraceID != "trace-1" {
		t.Errorf("Interaction 应答事件定位不完整: %+v", resolved)
	}

	select {
	case approved := <-resultCh:
		if !approved {
			t.Error("Request 应返回批准")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Request 未在 10s 内返回")
	}
	if err := <-errCh; err != nil {
		t.Errorf("Request 不应返回错误，实际: %v", err)
	}
}

// TestRequestTimeout 验证超时语义：on_timeout=reject——状态迁移 Expired，
// Request 返回拒绝；超时后应答返回 ErrNotPending。
func TestRequestTimeout(t *testing.T) {
	fx := newTestFixture(t)

	req := sampleRequest("cs-1")
	req.Timeout = 100 * time.Millisecond
	resultCh := make(chan bool, 1)
	go func() {
		approved, _ := fx.svc.Request(context.Background(), req)
		resultCh <- approved
	}()

	_, payload := readRequestEvent(t, fx.events)
	select {
	case approved := <-resultCh:
		if approved {
			t.Error("超时应按拒绝处理")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Request 未在超时后返回")
	}

	it, _ := fx.st.GetInteraction(payload.InteractionID)
	if it.Status != store.InteractionStatusExpired || it.ExpiredAt == nil {
		t.Errorf("状态应迁移 Expired，实际: %+v", it)
	}
	if err := fx.svc.Reply(context.Background(), payload.InteractionID, true); !errors.Is(err, ErrNotPending) {
		t.Errorf("过期后应答应返回 ErrNotPending，实际: %v", err)
	}
}

// TestReplyValidation 验证应答校验：不存在返回 ErrNotFound，
// 重复应答返回 ErrNotPending（防重复应答）。
func TestReplyValidation(t *testing.T) {
	fx := newTestFixture(t)

	if err := fx.svc.Reply(context.Background(), "int-missing", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("缺失交互应返回 ErrNotFound，实际: %v", err)
	}

	resultCh := make(chan bool, 1)
	go func() {
		approved, _ := fx.svc.Request(context.Background(), sampleRequest("cs-1"))
		resultCh <- approved
	}()
	_, payload := readRequestEvent(t, fx.events)

	if err := fx.svc.Reply(context.Background(), payload.InteractionID, false); err != nil {
		t.Fatalf("首次应答失败: %v", err)
	}
	if err := fx.svc.Reply(context.Background(), payload.InteractionID, true); !errors.Is(err, ErrNotPending) {
		t.Errorf("重复应答应返回 ErrNotPending，实际: %v", err)
	}
	if approved := <-resultCh; approved {
		t.Error("首次应答为拒绝，Request 应返回拒绝")
	}
}

// TestAuthorizeReplyOwnership 验证旧 Chat WebSocket 使用的归属校验：
// Interaction 必须属于连接订阅的 Conversation，Conversation 也必须属于
// token 对应用户；任一不匹配都返回统一的 ErrNotFound。
func TestAuthorizeReplyOwnership(t *testing.T) {
	fx := newTestFixture(t)
	now := time.Now().UTC()
	if err := fx.st.CreateChatSession(store.ChatSession{
		ID: "cs-2", Title: "其他会话", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("准备其他 Conversation 失败: %v", err)
	}
	interactionID := store.NewInteractionID()
	if err := fx.st.CreateInteraction(store.Interaction{
		ID: interactionID, SessionID: "cs-1", Agent: "leader",
		Type: store.InteractionTypeConfirm, Status: store.InteractionStatusPending,
		Payload: `{}`, CreatedAt: now,
	}); err != nil {
		t.Fatalf("准备 Interaction 失败: %v", err)
	}

	if err := fx.svc.AuthorizeReply(context.Background(), "", "cs-1", interactionID); err != nil {
		t.Fatalf("正确归属应通过校验: %v", err)
	}
	for name, scope := range map[string][2]string{
		"跨会话": {"", "cs-2"},
		"跨用户": {"usr-other", "cs-1"},
		"空会话": {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if err := fx.svc.AuthorizeReply(context.Background(), scope[0], scope[1], interactionID); !errors.Is(err, ErrNotFound) {
				t.Errorf("归属不匹配应返回 ErrNotFound，实际: %v", err)
			}
		})
	}
	if err := fx.svc.AuthorizeReply(context.Background(), "", "cs-1", "int-missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("缺失 Interaction 应返回 ErrNotFound，实际: %v", err)
	}
}

// TestCancelSession 验证会话删除路径：待应答交互批量迁移 Cancelled，
// 等待方被唤醒返回拒绝，取消后应答返回 ErrNotPending。
func TestCancelSession(t *testing.T) {
	fx := newTestFixture(t)

	resultCh := make(chan bool, 1)
	go func() {
		approved, _ := fx.svc.Request(context.Background(), sampleRequest("cs-1"))
		resultCh <- approved
	}()
	_, payload := readRequestEvent(t, fx.events)

	if err := fx.svc.CancelSession(context.Background(), "cs-1"); err != nil {
		t.Fatalf("CancelSession 失败: %v", err)
	}
	it, _ := fx.st.GetInteraction(payload.InteractionID)
	if it.Status != store.InteractionStatusCancelled {
		t.Errorf("状态应迁移 Cancelled，实际: %+v", it)
	}

	select {
	case approved := <-resultCh:
		if approved {
			t.Error("取消后 Request 应返回拒绝")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("等待方未被取消唤醒")
	}
	if err := fx.svc.Reply(context.Background(), payload.InteractionID, true); !errors.Is(err, ErrNotPending) {
		t.Errorf("取消后应答应返回 ErrNotPending，实际: %v", err)
	}
}

// TestRequestContextCancel 验证调用方 ctx 取消：状态迁移 Cancelled 并返回 ctx 错误。
func TestRequestContextCancel(t *testing.T) {
	fx := newTestFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := fx.svc.Request(ctx, sampleRequest("cs-1"))
		errCh <- err
	}()
	_, payload := readRequestEvent(t, fx.events)

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("应返回 context.Canceled，实际: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Request 未随 ctx 取消返回")
	}
	it, _ := fx.st.GetInteraction(payload.InteractionID)
	if it.Status != store.InteractionStatusCancelled {
		t.Errorf("状态应迁移 Cancelled，实际: %+v", it)
	}
}

type structuredTestRouter struct {
	count int
	fail  bool
}

func TestCancelStructuredRoutesCancelledOutcome(t *testing.T) {
	fx := newTestFixture(t)
	project, err := fx.st.CreateProject("cancel-user", "Cancel Interaction")
	if err != nil {
		t.Fatal(err)
	}
	session := store.ChatSession{ID: "cs-cancel", UserID: project.OwnerID,
		ProjectID: project.ID, Title: "cancel", CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC()}
	if err := fx.st.CreateChatSession(session); err != nil {
		t.Fatal(err)
	}
	if err := fx.st.CreateRunSession(store.RunSession{ID: "run-question", ProjectID: project.ID,
		ChatSessionID: session.ID, Kind: store.RunKindConversation, ContextID: session.ID,
		AgentID: "leader", AgentName: "Leader", Status: store.RunStatusRunning,
		StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	router := &structuredTestRouter{}
	fx.svc.SetAnswerRouter(router)
	created, err := fx.svc.CreateStructured(context.Background(), StructuredRequest{
		ProjectID: project.ID, SessionID: session.ID, RunID: "run-question",
		SourceAgentID: "leader", SourceRevision: 1, Type: store.InteractionTypeInput,
		UIKind: store.InteractionUIForm, Prompt: "可取消的问题",
		ResponseSchema: json.RawMessage(`{"type":"object"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = readInteractionEvent(t, fx.events, EventTypeInteractionRequest)
	if err := fx.svc.CancelStructured(context.Background(), project.OwnerID,
		project.ID, created.ID); err != nil {
		t.Fatal(err)
	}
	_ = readInteractionEvent(t, fx.events, EventTypeInteractionResolved)
	cancelled, err := fx.st.GetInteraction(created.ID)
	if err != nil || cancelled.Status != store.InteractionStatusCancelled ||
		cancelled.HandledAt == nil || router.count != 1 {
		t.Fatalf("取消应作为cancelled结果路由原Agent: value=%+v router=%d err=%v",
			cancelled, router.count, err)
	}
}

func (r *structuredTestRouter) RouteInteractionAnswer(_ context.Context, _ store.Interaction) error {
	r.count++
	if r.fail {
		return errors.New("temporary routing error")
	}
	return nil
}

func TestStructuredReplyResumesCheckpointApprovalWithoutAnswerRouter(t *testing.T) {
	fx := newTestFixture(t)
	project, err := fx.st.CreateProject("checkpoint-user", "Checkpoint Approval")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	session := store.ChatSession{ID: "cs-checkpoint", UserID: project.OwnerID,
		ProjectID: project.ID, Title: "checkpoint", CreatedAt: now, UpdatedAt: now}
	if err := fx.st.CreateChatSession(session); err != nil {
		t.Fatal(err)
	}
	run := store.RunSession{ID: "run-checkpoint", ProjectID: project.ID,
		ChatSessionID: session.ID, Kind: store.RunKindConversation,
		ContextID: "leader:" + session.ID, AgentID: "leader", AgentName: "leader",
		Status: store.RunStatusWaitingInput, StartedAt: now, UpdatedAt: now}
	if err := fx.st.CreateRunSession(run); err != nil {
		t.Fatal(err)
	}
	router := &structuredTestRouter{}
	fx.svc.SetAnswerRouter(router)
	resultCh := make(chan bool, 1)
	errCh := make(chan error, 1)
	go func() {
		approved, requestErr := fx.svc.Request(context.Background(), Request{
			SessionID: session.ID, Agent: "leader", Question: "允许执行 Robot？",
			Risk: "critical", RunID: run.ID, CheckpointID: run.ID,
		})
		resultCh <- approved
		errCh <- requestErr
	}()
	_, payload := readRequestEvent(t, fx.events)
	if err := fx.svc.ReplyStructured(context.Background(), project.OwnerID, project.ID,
		payload.InteractionID, json.RawMessage(`{"approved":true}`), 0); err != nil {
		t.Fatal(err)
	}
	_ = readInteractionEvent(t, fx.events, EventTypeInteractionResolved)
	select {
	case approved := <-resultCh:
		if !approved {
			t.Fatal("checkpoint approval 应唤醒原 Run 并返回批准")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("checkpoint approval 未唤醒原 Run")
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	value, err := fx.st.GetInteraction(payload.InteractionID)
	if err != nil || value.HandledAt == nil || router.count != 0 {
		t.Fatalf("checkpoint approval 不应进入 AnswerRouter: value=%+v router=%d err=%v",
			value, router.count, err)
	}
}

func TestStructuredInteractionsAllRenderersAndRecovery(t *testing.T) {
	fx := newTestFixture(t)
	project, err := fx.st.CreateProject("structured-user", "Structured Project")
	if err != nil {
		t.Fatal(err)
	}
	session := store.ChatSession{ID: "cs-structured", UserID: project.OwnerID, ProjectID: project.ID, Title: "structured", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := fx.st.CreateChatSession(session); err != nil {
		t.Fatal(err)
	}
	draft := store.WorkflowDraft{Goal: "structured input", Tasks: []store.TaskDraft{{ID: "task-structured", RequiredRole: "developer", Goal: "handle input", SubTasks: []store.SubTaskDraft{{Kind: "agent_step", Goal: "continue"}}}}}
	ready, err := fx.st.SubmitPlanProposal(session.ProjectID, session.ID, draft, "", nil,
		"# structured input", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	view, err := fx.st.ApprovePlanProposal(ready.ID, ready.Revision, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	semanticMap, err := fx.st.GetSemanticMap(session.ProjectID, store.MapSlotSimulation)
	if err != nil {
		t.Fatal(err)
	}
	mapView, err := fx.st.ApplyMapUpdate(session.ProjectID, store.MapSlotSimulation, store.MapUpdate{Generation: semanticMap.Generation, ExpectedRevision: semanticMap.Revision, Source: store.MapSourceUser, Entities: []store.MapEntity{{ID: "entity-choice", Type: "box", Name: "choice", Pose: store.Pose{Orientation: store.Quaternion{W: 1}}, Bounds: store.Bounds{Kind: "box", Size: &store.Position{X: 1, Y: 1, Z: 1}}}}}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	image, err := fx.st.PutUserArtifact(project.OwnerID, "image/png", "image", `{}`, []byte("image"))
	if err != nil {
		t.Fatal(err)
	}
	file, err := fx.st.PutUserArtifact(project.OwnerID, "text/plain", "file", `{}`, []byte("file"))
	if err != nil {
		t.Fatal(err)
	}
	router := &structuredTestRouter{}
	fx.svc.SetAnswerRouter(router)
	objectSchema := json.RawMessage(`{"type":"object"}`)
	tests := []struct {
		name       string
		kind       string
		typeName   string
		candidates json.RawMessage
		schema     json.RawMessage
		response   json.RawMessage
		mapID      string
		generation int64
	}{
		{name: "confirm", typeName: store.InteractionTypeConfirm, response: json.RawMessage(`{"approved":true}`)},
		{name: "form", typeName: store.InteractionTypeInput, kind: store.InteractionUIForm, schema: objectSchema, response: json.RawMessage(`{"name":"demo"}`)},
		{name: "single", typeName: store.InteractionTypeInput, kind: store.InteractionUISingleSelect, candidates: json.RawMessage(`[{"value":"a"}]`), schema: objectSchema, response: json.RawMessage(`{"value":"a"}`)},
		{name: "multi", typeName: store.InteractionTypeInput, kind: store.InteractionUIMultiSelect, candidates: json.RawMessage(`[{"value":"a"},{"value":"b"}]`), schema: objectSchema, response: json.RawMessage(`{"values":["a","b"]}`)},
		{name: "parameter", typeName: store.InteractionTypeInput, kind: store.InteractionUIParameter, schema: objectSchema, response: json.RawMessage(`{"value":3}`)},
		{name: "image", typeName: store.InteractionTypeInput, kind: store.InteractionUIImageSelect, candidates: json.RawMessage(`[{"artifact_id":"` + image.ID + `"}]`), schema: objectSchema, response: json.RawMessage(`{"artifact_id":"` + image.ID + `"}`)},
		{name: "file", typeName: store.InteractionTypeInput, kind: store.InteractionUIFileSelect, candidates: json.RawMessage(`[{"artifact_id":"` + file.ID + `"}]`), schema: objectSchema, response: json.RawMessage(`{"artifact_id":"` + file.ID + `"}`)},
		{name: "map", typeName: store.InteractionTypeInput, kind: store.InteractionUIMapSelect, candidates: json.RawMessage(`[{"entity_id":"entity-choice"}]`), schema: objectSchema, response: json.RawMessage(`{"map_id":"simulation_map","generation":1,"selections":[{"kind":"entity","entity_id":"entity-choice"}]}`), mapID: store.MapSlotSimulation, generation: mapView.Map.Generation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			created, err := fx.svc.CreateStructured(context.Background(), StructuredRequest{ProjectID: session.ProjectID, SessionID: session.ID, WorkflowID: view.Workflow.ID, SourceAgentID: "leader", SourceRevision: view.Workflow.Revision, Type: tc.typeName, UIKind: tc.kind, Prompt: tc.name, Candidates: tc.candidates, ResponseSchema: tc.schema, MapID: tc.mapID, MapGeneration: tc.generation})
			if err != nil {
				t.Fatal(err)
			}
			requestEvent := readInteractionEvent(t, fx.events, EventTypeInteractionRequest)
			payload, ok := requestEvent.Payload.(StructuredPayload)
			if !ok || payload.Prompt != tc.name || payload.SourceRevision != view.Workflow.Revision || payload.ResponseSchema == nil {
				t.Fatalf("public payload mismatch: %#v", requestEvent.Payload)
			}
			if err := fx.svc.ReplyStructured(context.Background(), project.OwnerID, session.ProjectID, created.ID, tc.response, view.Workflow.Revision); err != nil {
				t.Fatal(err)
			}
			_ = readInteractionEvent(t, fx.events, EventTypeInteractionResolved)
			answered, err := fx.st.GetInteraction(created.ID)
			if err != nil || answered.HandledAt == nil {
				t.Fatalf("answer not handled: %+v err=%v", answered, err)
			}
		})
	}
	if router.count != len(tests) {
		t.Fatalf("router count=%d want=%d", router.count, len(tests))
	}

	stale, err := fx.svc.CreateStructured(context.Background(), StructuredRequest{ProjectID: session.ProjectID, SessionID: session.ID, WorkflowID: view.Workflow.ID, SourceAgentID: "leader", SourceRevision: view.Workflow.Revision, Type: store.InteractionTypeInput, UIKind: store.InteractionUIForm, Prompt: "stale", ResponseSchema: objectSchema})
	if err != nil {
		t.Fatal(err)
	}
	_ = readInteractionEvent(t, fx.events, EventTypeInteractionRequest)
	if err := fx.svc.ReplyStructured(context.Background(), project.OwnerID, session.ProjectID, stale.ID, json.RawMessage(`{}`), 0); !errors.Is(err, store.ErrRevisionConflict) {
		t.Fatalf("zero source revision must not bypass: %v", err)
	}

	selectionSchema := json.RawMessage(`{"type":"object","required":["value"],"properties":{"value":{"type":"string","enum":["allowed"]}},"additionalProperties":false}`)
	invalidCandidate, err := fx.svc.CreateStructured(context.Background(), StructuredRequest{ProjectID: session.ProjectID, SessionID: session.ID, WorkflowID: view.Workflow.ID, SourceAgentID: "leader", SourceRevision: view.Workflow.Revision, Type: store.InteractionTypeInput, UIKind: store.InteractionUISingleSelect, Prompt: "candidate", Candidates: json.RawMessage(`[{"value":"allowed"}]`), ResponseSchema: selectionSchema})
	if err != nil {
		t.Fatal(err)
	}
	var defaultSelectionPayload StructuredPayload
	if err := json.Unmarshal([]byte(invalidCandidate.Payload), &defaultSelectionPayload); err != nil {
		t.Fatal(err)
	}
	if !defaultSelectionPayload.AllowOther {
		t.Fatal("单选默认应在协议中显式允许其他选项")
	}
	_ = readInteractionEvent(t, fx.events, EventTypeInteractionRequest)
	if err := fx.svc.ReplyStructured(context.Background(), project.OwnerID, session.ProjectID, invalidCandidate.ID, json.RawMessage(`{"value":"用户填写的其他选项"}`), view.Workflow.Revision); err != nil {
		t.Fatalf("默认应允许自由填写其他选项: %v", err)
	}
	_ = readInteractionEvent(t, fx.events, EventTypeInteractionResolved)

	dynamicSelectionSchema := json.RawMessage(`{"type":"object","required":["confirm_resubmit"],"properties":{"confirm_resubmit":{"type":"string","enum":["stop","wait"]}},"additionalProperties":false}`)
	dynamicSelection, err := fx.svc.CreateStructured(context.Background(), StructuredRequest{ProjectID: session.ProjectID, SessionID: session.ID, WorkflowID: view.Workflow.ID, SourceAgentID: "leader", SourceRevision: view.Workflow.Revision, Type: store.InteractionTypeInput, UIKind: store.InteractionUISingleSelect, Prompt: "dynamic field", Candidates: json.RawMessage(`[{"value":"stop"},{"value":"wait"}]`), ResponseSchema: dynamicSelectionSchema})
	if err != nil {
		t.Fatal(err)
	}
	_ = readInteractionEvent(t, fx.events, EventTypeInteractionRequest)
	if err := fx.svc.ReplyStructured(context.Background(), project.OwnerID, session.ProjectID, dynamicSelection.ID, json.RawMessage(`{"confirm_resubmit":"用户填写的处理方式"}`), view.Workflow.Revision); err != nil {
		t.Fatalf("动态字段名单选必须按原字段提交并允许其他选项: %v", err)
	}
	_ = readInteractionEvent(t, fx.events, EventTypeInteractionResolved)

	strict := false
	strictCandidate, err := fx.svc.CreateStructured(context.Background(), StructuredRequest{ProjectID: session.ProjectID, SessionID: session.ID, WorkflowID: view.Workflow.ID, SourceAgentID: "leader", SourceRevision: view.Workflow.Revision, Type: store.InteractionTypeInput, UIKind: store.InteractionUISingleSelect, Prompt: "strict candidate", Candidates: json.RawMessage(`[{"value":"allowed"}]`), ResponseSchema: objectSchema, AllowOther: &strict})
	if err != nil {
		t.Fatal(err)
	}
	var strictSelectionPayload StructuredPayload
	if err := json.Unmarshal([]byte(strictCandidate.Payload), &strictSelectionPayload); err != nil {
		t.Fatal(err)
	}
	if strictSelectionPayload.AllowOther {
		t.Fatal("严格单选必须在协议中显式关闭其他选项")
	}
	_ = readInteractionEvent(t, fx.events, EventTypeInteractionRequest)
	if err := fx.svc.ReplyStructured(context.Background(), project.OwnerID, session.ProjectID, strictCandidate.ID, json.RawMessage(`{"value":"forbidden"}`), view.Workflow.Revision); !errors.Is(err, store.ErrInvalidState) {
		t.Fatalf("显式关闭其他选项时非法候选必须失败: %v", err)
	}

	past := time.Now().UTC().Add(-time.Second)
	expired, err := fx.svc.CreateStructured(context.Background(), StructuredRequest{ProjectID: session.ProjectID, SessionID: session.ID, WorkflowID: view.Workflow.ID, SourceAgentID: "leader", SourceRevision: view.Workflow.Revision, Type: store.InteractionTypeInput, UIKind: store.InteractionUIForm, Prompt: "expired", ResponseSchema: objectSchema, ExpiresAt: &past})
	if err != nil {
		t.Fatal(err)
	}
	_ = readInteractionEvent(t, fx.events, EventTypeInteractionRequest)
	if err := fx.svc.ReplyStructured(context.Background(), project.OwnerID, session.ProjectID, expired.ID, json.RawMessage(`{}`), view.Workflow.Revision); !errors.Is(err, ErrNotPending) {
		t.Fatalf("expired answer must fail: %v", err)
	}

	staleMap, err := fx.svc.CreateStructured(context.Background(), StructuredRequest{ProjectID: session.ProjectID, SessionID: session.ID, WorkflowID: view.Workflow.ID, SourceAgentID: "leader", SourceRevision: view.Workflow.Revision, Type: store.InteractionTypeInput, UIKind: store.InteractionUIMapSelect, Prompt: "stale-map", Candidates: json.RawMessage(`[{"entity_id":"entity-choice"}]`), ResponseSchema: objectSchema, MapID: store.MapSlotSimulation, MapGeneration: mapView.Map.Generation})
	if err != nil {
		t.Fatal(err)
	}
	_ = readInteractionEvent(t, fx.events, EventTypeInteractionRequest)
	if _, err := fx.st.CreateMapGeneration(session.ProjectID, store.MapSlotSimulation, mapView.Map.Revision, "reset", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := fx.svc.ReplyStructured(context.Background(), project.OwnerID, session.ProjectID, staleMap.ID, json.RawMessage(`{"map_id":"simulation_map","generation":1,"selections":[{"kind":"entity","entity_id":"entity-choice"}]}`), view.Workflow.Revision); !errors.Is(err, store.ErrStaleMapGeneration) {
		t.Fatalf("stale map selection must fail: %v", err)
	}

	failing := &structuredTestRouter{fail: true}
	fx.svc.SetAnswerRouter(failing)
	recoverable, err := fx.svc.CreateStructured(context.Background(), StructuredRequest{ProjectID: session.ProjectID, SessionID: session.ID, WorkflowID: view.Workflow.ID, SourceAgentID: "leader", SourceRevision: view.Workflow.Revision, Type: store.InteractionTypeConfirm, Prompt: "recover"})
	if err != nil {
		t.Fatal(err)
	}
	_ = readInteractionEvent(t, fx.events, EventTypeInteractionRequest)
	if err := fx.svc.ReplyStructured(context.Background(), project.OwnerID, session.ProjectID, recoverable.ID, json.RawMessage(`{"approved":true}`), view.Workflow.Revision); err != nil {
		t.Fatal(err)
	}
	_ = readInteractionEvent(t, fx.events, EventTypeInteractionResolved)
	value, _ := fx.st.GetInteraction(recoverable.ID)
	if value.HandledAt != nil || value.HandlingError == "" {
		t.Fatalf("failed route must remain recoverable: %+v", value)
	}
	restarted := NewService(fx.st, event.NewBus(log.New(log.Options{Level: log.LevelError, Writer: io.Discard})), log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	recoveredRouter := &structuredTestRouter{}
	restarted.SetAnswerRouter(recoveredRouter)
	if err := restarted.RecoverAnsweredInteractions(context.Background()); err != nil {
		t.Fatal(err)
	}
	value, _ = fx.st.GetInteraction(recoverable.ID)
	if recoveredRouter.count != 1 || value.HandledAt == nil {
		t.Fatalf("restart recovery failed: router=%d value=%+v", recoveredRouter.count, value)
	}
}

func TestAgentQuestionBuildsSchemaAndRoutesAnsweredConversation(t *testing.T) {
	fx := newTestFixture(t)
	project, err := fx.st.CreateProject("question-user", "Agent Question")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	session := store.ChatSession{ID: "cs-question", UserID: project.OwnerID,
		ProjectID: project.ID, Title: "question", CreatedAt: now, UpdatedAt: now}
	if err := fx.st.CreateChatSession(session); err != nil {
		t.Fatal(err)
	}
	run := store.RunSession{ID: "run-question", ProjectID: project.ID,
		ChatSessionID: session.ID, Kind: store.RunKindConversation,
		ContextID: "leader:" + session.ID, AgentID: "leader", AgentName: "leader",
		Status: store.RunStatusRunning, StartedAt: now, UpdatedAt: now}
	if err := fx.st.CreateRunSession(run); err != nil {
		t.Fatal(err)
	}
	router := &structuredTestRouter{}
	fx.svc.SetAnswerRouter(router)
	created, err := fx.svc.CreateAgentQuestion(context.Background(), AgentQuestionRequest{
		InteractionMode: "plan", ProjectID: project.ID, SessionID: session.ID,
		RunID: run.ID, SourceAgentID: "leader", UIKind: store.InteractionUIForm,
		Prompt: "请补充搬运目标", Fields: []AgentQuestionField{
			{Name: "target", Label: "目标槽位", ValueType: "string", Required: true},
			{Name: "maximum_speed", Label: "最大速度", ValueType: "number", Required: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	requestEvent := readInteractionEvent(t, fx.events, EventTypeInteractionRequest)
	payload, ok := requestEvent.Payload.(StructuredPayload)
	if !ok || payload.InteractionID != created.ID || payload.InteractionMode != "plan" ||
		payload.UIKind != store.InteractionUIForm {
		t.Fatalf("类型化问题事件不完整: %#v", requestEvent.Payload)
	}
	if err := fx.svc.ReplyStructured(context.Background(), project.OwnerID, project.ID,
		created.ID, json.RawMessage(`{"target":"slot-a"}`), created.SourceRevision); err == nil {
		t.Fatal("缺少必填速度的回答必须被 Server schema 拒绝")
	}
	answer := json.RawMessage(`{"target":"slot-a","maximum_speed":0.4}`)
	if err := fx.svc.ReplyStructured(context.Background(), project.OwnerID, project.ID,
		created.ID, answer, created.SourceRevision); err != nil {
		t.Fatal(err)
	}
	_ = readInteractionEvent(t, fx.events, EventTypeInteractionResolved)
	if router.count != 1 {
		t.Fatalf("回答应精确路由一次，实际 %d", router.count)
	}
}
