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
	"net"
	"strings"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/log"
)

// EventKind 是运行事件的类别。
type EventKind int

const (
	// EventTextDelta 模型文本增量（流式帧；多次出现，拼接即完整回复）。
	EventTextDelta EventKind = iota + 1

	// EventReasoningDelta 模型服务显式返回的推理内容增量。
	EventReasoningDelta

	// EventToolCall 模型发起工具调用（参数仍是模型生成的原始 JSON 文本）。
	EventToolCall

	// EventToolResult 工具执行完成（ToolName 标识工具，Text 为工具结果原文）。
	EventToolResult

	// EventDone 本轮运行正常结束（携带轮次与累计用量）。
	EventDone

	// EventError 运行出错（Err 非空；出现后事件流随即关闭）。
	EventError

	// EventInterrupted 运行中断（危险工具等待人工审批）：断点已持久化，
	// 事件流随即关闭（不出现 EventDone）；调用方处理完中断原因后用
	// Runner.Resume 恢复执行。Turns/Usage 携带中断前的部分累计。
	EventInterrupted

	// EventSubAgentDelta SubAgent 文本增量（委派执行过程的冒泡事件；
	// AgentID 标识来源成员）。
	EventSubAgentDelta

	// EventSubAgentResult SubAgent 委派完成：Text 为结果全文（SubAgent
	// 最终回复，即 leader 看到的工具结果），Task 为委派任务文本
	// （与委派调用按 call_id 配对回填；提取失败时降级为空）。
	EventSubAgentResult
)

// Interrupt 是一个中断点的用户视图（由内核中断事件转换而来）。
type Interrupt struct {
	// ID 中断点的唯一标识（恢复时按它定向注入数据）。
	ID string

	// Info 中断负载（由中断发起方提供，如安全审批请求；调用方按类型断言解析）。
	Info any
}

// Usage 是一次运行的累计 token 用量（各模型调用之和）。
type Usage struct {
	// PromptTokens 输入侧累计 tokens。
	PromptTokens int

	// CompletionTokens 输出侧累计 tokens。
	CompletionTokens int

	// TotalTokens 累计总 tokens。
	TotalTokens int
}

// add 累加一次模型调用的用量。
func (u *Usage) add(prompt, completion, total int) {
	u.PromptTokens += prompt
	u.CompletionTokens += completion
	u.TotalTokens += total
}

// Event 是 kernel 暴露给运行时的运行事件（由 adk.AgentEvent 转换而来，
// eino 类型不外泄）。
type Event struct {
	// Kind 事件类别。
	Kind EventKind

	// Text 文本增量（EventTextDelta/EventSubAgentDelta）、普通工具结果原文
	// （EventToolResult）或委派结果全文（EventSubAgentResult）。
	Text string

	// ToolName 刚执行完成的工具名（Kind 为 EventToolResult 时有效）。
	ToolName string

	// CallID 工具调用 ID，用于把调用参数与执行结果稳定配对。
	CallID string

	// Arguments 工具调用参数的原始 JSON 文本（EventToolCall 时有效）。
	Arguments string

	// AgentID 事件来源的 SubAgent 成员 ID（Kind 为 EventSubAgent* 时有效）。
	AgentID string

	// Task 委派给 SubAgent 的任务文本（Kind 为 EventSubAgentResult 时有效）。
	Task string

	// Turns 模型调用轮次（EventDone 为全轮次；EventInterrupted 为中断前的部分轮次）。
	Turns int

	// Usage 累计 token 用量（EventDone 为全量；EventInterrupted 为中断前的部分量）。
	Usage Usage

	// Interrupts 中断点清单（Kind 为 EventInterrupted 时有效，含全部根因中断点）。
	Interrupts []Interrupt

	// Err 运行错误（Kind 为 EventError 时有效）。
	Err error
}

// RunOptions 是一次 Run 的观测与恢复参数。TraceID 可由上层提前生成并
// 与 Run 记录一起保存；为空时由 kernel 生成。CheckpointID 为空表示不恢复。
type RunOptions struct {
	CheckpointID string
	TraceID      string
}

type runnerTraceConfig struct {
	store   *store.Store
	logger  *log.Logger
	options TraceOptions
}

func newRunnerTraceConfig(cfg AgentConfig) *runnerTraceConfig {
	if cfg.Store == nil {
		return nil
	}
	return &runnerTraceConfig{store: cfg.Store, logger: cfg.Logger, options: TraceOptions{
		Agent: agentName(cfg.Name), Role: cfg.Role, Purpose: cfg.Purpose,
		Model: cfg.ModelName, Price: cfg.Price,
	}}
}

// NewTraceID 生成一个 Agent Run 的 trace_id。上层可在创建 Run 记录前调用，
// 随后通过 RunOptions 传回 kernel，保证持久化 Run 与实际回调使用同一 ID。
func NewTraceID() string { return uuid.NewString() }

// Runner 是 kernel 封装的可复用 Agent 装配。Runner 只缓存模型、工具和
// middleware；Run 级 TraceHandler 仅在执行期间存在。
type Runner struct {
	// inner 底层 eino 运行器。
	inner *adk.Runner

	// name Agent 名（日志/归因用）。
	name string

	// traceConfig 是创建每 Run TraceHandler 所需的不可变配置。
	traceConfig *runnerTraceConfig

	// subAgentTools 委派工具名 → SubAgent 名 的映射（事件冒泡归因用；
	// 工具集无委派工具时为 nil）。
	subAgentTools map[string]string

	// mu 保护 callTrackers 与 runTraces。
	mu sync.Mutex

	// runTraces 保存发生中断、可能 Resume 的 Run TraceHandler。正常终态后删除。
	runTraces map[string]*TraceHandler

	// callTrackers 中断 run 的委派调用配对表（断点 ID → call_id → 调用
	// 信息）：委派结果的任务文本配对是 run 级状态而非流级——中断会关闭
	// 当前事件流，恢复出的新流仍需按 call_id 回填任务文本。只有绑定断点 ID
	// 的流才注册（无断点的流不可恢复，无需跨流延续）；流正常结束（未
	// 中断）即注销，中断后永不恢复的条目随会话运行器淘汰一并回收。
	callTrackers map[string]map[string]subAgentCall
}

// Name 返回 Agent 名。
func (r *Runner) Name() string {
	return r.name
}

// Run 启动一轮对话运行：history 为会话历史（S7），text 为最新用户消息
// （S8，追加在历史之后）；返回事件流供调用方消费。
// 为什么历史每次全量传入：内核的 Run 是请求级模型，无跨 Run 记忆——
// 会话状态的唯一事实源是 store（chat_messages），每次 Run 由调用方重放。
func (r *Runner) Run(ctx context.Context, history []*schema.Message, text string) (*EventStream, error) {
	return r.RunWithCheckpoint(ctx, history, text, "")
}

// RunWithCheckpoint 同 Run，但为本次运行绑定断点 ID：运行中断时断点
// 持久化到 CheckPointStore（BuildAgent 挂接了 store 才生效），
// 之后可用同 ID 经 Resume 恢复。checkpointID 为空时不做断点持久化
// （无审批工具的运行不需要；中断事件仍会正常上行，只是无法恢复）。
func (r *Runner) RunWithCheckpoint(ctx context.Context, history []*schema.Message, text, checkpointID string) (*EventStream, error) {
	messages := append([]*schema.Message(nil), history...)
	messages = append(messages, schema.UserMessage(text))
	return r.RunMessagesWithCheckpoint(ctx, messages, checkpointID)
}

// RunMessagesWithCheckpoint 直接运行 Eino schema.Message 序列。调用方负责
// 把最新用户消息追加到历史末尾；统一入口避免在 Runtime 与 Kernel 之间维护
// 第二套 role/content/attachment 转换模型。
func (r *Runner) RunMessagesWithCheckpoint(ctx context.Context, messages []*schema.Message,
	checkpointID string) (*EventStream, error) {
	return r.RunMessages(ctx, messages, RunOptions{CheckpointID: checkpointID})
}

// RunMessages 启动一次具有独立 Trace 的执行。TraceHandler 通过 Eino 官方
// adk.WithCallbacks 挂入本次 Run，不写入缓存 Agent 的 middleware 栈。
func (r *Runner) RunMessages(ctx context.Context, messages []*schema.Message,
	runOpts RunOptions) (*EventStream, error) {
	if len(messages) == 0 || messages[len(messages)-1] == nil {
		return nil, errors.New("运行输入必须包含非空的最新消息")
	}
	var opts []adk.AgentRunOption
	if runOpts.CheckpointID != "" {
		opts = append(opts, adk.WithCheckPointID(runOpts.CheckpointID))
	}
	handler := r.startRunTrace(runOpts.CheckpointID, runOpts.TraceID)
	if handler != nil {
		opts = append(opts, adk.WithCallbacks(handler))
	}
	es := r.newEventStream(runOpts.CheckpointID, handler)
	go es.pump(ctx, r.inner.Run(ctx, messages, opts...))
	return es, nil
}

// Resume 从中断点恢复执行：checkpointID 定位断点（= RunWithCheckpoint
// 绑定的 ID），targets 按中断点 ID 注入恢复数据（如审批结果）。
// 恢复产出新的事件流（可能再次中断——同 run 可多次暂停）。
// 一致性约束：调用方必须先把中断原因的处理结果持久化（如审批应答落库）
// 再调用本方法——恢复执行会基于这些结果继续，先落库后恢复才经得起崩溃复盘。
func (r *Runner) Resume(ctx context.Context, checkpointID string, targets map[string]any) (*EventStream, error) {
	handler := r.resumeTrace(checkpointID)
	var opts []adk.AgentRunOption
	if handler != nil {
		opts = append(opts, adk.WithCallbacks(handler))
	}
	iter, err := r.inner.ResumeWithParams(ctx, checkpointID,
		&adk.ResumeParams{Targets: targets}, opts...)
	if err != nil {
		return nil, fmt.Errorf("从断点 %q 恢复失败: %w", checkpointID, err)
	}
	es := r.newEventStream(checkpointID, handler)
	go es.pump(ctx, iter)
	return es, nil
}

func (r *Runner) startRunTrace(checkpointID, traceID string) *TraceHandler {
	if r.traceConfig == nil {
		return nil
	}
	opts := r.traceConfig.options
	opts.TraceID = traceID
	handler := NewTraceHandler(r.traceConfig.store, r.traceConfig.logger, opts)
	if checkpointID != "" {
		r.mu.Lock()
		if r.runTraces == nil {
			r.runTraces = make(map[string]*TraceHandler)
		}
		r.runTraces[checkpointID] = handler
		r.mu.Unlock()
	}
	return handler
}

func (r *Runner) resumeTrace(checkpointID string) *TraceHandler {
	if r.traceConfig == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runTraces[checkpointID]
}

// newEventStream 绑定本次 Run 的 trace_id，并为中断恢复保留 TraceHandler
// 与 SubAgent 调用配对表。只有正常终态才清理；中断流结束后 Resume 仍需使用。
func (r *Runner) newEventStream(checkpointID string, handler *TraceHandler) *EventStream {
	es := &EventStream{ch: make(chan Event, 16), parentName: r.name,
		subAgentTools: r.subAgentTools}
	if handler != nil {
		es.traceID = handler.TraceID()
		es.waitTraces = handler.WaitStreams
	}
	if checkpointID == "" {
		return es
	}
	r.mu.Lock()
	if len(r.subAgentTools) > 0 {
		if r.callTrackers == nil {
			r.callTrackers = make(map[string]map[string]subAgentCall)
		}
		tracker := r.callTrackers[checkpointID]
		if tracker == nil {
			tracker = make(map[string]subAgentCall)
			r.callTrackers[checkpointID] = tracker
		}
		es.pendingCalls = tracker
	}
	r.mu.Unlock()
	es.terminalDone = func() {
		r.mu.Lock()
		delete(r.callTrackers, checkpointID)
		delete(r.runTraces, checkpointID)
		r.mu.Unlock()
	}
	return es
}

// EventStream 是一次运行的事件流：由后台 goroutine 消费内核事件并转换，
// Next 顺序读取直到流关闭（运行结束）。
type EventStream struct {
	waitTraces func()
	modelTurn  int
	// ch 转换后的事件通道（pump 关闭它表示运行结束）。
	ch chan Event

	// traceID 是本次 Run 的链路 ID；未配置 Store 时为空。
	traceID string

	// parentName 父 agent 名：冒泡事件（EmitInternalEvents 转发的子 agent
	// 事件）的识别基准——AgentName ≠ parentName 即冒泡事件。
	parentName string

	// subAgentTools 委派工具名 → SubAgent 名 的映射（nil = 无委派面，
	// 全部走既有转换路径，零开销）。
	subAgentTools map[string]string

	// pendingCalls 待配对的委派调用（call_id → 调用信息）：委派结果事件
	// 到达时按 call_id 回填任务文本。绑定断点 ID 的流共享 Runner 侧同 ID
	// 的配对表（中断-恢复跨流延续，见 newEventStream）。
	pendingCalls map[string]subAgentCall

	// terminalDone 在正常终态清理本 Run 的恢复期状态；中断时不调用。
	terminalDone func()
}

// subAgentCall 是一次待配对的委派调用（工具结果按 call_id 回填结果用）。
type subAgentCall struct {
	// agentID 委派的 SubAgent 成员 ID。
	agentID string

	// task 委派任务文本（从工具调用参数提取）。
	task string
}

// TraceID 返回本次 Run 的链路 ID。
func (s *EventStream) TraceID() string { return s.traceID }

// Next 读取下一个事件；ok 为 false 表示运行结束（流已关闭）。
func (s *EventStream) Next() (Event, bool) {
	ev, ok := <-s.ch
	return ev, ok
}

// pump 消费内核事件迭代器并转换为 kernel Event：
//   - 助手流式事件 → 逐帧 EventTextDelta（累计轮次与用量，usage 在末帧）；
//   - 冒泡的 SubAgent 事件（EmitInternalEvents 转发，AgentName ≠ 父 agent 名）
//     → 助手文本逐帧 EventSubAgentDelta，其余排空——轮次/用量不归并父 run
//     （SubAgent 有自己的 trace 链路，父 run 只统计本级模型调用）；
//   - 工具结果事件 → EventToolResult（保留结果原文供受控调试界面展示）；
//     委派工具的结果
//     → EventSubAgentResult（结果全文=SubAgent 最终回复，按 call_id 配对
//     回填任务文本）；
//   - 中断事件 → EventInterrupted（携带根因中断点与中断前的部分轮次/用量，
//     流排空后不再发 EventDone——运行未结束，等 Runner.Resume 恢复）；
//   - 错误事件 → EventError（继续排空迭代器，避免上游 goroutine 阻塞泄漏）；
//   - 迭代器耗尽 → EventDone（携带轮次与累计用量）后关闭流。
func (s *EventStream) pump(ctx context.Context, iter *adk.AsyncIterator[*adk.AgentEvent]) {
	defer close(s.ch)

	var turns int
	var usage Usage
	var interrupted bool
	var failed bool
	for {
		ev, ok := iter.Next()
		if !ok {
			break
		}
		if ev.Err != nil {
			failed = true
			// A policy may terminate the run before the next model call. Preserve
			// usage from the completed model round for that terminal event.
			s.ch <- Event{Kind: EventError, Err: ev.Err, Turns: turns, Usage: usage}
			continue
		}
		if ev.Action != nil && ev.Action.Interrupted != nil {
			// 中断：checkpoint 已由内核在事件上行前持久化（adk runner 的
			// 顺序保证），这里只做转换转发，不触碰断点。
			interrupted = true
			s.ch <- Event{
				Kind:       EventInterrupted,
				Interrupts: rootCauses(ev.Action.Interrupted.InterruptContexts),
				Turns:      turns,
				Usage:      usage,
			}
			continue
		}
		if ev.Output == nil || ev.Output.MessageOutput == nil {
			continue
		}

		mv := ev.Output.MessageOutput
		// 冒泡的 SubAgent 事件：AgentName 是子 agent 名（eino flow 按来源
		// agent 标注），与父 agent 名不同即冒泡。转发的事件不进父 run 的
		// runSession（eino 契约），这里也不计轮次/用量，语义一致。
		if ev.AgentName != "" && ev.AgentName != s.parentName {
			s.forwardSubAgent(ev.AgentName, mv)
			continue
		}
		switch mv.Role {
		case schema.Assistant:
			turns++
			s.modelTurn = turns
			frameUsage, err := s.forwardAssistant(mv)
			usage.add(frameUsage.PromptTokens, frameUsage.CompletionTokens, frameUsage.TotalTokens)
			if err != nil {
				failed = true
				s.ch <- Event{Kind: EventError, Err: err}
			}
		case schema.Tool:
			if s.forwardSubAgentResult(mv) {
				continue
			}
			// 普通工具结果：消费消息（流式时重组）并保留结果原文。读取失败
			// 不反向中断已经完成的工具调用，降级为空结果，最终答案仍由模型给出。
			var result, callID string
			if msg, err := mv.GetMessage(); err == nil && msg != nil {
				result, callID = msg.Content, msg.ToolCallID
			}
			s.ch <- Event{Kind: EventToolResult, ToolName: mv.ToolName, CallID: callID, Text: result}
		default:
			drainStream(mv)
		}
	}
	// 中断后同一 TraceHandler 会在 Resume 上继续 Add 流式回调。
	// 这里若 Wait，会和 Resume 的 WaitGroup.Add 重叠，触发 -race。
	// 未完成的 drain 由 Resume 终态的 Wait 一并收口。
	if s.waitTraces != nil && !interrupted {
		s.waitTraces()
	}
	if !interrupted {
		if ctx.Err() != nil && !failed {
			failed = true
			s.ch <- Event{Kind: EventError, Err: ctx.Err()}
		}
		if !failed {
			s.ch <- Event{Kind: EventDone, Turns: turns, Usage: usage}
		}
		// 成功或失败都是终态，释放本 Run 的恢复期状态；只有等待审批的
		// 中断需要保留，供 Resume 继续使用同一 Trace 和调用配对表。
		if s.terminalDone != nil {
			s.terminalDone()
		}
	}
}

// rootCauses 提取中断链中的全部根因中断点（可能多个：同轮并行工具调用
// 同时中断）。根因是恢复数据的定向目标；无根因时保底返回一个空信息点
// （防御性分支，正常中断链必有根因）。
func rootCauses(contexts []*adk.InterruptCtx) []Interrupt {
	var interrupts []Interrupt
	for _, ic := range contexts {
		if ic != nil && ic.IsRootCause {
			interrupts = append(interrupts, Interrupt{ID: ic.ID, Info: ic.Info})
		}
	}
	if len(interrupts) == 0 {
		interrupts = append(interrupts, Interrupt{})
	}
	return interrupts
}

// forwardAssistant 消费一个助手事件：流式时逐帧转发文本增量，
// 非流式时整段转发；返回该次模型调用的用量（流式约定 usage 在末帧）。
// 工具集含委派工具时同时收集帧重组 tool call（trackSubAgentCalls），
// 为委派结果事件配对任务文本。
func (s *EventStream) forwardAssistant(mv *adk.MessageVariant) (Usage, error) {
	var usage Usage
	if !mv.IsStreaming {
		if mv.Message == nil {
			return usage, nil
		}
		splitter := &thinkTagSplitter{}
		s.forwardModelContent(splitter.Write(mv.Message.Content), EventTextDelta, "")
		s.forwardModelContent(splitter.Flush(), EventTextDelta, "")
		if mv.Message.ReasoningContent != "" {
			s.ch <- Event{Kind: EventReasoningDelta, Text: mv.Message.ReasoningContent, Turns: s.modelTurn}
		}
		s.forwardToolCalls("", mv.Message)
		s.trackSubAgentCalls(mv.Message)
		return messageUsage(mv.Message), nil
	}

	// 工具调用可能分帧到达，始终收集帧并在结束后重组，才能稳定取得
	// call_id、工具名和完整参数。
	collect := true
	var frames []*schema.Message
	splitter := &thinkTagSplitter{}
	for {
		frame, err := mv.MessageStream.Recv()
		if errors.Is(err, io.EOF) {
			s.forwardModelContent(splitter.Flush(), EventTextDelta, "")
			break
		}
		if err != nil {
			return usage, wrapStreamReadError(err)
		}
		if frame == nil {
			continue
		}
		if collect {
			frames = append(frames, frame)
		}
		s.forwardModelContent(splitter.Write(frame.Content), EventTextDelta, "")
		if frame.ReasoningContent != "" {
			s.ch <- Event{Kind: EventReasoningDelta, Text: frame.ReasoningContent, Turns: s.modelTurn}
		}
		if u := messageUsage(frame); u.TotalTokens > 0 {
			usage = u
		}
	}
	if collect && len(frames) > 0 {
		// 流式 tool call 分帧到达（参数按 index 切片），重组后才能拿到完整
		// 参数文本。重组失败只影响委派结果事件的任务字段，降级为空文本而不阻断 run。
		if full, err := schema.ConcatMessages(frames); err == nil {
			s.forwardToolCalls("", full)
			s.trackSubAgentCalls(full)
		}
	}
	return usage, nil
}

// forwardModelContent 把 thinkTagSplitter 的有序片段转换为内核事件。
// textKind 由调用场景决定：主 Agent 使用 EventTextDelta，SubAgent 使用
// EventSubAgentDelta；思考片段始终使用 EventReasoningDelta。
func (s *EventStream) forwardModelContent(parts []contentPart, textKind EventKind, agentID string) {
	for _, part := range parts {
		kind := textKind
		if part.reasoning {
			kind = EventReasoningDelta
		}
		s.ch <- Event{Kind: kind, AgentID: agentID, Text: part.text, Turns: s.modelTurn}
	}
}

// forwardToolCalls 把助手消息中的普通工具调用转成显式事件。父 Agent 的
// 委派工具不重复作为普通工具展示（委派已有 subagent.* 独立时间线）；
// SubAgent 冒泡事件携带 agentID，运行时据此归到对应成员而非 Leader。
func (s *EventStream) forwardToolCalls(agentID string, msg *schema.Message) {
	if msg == nil {
		return
	}
	for _, tc := range msg.ToolCalls {
		if agentID == "" {
			if _, delegated := s.subAgentTools[tc.Function.Name]; delegated {
				continue
			}
		}
		s.ch <- Event{
			Kind: EventToolCall, AgentID: agentID, ToolName: tc.Function.Name,
			CallID: tc.ID, Arguments: tc.Function.Arguments,
		}
	}
}

// trackSubAgentCalls 记录助手消息中的委派调用（call_id → 任务文本）：
// 委派完成（工具结果到达）时按 call_id 配对回填（forwardSubAgentResult）。
func (s *EventStream) trackSubAgentCalls(msg *schema.Message) {
	if len(s.subAgentTools) == 0 || msg == nil {
		return
	}
	for _, tc := range msg.ToolCalls {
		agentID, ok := s.subAgentTools[tc.Function.Name]
		if !ok {
			continue
		}
		if s.pendingCalls == nil {
			s.pendingCalls = make(map[string]subAgentCall)
		}
		s.pendingCalls[tc.ID] = subAgentCall{agentID: agentID, task: parseTaskArg(tc.Function.Arguments)}
	}
}

// parseTaskArg 从委派调用参数提取任务文本（{task: string} 契约）；解析失败
// 取参数原文——任务文本只是摘要素材，模型写出非契约参数不应让委派结果事件丢失。
func parseTaskArg(arguments string) string {
	var args delegationArgs
	if err := json.Unmarshal([]byte(arguments), &args); err == nil && args.Task != "" {
		return args.Task
	}
	return arguments
}

// forwardSubAgent 转换冒泡的 SubAgent 事件：文本、工具调用与工具结果均
// 显式携带 agentID 下行，避免被归并成 Leader 的运行活动。
// 流读取失败不上报错误事件：冒泡流只是旁路拷贝（eino 对流 Copy 扇出），
// 委派的真实结果/错误会经工具结果路径到达，重复上报会误杀健康的父 run。
func (s *EventStream) forwardSubAgent(agentID string, mv *adk.MessageVariant) {
	if mv.Role == schema.Tool {
		msg, err := mv.GetMessage()
		if err != nil || msg == nil {
			return
		}
		s.ch <- Event{Kind: EventToolResult, AgentID: agentID, ToolName: mv.ToolName,
			CallID: msg.ToolCallID, Text: msg.Content}
		return
	}
	if mv.Role != schema.Assistant {
		drainStream(mv)
		return
	}
	if !mv.IsStreaming {
		splitter := &thinkTagSplitter{}
		if mv.Message != nil {
			s.forwardModelContent(splitter.Write(mv.Message.Content), EventSubAgentDelta, agentID)
			s.forwardModelContent(splitter.Flush(), EventSubAgentDelta, agentID)
		}
		if mv.Message != nil && mv.Message.ReasoningContent != "" {
			s.ch <- Event{Kind: EventReasoningDelta, AgentID: agentID, Text: mv.Message.ReasoningContent}
		}
		s.forwardToolCalls(agentID, mv.Message)
		return
	}
	var frames []*schema.Message
	splitter := &thinkTagSplitter{}
	for {
		frame, err := mv.MessageStream.Recv()
		if errors.Is(err, io.EOF) {
			s.forwardModelContent(splitter.Flush(), EventSubAgentDelta, agentID)
			if full, concatErr := schema.ConcatMessages(frames); concatErr == nil {
				s.forwardToolCalls(agentID, full)
			}
			return
		}
		if err != nil {
			return
		}
		if frame != nil {
			frames = append(frames, frame)
			s.forwardModelContent(splitter.Write(frame.Content), EventSubAgentDelta, agentID)
			if frame.ReasoningContent != "" {
				s.ch <- Event{Kind: EventReasoningDelta, AgentID: agentID, Text: frame.ReasoningContent}
			}
		}
	}
}

// forwardSubAgentResult 处理委派工具的结果事件：结果 = SubAgent 最终回复
// 全文（即 leader 看到的工具结果），转为 EventSubAgentResult 并按 call_id
// 配对回填任务文本，返回 true；普通工具返回 false（调用方走 EventToolResult）。
func (s *EventStream) forwardSubAgentResult(mv *adk.MessageVariant) bool {
	agentID, ok := s.subAgentTools[mv.ToolName]
	if !ok {
		return false
	}
	msg, err := mv.GetMessage() // 流式则重组（委派工具结果通常非流式）
	if err != nil {
		s.ch <- Event{Kind: EventError, Err: fmt.Errorf("读取委派工具 %q 的结果失败: %w", mv.ToolName, err)}
		return true
	}
	var task, text string
	if msg != nil {
		text = msg.Content
		if call, ok := s.pendingCalls[msg.ToolCallID]; ok {
			task = call.task
			delete(s.pendingCalls, msg.ToolCallID)
		}
	}
	s.ch <- Event{Kind: EventSubAgentResult, AgentID: agentID, Task: task, Text: text}
	return true
}

// drainStream 排空事件携带的消息流（流只能读一次，不消费必须关闭防泄漏）。
func drainStream(mv *adk.MessageVariant) {
	if mv.IsStreaming && mv.MessageStream != nil {
		mv.MessageStream.Close()
	}
}

// wrapStreamReadError 把模型 HTTP/流式超时从“读帧失败”改写成可操作的超时说明。
// context.Canceled 是用户或上层取消，不当作端点超时。
func wrapStreamReadError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("读取模型流式帧失败: %w", err)
	}
	if isModelRequestTimeout(err) {
		return fmt.Errorf("模型请求超时（timeout_seconds，默认 %d）：整次流式读取超过 HTTP 时限: %w",
			DefaultModelRequestTimeoutSeconds, err)
	}
	return fmt.Errorf("读取模型流式帧失败: %w", err)
}

func isModelRequestTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "Client.Timeout") || strings.Contains(msg, "context deadline exceeded")
}

// messageUsage 提取消息元数据中的 token 用量。
func messageUsage(msg *schema.Message) Usage {
	var usage Usage
	if msg.ResponseMeta != nil && msg.ResponseMeta.Usage != nil {
		usage.PromptTokens = msg.ResponseMeta.Usage.PromptTokens
		usage.CompletionTokens = msg.ResponseMeta.Usage.CompletionTokens
		usage.TotalTokens = msg.ResponseMeta.Usage.TotalTokens
	}
	return usage
}
