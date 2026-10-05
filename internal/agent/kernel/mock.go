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
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// mockModelType 是 mock 模型在 callbacks RunInfo 中的实现标识。
const mockModelType = "Mock"

// MockScriptEnv 是脚本化 mock 响应的环境变量：值为 JSON，两种形态——
//  1. 数组：每条形如 {"content":"..."} 或
//     {"tool_calls":[{"id":"c1","name":"artifact.put","arguments":"{...}"}]}，
//     本实例按调用序消费（单 agent 场景）；
//  2. 对象 {"scripts":[{"match":"...","replies":[同上数组条目]}]}：多 agent
//     或多 Run 场景。每次调用按输入选择路由，各路由独立消费响应队列；
//     按声明顺序首个命中生效，match 为空串视为兜底路由，无命中返回默认响应。
//
// 为什么走环境变量：集成测试与手动联调需要"模型按剧本调用工具"，
// 而 mock 实例在运行时内部构建、外部拿不到引用——env 是唯一穿透点。
// 仅 mock 驱动读取，生产端点不受影响。
const MockScriptEnv = "SEMANTIC_MOCK_SCRIPT"

// 固定计量：mock 不产生真实 token 消耗，但计量/链路代码路径需要
// 非零 usage 才能被端到端验证（与旧框架 mock_provider 的约定一致）。
// 每次调用返回独立副本，避免共享指针被调用方意外改写。
func freshUsage() *schema.TokenUsage {
	return &schema.TokenUsage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30}
}

// MockReply 是脚本化的一轮模型响应。
type MockReply struct {
	// Content 响应文本。
	Content string

	// ToolCalls 本轮要求执行的工具调用；非空时 finish_reason 为 tool_calls。
	ToolCalls []schema.ToolCall
}

// MockChatModel 是预设响应的模型实现，与真实模型同接口（model.BaseChatModel）。
// 支持预设响应文本、工具调用、流式分块、错误与调用计数，
// 使上层代码（含 agent loop）可以在无真实 LLM 服务的情况下端到端测试。
//
// 与真实实现对齐的一点：mock 也主动触发 eino callbacks（OnStart/OnEnd/
// OnError/OnEndWithStreamOutput），否则 TraceHandler 对 mock 调用不可见，
// 冒烟测试就失去了对"回调 → 落库"链路的验证意义。
type MockChatModel struct {
	// mu 保护内部状态的互斥锁。
	mu sync.Mutex

	// script 脚本化响应队列：每次调用消费一条，耗尽后重复最后一条。
	script []MockReply

	// routes 路由剧本（对象形态脚本）：每次调用按输入选择一条路由，
	// 每条路由独立消费自己的响应队列。这样同一个缓存模型实例可以服务
	// 多个独立 Agent Run，又能保持单次工具循环的响应顺序。
	routes []mockRoute

	// streamChunks 预设的流式输出文本块；为空时流式输出整段响应。
	streamChunks []string

	// err 预设错误（设置后所有调用均返回此错误）。
	err error

	// generateCalls Generate 的调用次数。
	generateCalls int

	// streamCalls Stream 的调用次数。
	streamCalls int

	// inputs 记录每次调用的输入消息（调用序），供上层断言上下文组装结果。
	inputs [][]*schema.Message
}

// NewMockChatModel 创建一个 mock 模型，默认响应一段占位文本。
// 设置了 SEMANTIC_MOCK_SCRIPT 时按环境脚本响应（集成测试/手动联调用）；
// 脚本非法时全部调用返回解析错误——显性失败优于静默降级。
func NewMockChatModel() *MockChatModel {
	m := &MockChatModel{
		script: []MockReply{{Content: "（mock 模型默认响应）"}},
	}
	if raw := os.Getenv(MockScriptEnv); raw != "" {
		script, routes, err := parseMockScript(raw)
		switch {
		case err != nil:
			m.err = err
		case routes != nil:
			// 路由模式：剧本在首次调用时按输入选定（script 置空待锁定）。
			m.routes = routes
			m.script = nil
		default:
			m.script = script
		}
	}
	return m
}

// mockScriptReply 是 SEMANTIC_MOCK_SCRIPT 的单条脚本（JSON 反序列化结构）。
type mockScriptReply struct {
	// Content 响应文本。
	Content string `json:"content"`

	// ToolCalls 本轮要求执行的工具调用。
	ToolCalls []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"tool_calls"`
}

// mockRoute 是一条路由剧本（多 agent 脚本形态）：match 为输入消息内容的
// 子串匹配词（角色系统提示中的身份标识是稳定的匹配素材），replies 为命中后
// 该路由独立消费的响应队列。
type mockRoute struct {
	// match 子串匹配词；空串视为兜底路由（strings.Contains 恒真）。
	match string

	// matchAll 要求所有子串同时出现在本轮输入中。它用于真实 Server 的
	// 多 Robot Gate 精确区分同一 Agent 的多个独立任务；为空时保持旧 match 行为。
	matchAll []string

	// replies 命中后本实例按调用序消费的响应队列。
	replies []MockReply
}

// mockScriptRoute 是路由剧本的 JSON 反序列化结构。
type mockScriptRoute struct {
	// Match 子串匹配词。
	Match string `json:"match"`

	// MatchAll 中的子串必须全部命中；可与旧 Match 二选一。
	MatchAll []string `json:"match_all"`

	// Replies 响应队列（与数组形态同条目结构）。
	Replies []mockScriptReply `json:"replies"`
}

// parseMockScript 解析环境脚本：对象形态（含 scripts 数组）返回路由剧本，
// 数组形态返回单一响应队列；两种形态互斥，解析失败显式报错。
func parseMockScript(raw string) ([]MockReply, []mockRoute, error) {
	var routed struct {
		Scripts []mockScriptRoute `json:"scripts"`
	}
	if err := json.Unmarshal([]byte(raw), &routed); err == nil && len(routed.Scripts) > 0 {
		routes := make([]mockRoute, 0, len(routed.Scripts))
		for i, row := range routed.Scripts {
			replies, err := convertMockReplies(row.Replies)
			if err != nil {
				return nil, nil, fmt.Errorf("%s 第 %d 条路由: %w", MockScriptEnv, i+1, err)
			}
			routes = append(routes, mockRoute{match: row.Match, matchAll: row.MatchAll, replies: replies})
		}
		return nil, routes, nil
	}

	var rows []mockScriptReply
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil, nil, fmt.Errorf("%s 不是合法 JSON: %w", MockScriptEnv, err)
	}
	if len(rows) == 0 {
		return nil, nil, fmt.Errorf("%s 为空脚本", MockScriptEnv)
	}
	script, err := convertMockReplies(rows)
	if err != nil {
		return nil, nil, err
	}
	return script, nil, nil
}

// convertMockReplies 把脚本条目转换为 MockReply 队列（两种形态共用）。
func convertMockReplies(rows []mockScriptReply) ([]MockReply, error) {
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s 响应队列为空", MockScriptEnv)
	}
	script := make([]MockReply, 0, len(rows))
	for i, row := range rows {
		var calls []schema.ToolCall
		for _, c := range row.ToolCalls {
			if c.Name == "" {
				return nil, fmt.Errorf("%s 第 %d 条工具调用缺少 name", MockScriptEnv, i+1)
			}
			calls = append(calls, schema.ToolCall{
				ID:   c.ID,
				Type: "function",
				Function: schema.FunctionCall{
					Name:      c.Name,
					Arguments: c.Arguments,
				},
			})
		}
		script = append(script, MockReply{Content: row.Content, ToolCalls: calls})
	}
	return script, nil
}

// IsCallbacksEnabled 告诉 Eino 该模型已经自行触发回调，避免图节点再包一层
// OnStart/OnEnd，导致同一次模拟调用重复记录 Span 与计量。
func (m *MockChatModel) IsCallbacksEnabled() bool { return true }

// SetResponse 设置后续所有调用返回的文本内容。
func (m *MockChatModel) SetResponse(content string) {
	m.SetScript(MockReply{Content: content})
}

// SetToolCallResponse 设置后续所有调用返回的工具调用列表。
func (m *MockChatModel) SetToolCallResponse(calls []schema.ToolCall) {
	m.SetScript(MockReply{ToolCalls: calls})
}

// SetScript 设置脚本化响应：每次调用顺序消费一条，耗尽后重复最后一条。
// agent loop 的多轮调用（如先返回工具调用、再返回总结文本）由此建模。
func (m *MockChatModel) SetScript(replies ...MockReply) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(replies) == 0 {
		replies = []MockReply{{Content: "（mock 模型默认响应）"}}
	}
	m.script = replies
}

// SetStreamResponse 设置后续 Stream 调用逐块发送的文本列表。
// 设置后流式输出按分块发送，整段内容（含工具调用）不再出现在流帧中。
func (m *MockChatModel) SetStreamResponse(chunks []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.streamChunks = chunks
}

// SetError 设置一个错误，使后续所有 Generate 和 Stream 调用都返回该错误。
// 传入 nil 可清除已设置的错误。
func (m *MockChatModel) SetError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// GenerateCallCount 返回 Generate 被调用的次数。
func (m *MockChatModel) GenerateCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.generateCalls
}

// StreamCallCount 返回 Stream 被调用的次数。
func (m *MockChatModel) StreamCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streamCalls
}

// CallInputs 返回每次调用的输入消息（按调用序，Generate 与 Stream 混合排列）。
// 用于断言 middleware 的上下文组装结果（系统提示注入、历史重放等）。
func (m *MockChatModel) CallInputs() [][]*schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	inputs := make([][]*schema.Message, len(m.inputs))
	copy(inputs, m.inputs)
	return inputs
}

// next 消费下一条脚本响应（耗尽后重复最后一条）。调用方须持锁。
func (m *MockChatModel) next() MockReply {
	reply := m.script[0]
	if len(m.script) > 1 {
		m.script = m.script[1:]
	}
	return reply
}

// nextFor 消费下一条脚本响应。路由模式下每次调用重新匹配输入，但只推进
// 命中路由自己的响应队列，因此同一工具循环能按序执行，同时不同 Run 不串剧本。
// 调用方须持锁。
func (m *MockChatModel) nextFor(input []*schema.Message) MockReply {
	if m.routes == nil {
		return m.next()
	}
	route := m.matchRoute(input)
	if route == nil {
		return MockReply{Content: "（mock 模型默认响应）"}
	}
	reply := route.replies[0]
	if len(route.replies) > 1 {
		route.replies = route.replies[1:]
	}
	return reply
}

// matchRoute 按输入消息内容的子串匹配选定剧本。输入包含当前请求和历史对话，
// 因此必须先倒序选择最新的一条匹配消息，再按路由声明顺序选择；否则同一
// Conversation 的第二个 Plan 会被第一条历史需求劫持。match 为空串仍可作兜底。
// 无命中回退默认响应。调用方须持锁。
func (m *MockChatModel) matchRoute(input []*schema.Message) *mockRoute {
	// match_all 表达当前调用阶段与业务标记等多个条件，优先级高于普通
	// match。所有条件必须出现在同一条消息中，不能把历史 Planning 消息
	// 与当前 Execution 消息拼接后误判。倒序检查输入让当前消息优先于历史。
	for messageIndex := len(input) - 1; messageIndex >= 0; messageIndex-- {
		msg := input[messageIndex]
		if msg == nil {
			continue
		}
		for routeIndex := range m.routes {
			route := &m.routes[routeIndex]
			if len(route.matchAll) == 0 {
				continue
			}
			matched := true
			for _, required := range route.matchAll {
				if !strings.Contains(msg.Content, required) {
					matched = false
					break
				}
			}
			if matched {
				return route
			}
		}
	}
	for messageIndex := len(input) - 1; messageIndex >= 0; messageIndex-- {
		msg := input[messageIndex]
		if msg == nil {
			continue
		}
		for routeIndex := range m.routes {
			route := &m.routes[routeIndex]
			if len(route.matchAll) == 0 && strings.Contains(msg.Content, route.match) {
				return route
			}
		}
	}
	return nil
}

// buildMessage 把一条脚本响应组装为完整的 assistant 消息（含 usage）。
func buildMessage(reply MockReply) *schema.Message {
	msg := schema.AssistantMessage(reply.Content, reply.ToolCalls)
	finishReason := "stop"
	if len(reply.ToolCalls) > 0 {
		finishReason = "tool_calls"
	}
	msg.ResponseMeta = &schema.ResponseMeta{FinishReason: finishReason, Usage: freshUsage()}
	return msg
}

// Generate 返回预设的同步响应；已设置错误时直接返回错误。
func (m *MockChatModel) Generate(ctx context.Context, input []*schema.Message,
	_ ...model.Option) (*schema.Message, error) {
	ctx = callbacks.EnsureRunInfo(ctx, mockModelType, components.ComponentOfChatModel)
	ctx = callbacks.OnStart(ctx, &model.CallbackInput{Messages: input})

	m.mu.Lock()
	m.generateCalls++
	m.inputs = append(m.inputs, input)
	err := m.err
	var msg *schema.Message
	if err == nil {
		msg = buildMessage(m.nextFor(input))
	}
	m.mu.Unlock()

	if err != nil {
		callbacks.OnError(ctx, err)
		return nil, err
	}
	callbacks.OnEnd(ctx, &model.CallbackOutput{
		Message:    msg,
		TokenUsage: toCallbackUsage(msg.ResponseMeta),
	})
	return msg, nil
}

// Stream 返回预设的流式响应：预设了分块则逐块发送（usage 挂在最后一帧），
// 否则单帧发送整段响应；已设置错误时直接返回错误。
func (m *MockChatModel) Stream(ctx context.Context, input []*schema.Message,
	_ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	ctx = callbacks.EnsureRunInfo(ctx, mockModelType, components.ComponentOfChatModel)
	ctx = callbacks.OnStart(ctx, &model.CallbackInput{Messages: input})

	m.mu.Lock()
	m.streamCalls++
	m.inputs = append(m.inputs, input)
	err := m.err
	var frames []*schema.Message
	if err == nil {
		reply := m.nextFor(input)
		if len(m.streamChunks) > 0 {
			frames = make([]*schema.Message, 0, len(m.streamChunks))
			for _, chunk := range m.streamChunks {
				frames = append(frames, schema.AssistantMessage(chunk, nil))
			}
			// usage 挂在最后一帧，与 OpenAI 兼容端点的流式约定一致。
			frames[len(frames)-1].ResponseMeta = &schema.ResponseMeta{FinishReason: "stop", Usage: freshUsage()}
		} else {
			frames = []*schema.Message{buildMessage(reply)}
		}
	}
	m.mu.Unlock()

	if err != nil {
		callbacks.OnError(ctx, err)
		return nil, err
	}

	// 与 eino-ext openai 相同的回调桥接模式：消息帧转换为 CallbackOutput 帧
	// 交给 callbacks，再把处理器消费后的流转回消息帧流。
	sr, sw := schema.Pipe[*model.CallbackOutput](len(frames))
	go func() {
		defer sw.Close()
		for _, frame := range frames {
			if sw.Send(&model.CallbackOutput{
				Message:    frame,
				TokenUsage: toCallbackUsage(frame.ResponseMeta),
			}, nil) {
				return
			}
		}
	}()

	// 返回的 ctx 在本函数内不再有后续时机消费，丢弃（与 eino-ext openai 同模式）。
	_, nsr := callbacks.OnEndWithStreamOutput(ctx, schema.StreamReaderWithConvert(sr,
		func(src *model.CallbackOutput) (callbacks.CallbackOutput, error) {
			return src, nil
		}))

	return schema.StreamReaderWithConvert(nsr,
		func(src callbacks.CallbackOutput) (*schema.Message, error) {
			out, ok := src.(*model.CallbackOutput)
			if !ok || out.Message == nil {
				return nil, schema.ErrNoValue
			}
			return out.Message, nil
		}), nil
}

// toCallbackUsage 把消息元数据中的 usage 转为回调计量类型。
func toCallbackUsage(meta *schema.ResponseMeta) *model.TokenUsage {
	if meta == nil || meta.Usage == nil {
		return nil
	}
	return &model.TokenUsage{
		PromptTokens:     meta.Usage.PromptTokens,
		CompletionTokens: meta.Usage.CompletionTokens,
		TotalTokens:      meta.Usage.TotalTokens,
	}
}
