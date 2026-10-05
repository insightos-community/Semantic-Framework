package runtime

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/interaction"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/llm"
	"insightos.cn/semantic-framework/pkg/log"
)

// runtimeEventWait 是本包用例里“等待某件事最终发生”的期限：进入模型、
// 审批唤醒、Run 收尾都保证会发生，这里的期限只用于避免测试挂死，不用来
// 断言响应速度。CI Runner 比本机慢一个数量级，共享 Runner 的负载波动又很大，
// 5 秒在满载 Runner 上曾把正常完成判成失败。
const runtimeEventWait = 20 * time.Second

// approvalFixture 是审批链路单测的装配：带工具范围与审批名单的 leader
// profile、高危测试工具、真实交互服务。
type approvalFixture struct {
	st          *store.Store
	svc         *Service
	interaction *interaction.Service
	events      <-chan event.Event
	saveHits    *int32
}

// approvalSaveTool 是测试用的高危写入工具。
type approvalSaveTool struct {
	// hits 执行次数。
	hits int32
}

// Def 返回 artifact.put 的契约。
func (t *approvalSaveTool) Def() tool.Definition {
	return tool.Definition{
		Name: "artifact.put", Namespace: "artifact", Description: "保存产物",
		ParametersJSON: `{
			"type": "object",
			"properties": {"content": {"type": "string"}},
			"required": ["content"], "additionalProperties": false
		}`,
		Annotations: tool.Annotations{Risk: tool.RiskHigh},
	}
}

// Run 执行 artifact.put：计数并返回成功结果。
func (t *approvalSaveTool) Run(_ context.Context, _ string) (string, error) {
	atomic.AddInt32(&t.hits, 1)
	return tool.OKResult(map[string]any{"artifact_id": "art-test", "uri": "artifact://art-test"})
}

// newApprovalFixture 创建审批链路装配：mock 脚本固定为"先调 artifact.put、
// 后总结"，profile 声明 tools.namespaces=[artifact.*] 与
// interrupt.approval_required=[artifact.*]（两条审批触发路径同时覆盖）。
func newApprovalFixture(t *testing.T, summary string) *approvalFixture {
	t.Helper()
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})

	// 角色 profile 夹具（带工具范围与审批名单）。
	root := t.TempDir()
	dir := filepath.Join(root, "leader")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("创建角色目录失败: %v", err)
	}
	files := map[string]string{
		"role.yaml": "name: leader\nmode: coordinator\ndescription: 团队指挥官\nmodel: mock\n" +
			"tools:\n  namespaces: [artifact.*]\n" +
			"interrupt:\n  approval_required: [artifact.*]\n",
		"AGENT.md": "# Role\n你是 Leader。",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("写入 %s 失败: %v", name, err)
		}
	}

	llmReg, err := llm.Load(config.LLMConfig{
		Default:   "mock",
		Providers: map[string]config.LLMProviderConfig{"mock": {Component: "mock", Model: "mock"}},
	})
	if err != nil {
		t.Fatalf("构建 LLM 注册表失败: %v", err)
	}
	// 与 newTestFixture 一致：复用共享模板的私有副本，避免重复支付迁移成本。
	st := storetest.OpenMigrated(t, logger)

	m := kernel.NewMockChatModel()
	m.SetScript(
		kernel.MockReply{ToolCalls: []schema.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: schema.FunctionCall{
				Name:      "artifact_put",
				Arguments: `{"content":"季度报告"}`,
			},
		}}},
		kernel.MockReply{Content: summary},
	)

	saveTool := &approvalSaveTool{}
	reg := tool.NewRegistry()
	if err := reg.Register(saveTool); err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
	executor := tool.NewExecutor(reg, tool.ExecutorOptions{Logger: logger})
	bus := event.NewBus(logger)
	interactionSvc := interaction.NewService(st, bus, logger)

	svc := NewService(Deps{
		Profiles: profile.NewLoader(root), LLM: llmReg, Store: st, Bus: bus, Logger: logger,
		BuildModel: func(context.Context, llm.Provider, string) (kernel.Model, error) { return m, nil },
		Registry:   reg, Executor: executor, Interaction: interactionSvc,
	})
	return &approvalFixture{
		st: st, svc: svc, interaction: interactionSvc,
		events: bus.Subscribe(event.TopicAgentEvents), saveHits: &saveTool.hits,
	}
}

// awaitInteractionRequest 轮询总线事件直到 interaction.request，返回交互 ID。
func awaitInteractionRequest(t *testing.T, ch <-chan event.Event) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case ev := <-ch:
			env, ok := ev.Payload.(ws.Envelope)
			if !ok || env.Channel != ws.ChannelInteraction || env.Type != interaction.EventTypeInteractionRequest {
				continue
			}
			payload, ok := env.Payload.(interaction.RequestPayload)
			if !ok {
				t.Fatalf("interaction.request 负载类型不符: %T", env.Payload)
			}
			if payload.Question == "" || payload.Risk != tool.RiskHigh || payload.TimeoutTS == 0 {
				t.Errorf("交互请求负载不符: %+v", payload)
			}
			return payload.InteractionID
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("5s 内未收到 interaction.request 事件")
	return ""
}

// TestApprovalFlowApproved 验证审批批准链路：
// 高危工具中断（run → waiting_input，断点已存，交互记录 pending）→
// 用户批准（run → running）→ 工具执行 → 模型总结 → run 完成。
func TestApprovalFlowApproved(t *testing.T) {
	fx := newApprovalFixture(t, "报告已存好。")
	stateEvents := fx.svc.bus.Subscribe(event.TopicAgentEvents)

	type runResult struct {
		runID string
		err   error
	}
	resultCh := make(chan runResult, 1)
	go func() {
		runID, err := fx.svc.HandleMessage(context.Background(), "usr-1", "", "帮我把报告存起来")
		resultCh <- runResult{runID, err}
	}()

	// ① 中断：收到交互请求；run 状态 waiting_input；工具未执行。
	interactionID := awaitInteractionRequest(t, fx.events)
	it, err := fx.st.GetInteraction(interactionID)
	if err != nil {
		t.Fatalf("GetInteraction 失败: %v", err)
	}
	if it.Status != store.InteractionStatusPending || it.Agent != "leader" || it.RunID == "" || it.CheckpointID != it.RunID {
		t.Errorf("交互记录不符: %+v", it)
	}
	run, err := fx.st.GetRunSession(it.RunID)
	if err != nil {
		t.Fatalf("GetRunSession 失败: %v", err)
	}
	if run.Status != store.RunStatusWaitingInput {
		t.Errorf("run 应为 waiting_input，实际: %+v", run)
	}
	if _, ok, _ := fx.st.GetRunCheckpoint(it.RunID); !ok {
		t.Error("断点应已持久化")
	}
	if got := atomic.LoadInt32(fx.saveHits); got != 0 {
		t.Errorf("审批前工具不应执行，实际: %d", got)
	}

	// ② 批准：HandleMessage 完成；工具执行；run completed；消息落库。
	if err := fx.interaction.Reply(context.Background(), interactionID, true); err != nil {
		t.Fatalf("Reply 失败: %v", err)
	}
	var result runResult
	select {
	case result = <-resultCh:
	case <-time.After(runtimeEventWait):
		t.Fatalf("批准后 HandleMessage 未在 %s 内完成", runtimeEventWait)
	}
	if result.err != nil {
		t.Fatalf("HandleMessage 失败: %v", result.err)
	}
	if result.runID != it.RunID {
		t.Errorf("返回的 runID 应与交互记录一致: %q vs %q", result.runID, it.RunID)
	}
	if got := atomic.LoadInt32(fx.saveHits); got != 1 {
		t.Errorf("批准后工具应执行 1 次，实际: %d", got)
	}
	run, _ = fx.st.GetRunSession(result.runID)
	if run.Status != store.RunStatusCompleted || run.EndedAt == nil {
		t.Errorf("run 应迁移到 completed，实际: %+v", run)
	}
	var runStateEvents []ws.Envelope
	for _, item := range drainEvents(stateEvents) {
		envelope, ok := item.Payload.(ws.Envelope)
		if ok && envelope.ResourceType == "agent_run" {
			runStateEvents = append(runStateEvents, envelope)
		}
	}
	wantStates := []string{EventTypeRunStarted, EventTypeRunWaitingInput,
		EventTypeRunRunning, EventTypeRunCompleted}
	if len(runStateEvents) != len(wantStates) {
		t.Fatalf("审批 Run 应发布四个状态事件: %+v", runStateEvents)
	}
	for index, envelope := range runStateEvents {
		if envelope.Type != wantStates[index] || envelope.Revision != int64(index+1) ||
			envelope.Parent.RunID != result.runID || envelope.Parent.TraceID != run.TraceID {
			t.Errorf("审批状态事件 %d 不符: %+v", index, envelope)
		}
	}

	sessions, _ := fx.st.ListChatSessionsByUser("usr-1")
	msgs, _ := fx.st.ListChatMessages(sessions[0].ID, 0, 0)
	if len(msgs) != 2 || msgs[1].Message == nil ||
		msgs[1].Message.Role != schema.Assistant || msgs[1].Message.Content != "报告已存好。" {
		t.Errorf("消息落库不符: %+v", msgs)
	}
}

// TestApprovalFlowRejected 验证审批拒绝链路：工具不执行，
// 模型读到结构化拒绝结果后如实告知，run 正常完成。
func TestApprovalFlowRejected(t *testing.T) {
	fx := newApprovalFixture(t, "抱歉，未获批准，报告未存储。")

	resultCh := make(chan error, 1)
	go func() {
		_, err := fx.svc.HandleMessage(context.Background(), "usr-1", "", "帮我把报告存起来")
		resultCh <- err
	}()

	interactionID := awaitInteractionRequest(t, fx.events)
	if err := fx.interaction.Reply(context.Background(), interactionID, false); err != nil {
		t.Fatalf("Reply 失败: %v", err)
	}

	select {
	case err := <-resultCh:
		if err != nil {
			t.Fatalf("拒绝路径 HandleMessage 不应失败: %v", err)
		}
	case <-time.After(runtimeEventWait):
		t.Fatalf("拒绝后 HandleMessage 未在 %s 内完成", runtimeEventWait)
	}
	if got := atomic.LoadInt32(fx.saveHits); got != 0 {
		t.Errorf("拒绝后工具不应执行，实际: %d", got)
	}

	sessions, _ := fx.st.ListChatSessionsByUser("usr-1")
	msgs, _ := fx.st.ListChatMessages(sessions[0].ID, 0, 0)
	if len(msgs) != 2 || msgs[1].Message == nil ||
		msgs[1].Message.Content != "抱歉，未获批准，报告未存储。" {
		t.Errorf("拒绝后应落库模型的如实告知，实际: %+v", msgs)
	}
}

// TestEvictSessionCancelsApproval 验证会话删除路径：EvictSession 批量取消
// 待应答交互（Pending → Cancelled），等待审批的 run 被唤醒按拒绝收尾，
// 工具不执行，run 正常完成。
func TestEvictSessionCancelsApproval(t *testing.T) {
	fx := newApprovalFixture(t, "审批已取消，未执行。")

	resultCh := make(chan error, 1)
	go func() {
		_, err := fx.svc.HandleMessage(context.Background(), "usr-1", "", "帮我把报告存起来")
		resultCh <- err
	}()

	interactionID := awaitInteractionRequest(t, fx.events)
	it, _ := fx.st.GetInteraction(interactionID)

	// 删除会话：取消待应答交互并唤醒等待方。
	if err := fx.svc.EvictSession(context.Background(), it.SessionID); err != nil {
		t.Fatalf("EvictSession 失败: %v", err)
	}

	select {
	case err := <-resultCh:
		if err != nil {
			t.Fatalf("取消路径 HandleMessage 不应失败: %v", err)
		}
	case <-time.After(runtimeEventWait):
		t.Fatalf("会话取消后 HandleMessage 未在 %s 内完成", runtimeEventWait)
	}

	it, _ = fx.st.GetInteraction(interactionID)
	if it.Status != store.InteractionStatusCancelled {
		t.Errorf("交互应迁移 Cancelled，实际: %+v", it)
	}
	if got := atomic.LoadInt32(fx.saveHits); got != 0 {
		t.Errorf("取消后工具不应执行，实际: %d", got)
	}
}
