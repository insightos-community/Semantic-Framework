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

package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/llm"
	"insightos.cn/semantic-framework/pkg/log"
)

// TraceOptions 定义一次 Agent Run 的链路归因参数。
type TraceOptions struct {
	TraceID string
	Agent   string
	Role    string
	Purpose string
	Model   string
	Price   llm.Price
}

// TraceHandler 在单次 Agent Run 开始时创建，并通过 adk.WithCallbacks 动态
// 挂接。它既观察 Agent 根调用，也观察内部模型和工具调用；缓存的 Runner
// 不持有这个处理器，因此不同 Run 不会复用 trace_id。
type TraceHandler struct {
	st      *store.Store
	logger  *log.Logger
	traceID string
	agent   string
	role    string
	purpose string
	model   string
	price   llm.Price

	// root 在中断与 Resume 之间复用。Resume 会重新进入 Agent OnStart，但
	// 同一 Run 只能有一个根 Span，后续进入只更新原根 Span 的结束信息。
	mu      sync.Mutex
	root    *spanState
	streams sync.WaitGroup
}

// spanState 由 OnStart 放入 context，后续 OnEnd/OnError 使用同一条记录。
// Span 在开始时即写入数据库，子调用因而能稳定引用其数据库 ID。
type spanState struct {
	id        int64
	startedAt time.Time
	inputText string
}

type spanCtxKey struct{}

func NewTraceHandler(st *store.Store, logger *log.Logger, opts TraceOptions) *TraceHandler {
	traceID := opts.TraceID
	if traceID == "" {
		traceID = uuid.NewString()
	}
	return &TraceHandler{
		st: st, logger: logger, traceID: traceID,
		agent: opts.Agent, role: opts.Role, purpose: opts.Purpose,
		model: opts.Model, price: opts.Price,
	}
}

func (h *TraceHandler) TraceID() string { return h.traceID }

func (h *TraceHandler) Needed(_ context.Context, info *callbacks.RunInfo, timing callbacks.CallbackTiming) bool {
	if timing == callbacks.TimingOnStartWithStreamInput || info == nil {
		return false
	}
	switch info.Component {
	case adk.ComponentOfAgent, components.ComponentOfTool:
		return true
	case components.ComponentOfChatModel:
		return true
	default:
		return false
	}
}

// OnStart 立即写入 Span。根 Agent 没有父 Span；模型、工具和 SubAgent 从
// context 取得当前 Span，并把其数据库 ID 写为 parent_id。
func (h *TraceHandler) OnStart(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
	parent, _ := ctx.Value(spanCtxKey{}).(*spanState)
	var state *spanState
	if info != nil && info.Component == adk.ComponentOfAgent && parent == nil {
		if info.Name == h.agent || h.rootState() == nil {
			state = h.ensureRoot(info)
		} else {
			// AgentTool 的子 Agent 属于父 Run：作为根 Span 的子跨度保存，
			// 它自己的模型与工具随后会继承这条 parent_id。
			parent = h.rootState()
			state = h.startSpan(info, parent)
		}
	} else {
		state = h.startSpan(info, parent)
	}
	if isChatModelSpan(info) {
		state.inputText = encodeChatModelInput(input)
	}
	return context.WithValue(ctx, spanCtxKey{}, state)
}

func (h *TraceHandler) OnEnd(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
	attrs := map[string]any{"status": "completed"}
	var usage *model.TokenUsage
	modelName := h.model
	if out := model.ConvCallbackOutput(output); out != nil {
		usage = out.TokenUsage
		if out.Config != nil && out.Config.Model != "" {
			modelName = out.Config.Model
		}
		if out.Message != nil {
			attrs["content_len"] = len(out.Message.Content)
			if len(out.Message.ToolCalls) > 0 {
				attrs["tool_calls"] = len(out.Message.ToolCalls)
			}
			if out.Message.ResponseMeta != nil {
				attrs["finish_reason"] = out.Message.ResponseMeta.FinishReason
			}
		}
	}
	h.completeSpan(ctx, info, attrs)
	h.persistChatModelIO(ctx, info, encodeChatModelOutput(output))
	if usage != nil {
		h.insertMetering(modelName, llm.Usage{
			PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
			TotalTokens: usage.TotalTokens,
		})
	}
	return ctx
}

func (h *TraceHandler) OnError(ctx context.Context, info *callbacks.RunInfo, err error) context.Context {
	h.completeSpan(ctx, info, map[string]any{"status": "failed", "error": err.Error()})
	h.persistChatModelIO(ctx, info, "")
	return ctx
}

func (h *TraceHandler) OnStartWithStreamInput(ctx context.Context, _ *callbacks.RunInfo,
	input *schema.StreamReader[callbacks.CallbackInput]) context.Context {
	input.Close()
	return ctx
}

func (h *TraceHandler) OnEndWithStreamOutput(ctx context.Context, info *callbacks.RunInfo,
	output *schema.StreamReader[callbacks.CallbackOutput]) context.Context {
	// Eino invokes this callback synchronously before returning the consumer's
	// copy. Drain only our copy asynchronously so tracing cannot buffer the UI.
	h.streams.Add(1)
	go func() {
		defer h.streams.Done()
		h.recordStream(ctx, info, output)
	}()
	return ctx
}

// WaitStreams 只在一次 Run 的终态调用。中断流不得 Wait：Resume 会继续
// 向同一个 WaitGroup Add，重叠 Wait/Add 会被 -race 判为 data race。
func (h *TraceHandler) WaitStreams() { h.streams.Wait() }

func (h *TraceHandler) recordStream(ctx context.Context, info *callbacks.RunInfo,
	output *schema.StreamReader[callbacks.CallbackOutput]) {
	defer output.Close()
	var usage *model.TokenUsage
	modelName := h.model
	chunks := 0
	var outputText strings.Builder
	for {
		frame, err := output.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			h.logger.WithError(err).Warn("读取流式回调帧失败", "trace_id", h.traceID)
			break
		}
		out := model.ConvCallbackOutput(frame)
		if out == nil {
			continue
		}
		chunks++
		if out.Config != nil && out.Config.Model != "" {
			modelName = out.Config.Model
		}
		if out.TokenUsage != nil {
			usage = out.TokenUsage
		}
		if text := encodeChatModelOutput(out); text != "" {
			if outputText.Len() > 0 {
				outputText.WriteByte('\n')
			}
			outputText.WriteString(text)
		}
	}
	h.completeSpan(ctx, info, map[string]any{"status": "completed", "stream_chunks": chunks})
	h.persistChatModelIO(ctx, info, outputText.String())
	if usage != nil {
		h.insertMetering(modelName, llm.Usage{
			PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
			TotalTokens: usage.TotalTokens,
		})
	}
}

func (h *TraceHandler) rootState() *spanState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.root
}

func (h *TraceHandler) ensureRoot(info *callbacks.RunInfo) *spanState {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.root != nil {
		return h.root
	}
	h.root = h.startSpan(info, nil)
	return h.root
}

func (h *TraceHandler) startSpan(info *callbacks.RunInfo, parent *spanState) *spanState {
	startedAt := time.Now().UTC()
	name, kind := spanNameKind(info)
	parentID := ""
	if parent != nil && parent.id > 0 {
		parentID = strconv.FormatInt(parent.id, 10)
	}
	attrs, _ := json.Marshal(map[string]any{"status": "running"})
	id, err := h.st.InsertSpan(store.Span{
		TraceID: h.traceID, ParentID: parentID, Name: name, Kind: kind,
		StartedAt: startedAt, Attrs: string(attrs),
	})
	if err != nil {
		h.logger.WithError(err).Warn("写入 trace 跨度失败", "trace_id", h.traceID, "span", name)
	}
	return &spanState{id: id, startedAt: startedAt}
}

func (h *TraceHandler) completeSpan(ctx context.Context, info *callbacks.RunInfo, attrs map[string]any) {
	state, _ := ctx.Value(spanCtxKey{}).(*spanState)
	if state == nil || state.id == 0 {
		h.logger.Warn("trace 回调缺少起始 Span", "trace_id", h.traceID)
		return
	}
	encoded, err := json.Marshal(attrs)
	if err != nil {
		h.logger.WithError(err).Warn("序列化跨度属性失败", "trace_id", h.traceID)
		encoded = []byte(`{"status":"unknown"}`)
	}
	duration := time.Since(state.startedAt).Milliseconds()
	if err := h.st.CompleteSpan(state.id, duration, string(encoded)); err != nil {
		name, _ := spanNameKind(info)
		h.logger.WithError(err).Warn("完成 trace 跨度失败", "trace_id", h.traceID, "span", name)
	}
}

func (h *TraceHandler) insertMetering(modelName string, usage llm.Usage) {
	cost := llm.EstimateCost(h.price, usage)
	if _, err := h.st.InsertMetering(store.Metering{
		TraceID: h.traceID, Agent: h.agent, Role: h.role, Model: modelName,
		Purpose: h.purpose, PromptTokens: usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens, TotalTokens: usage.TotalTokens,
		CostEstimate: cost, CreatedAt: time.Now().UTC(),
	}); err != nil {
		h.logger.WithError(err).Warn("写入计量记录失败", "trace_id", h.traceID, "model", modelName)
		return
	}
	h.logger.Debug("模型调用计量已落库", "trace_id", h.traceID, "model", modelName,
		"prompt_tokens", usage.PromptTokens, "completion_tokens", usage.CompletionTokens,
		"total_tokens", usage.TotalTokens, "cost_estimate", cost)
}

func spanNameKind(info *callbacks.RunInfo) (string, string) {
	if info == nil {
		return "unknown", ""
	}
	name := info.Name
	if name == "" {
		name = info.Type
	}
	if name == "" {
		name = "unknown"
	}
	return name, string(info.Component)
}

func isChatModelSpan(info *callbacks.RunInfo) bool {
	return info != nil && info.Component == components.ComponentOfChatModel
}

func (h *TraceHandler) persistChatModelIO(ctx context.Context, info *callbacks.RunInfo, output string) {
	if !isChatModelSpan(info) {
		return
	}
	state, _ := ctx.Value(spanCtxKey{}).(*spanState)
	if state == nil || state.id == 0 {
		return
	}
	rec := store.PrepareSpanIO(state.id, state.inputText, output)
	if rec.Input == "" && rec.Output == "" {
		return
	}
	if err := h.st.UpsertSpanIO(rec); err != nil {
		h.logger.WithError(err).Warn("写入模型输入输出失败", "trace_id", h.traceID, "span_id", state.id)
	}
}

func encodeChatModelInput(input callbacks.CallbackInput) string {
	in := model.ConvCallbackInput(input)
	if in == nil || len(in.Messages) == 0 {
		return ""
	}
	var b strings.Builder
	for _, msg := range in.Messages {
		if msg == nil {
			continue
		}
		writeChatMessage(&b, string(msg.Role), msg)
	}
	return strings.TrimSpace(b.String())
}

func encodeChatModelOutput(output callbacks.CallbackOutput) string {
	out := model.ConvCallbackOutput(output)
	if out == nil || out.Message == nil {
		return ""
	}
	var b strings.Builder
	writeChatMessage(&b, "assistant", out.Message)
	return strings.TrimSpace(b.String())
}

func writeChatMessage(b *strings.Builder, role string, msg *schema.Message) {
	if msg.ReasoningContent != "" {
		fmt.Fprintf(b, "reasoning: %s\n", msg.ReasoningContent)
	}
	if msg.Content != "" {
		if role == "" {
			role = "message"
		}
		fmt.Fprintf(b, "%s: %s\n", role, msg.Content)
	}
	for _, call := range msg.ToolCalls {
		fmt.Fprintf(b, "tool_call %s %s %s\n", call.ID, call.Function.Name, call.Function.Arguments)
	}
}
