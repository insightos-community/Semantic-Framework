package kernel

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/llm"
	"insightos.cn/semantic-framework/pkg/log"
)

func TestTraceStreamCallbackDoesNotBufferConsumer(t *testing.T) {
	st := openKernelTestStore(t)
	h := newTestTraceHandler(st)
	ctx := h.OnStart(context.Background(), modelRunInfo(), &model.CallbackInput{})
	reader, writer := schema.Pipe[callbacks.CallbackOutput](1)
	returned := make(chan struct{})
	go func() {
		h.OnEndWithStreamOutput(ctx, modelRunInfo(), reader)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		writer.Close()
		<-returned
		h.WaitStreams()
		t.Fatal("trace callback waited for EOF before returning consumer stream")
	}
	writer.Send(&model.CallbackOutput{Message: schema.AssistantMessage("first", nil)}, nil)
	writer.Close()
	h.WaitStreams()
	spans, err := st.QuerySpans(h.TraceID())
	if err != nil || len(spans) != 1 {
		t.Fatalf("stream span was not persisted: %v, %v", spans, err)
	}
}

// openKernelTestStore 在临时目录打开一个已迁移的 Store，测试结束自动关闭。
func openKernelTestStore(t *testing.T) *store.Store {
	t.Helper()
	return storetest.OpenMigrated(t, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
}

// newTestTraceHandler 创建带固定归因参数的测试处理器。
func newTestTraceHandler(st *store.Store) *TraceHandler {
	return NewTraceHandler(st,
		log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
		TraceOptions{
			Agent:   "test-agent",
			Purpose: "chat",
			Model:   "mock",
			Price:   llm.Price{Prompt: 0.001, Completion: 0.002},
		})
}

// modelRunInfo 返回模型组件的回调运行信息。
func modelRunInfo() *callbacks.RunInfo {
	return &callbacks.RunInfo{Type: "Mock", Component: components.ComponentOfChatModel}
}

// TestTraceHandlerModelCall 验证模型调用回调：跨度与计量同时落库。
func TestTraceHandlerModelCall(t *testing.T) {
	st := openKernelTestStore(t)
	h := newTestTraceHandler(st)

	ctx := h.OnStart(context.Background(), modelRunInfo(), &model.CallbackInput{
		Messages: []*schema.Message{schema.UserMessage("hi")},
	})
	msg := schema.AssistantMessage("你好", nil)
	msg.ResponseMeta = &schema.ResponseMeta{FinishReason: "stop"}
	h.OnEnd(ctx, modelRunInfo(), &model.CallbackOutput{
		Message:    msg,
		TokenUsage: &model.TokenUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
	})

	// 跨度：名称/类型/属性。
	spans, err := st.QuerySpans(h.TraceID())
	if err != nil {
		t.Fatalf("QuerySpans 失败: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("应有 1 条跨度，实际: %d", len(spans))
	}
	span := spans[0]
	if span.Name != "Mock" || span.Kind != "ChatModel" {
		t.Errorf("跨度名称/类型不符: %+v", span)
	}
	if span.StartedAt.IsZero() || span.DurationMs < 0 {
		t.Errorf("跨度起止/耗时不符: %+v", span)
	}
	var attrs map[string]any
	if err := json.Unmarshal([]byte(span.Attrs), &attrs); err != nil {
		t.Fatalf("跨度 attrs 应为合法 JSON: %v", err)
	}
	if attrs["finish_reason"] != "stop" {
		t.Errorf("attrs 应含 finish_reason=stop，实际: %v", attrs)
	}

	// 计量：usage 与成本估算（100/1000*0.001 + 50/1000*0.002 = 0.0002）。
	records, err := st.QueryMetering(store.MeteringFilter{Agent: "test-agent"})
	if err != nil {
		t.Fatalf("QueryMetering 失败: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("应有 1 条计量记录，实际: %d", len(records))
	}
	rec := records[0]
	if rec.Model != "mock" || rec.Purpose != "chat" || rec.TraceID != h.TraceID() {
		t.Errorf("计量归因字段不符: %+v", rec)
	}
	if rec.PromptTokens != 100 || rec.CompletionTokens != 50 || rec.TotalTokens != 150 {
		t.Errorf("计量 token 数不符: %+v", rec)
	}
	if rec.CostEstimate < 0.0002-1e-12 || rec.CostEstimate > 0.0002+1e-12 {
		t.Errorf("成本估算应为 0.0002，实际: %v", rec.CostEstimate)
	}

	ioRec, err := st.GetSpanIO(h.TraceID(), span.ID)
	if err != nil {
		t.Fatalf("ChatModel 跨度应写入输入输出: %v", err)
	}
	if !strings.Contains(ioRec.Input, "hi") || strings.Contains(span.Attrs, "hi") {
		t.Fatalf("输入应落独立表且不进 attrs: io=%q attrs=%s", ioRec.Input, span.Attrs)
	}
	if !strings.Contains(ioRec.Output, "你好") {
		t.Fatalf("输出应包含模型正文，实际: %q", ioRec.Output)
	}
}

// TestTraceHandlerError 验证错误回调写入带错误属性的跨度。
func TestTraceHandlerError(t *testing.T) {
	st := openKernelTestStore(t)
	h := newTestTraceHandler(st)

	ctx := h.OnStart(context.Background(), modelRunInfo(), &model.CallbackInput{})
	h.OnError(ctx, modelRunInfo(), context.DeadlineExceeded)

	spans, err := st.QuerySpans(h.TraceID())
	if err != nil {
		t.Fatalf("QuerySpans 失败: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("应有 1 条跨度，实际: %d", len(spans))
	}
	var attrs map[string]any
	if err := json.Unmarshal([]byte(spans[0].Attrs), &attrs); err != nil {
		t.Fatalf("跨度 attrs 应为合法 JSON: %v", err)
	}
	if attrs["error"] != context.DeadlineExceeded.Error() {
		t.Errorf("attrs 应含 error，实际: %v", attrs)
	}

	// 错误路径不写计量。
	records, err := st.QueryMetering(store.MeteringFilter{})
	if err != nil {
		t.Fatalf("QueryMetering 失败: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("错误路径不应写入计量，实际: %d 条", len(records))
	}
}

// TestTraceHandlerStreamOutput 验证流式输出回调：usage 只在末帧出现也能正确计量。
func TestTraceHandlerStreamOutput(t *testing.T) {
	st := openKernelTestStore(t)
	h := newTestTraceHandler(st)

	ctx := h.OnStart(context.Background(), modelRunInfo(), &model.CallbackInput{})
	frames := []callbacks.CallbackOutput{
		&model.CallbackOutput{Message: schema.AssistantMessage("你好", nil)},
		&model.CallbackOutput{Message: schema.AssistantMessage("世界", nil)},
		// 末帧只携带 usage（OpenAI 兼容端点的流式约定）。
		&model.CallbackOutput{
			TokenUsage: &model.TokenUsage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
		},
	}
	h.OnEndWithStreamOutput(ctx, modelRunInfo(), schema.StreamReaderFromArray(frames))
	h.WaitStreams()

	spans, err := st.QuerySpans(h.TraceID())
	if err != nil {
		t.Fatalf("QuerySpans 失败: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("应有 1 条跨度，实际: %d", len(spans))
	}
	var attrs map[string]any
	if err := json.Unmarshal([]byte(spans[0].Attrs), &attrs); err != nil {
		t.Fatalf("跨度 attrs 应为合法 JSON: %v", err)
	}
	if attrs["stream_chunks"] != float64(3) {
		t.Errorf("attrs 应含 stream_chunks=3，实际: %v", attrs)
	}

	records, err := st.QueryMetering(store.MeteringFilter{})
	if err != nil {
		t.Fatalf("QueryMetering 失败: %v", err)
	}
	if len(records) != 1 || records[0].TotalTokens != 30 {
		t.Errorf("流式计量应落库 30 tokens，实际: %+v", records)
	}

	ioRec, err := st.GetSpanIO(h.TraceID(), spans[0].ID)
	if err != nil {
		t.Fatalf("流式 ChatModel 应写入输入输出: %v", err)
	}
	if !strings.Contains(ioRec.Output, "你好") || !strings.Contains(ioRec.Output, "世界") {
		t.Fatalf("流式输出应累加各帧正文，实际: %q", ioRec.Output)
	}
	if strings.Contains(spans[0].Attrs, "你好") {
		t.Fatalf("流式正文不应写入 attrs: %s", spans[0].Attrs)
	}
}

// TestTraceHandlerNeeded 验证时机声明：只跳过流式输入。
func TestTraceHandlerNeeded(t *testing.T) {
	h := newTestTraceHandler(openKernelTestStore(t))
	if h.Needed(context.Background(), modelRunInfo(), callbacks.TimingOnStartWithStreamInput) {
		t.Error("流式输入时机应声明为不需要")
	}
	for _, timing := range []callbacks.CallbackTiming{
		callbacks.TimingOnStart, callbacks.TimingOnEnd,
		callbacks.TimingOnError, callbacks.TimingOnEndWithStreamOutput,
	} {
		if !h.Needed(context.Background(), modelRunInfo(), timing) {
			t.Errorf("时机 %d 应声明为需要", timing)
		}
	}
}
