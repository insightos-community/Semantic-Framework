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
	"errors"
	"io"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// TestMockGenerateText 验证预设文本响应：内容、finish_reason、usage 与调用计数。
func TestMockGenerateText(t *testing.T) {
	m := NewMockChatModel()
	m.SetResponse("你好，我是 mock")

	msg, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatalf("Generate 不应返回错误: %v", err)
	}
	if msg.Content != "你好，我是 mock" {
		t.Errorf("响应内容不符: %q", msg.Content)
	}
	if msg.Role != schema.Assistant {
		t.Errorf("响应角色应为 assistant，实际: %q", msg.Role)
	}
	if msg.ResponseMeta == nil || msg.ResponseMeta.FinishReason != "stop" {
		t.Errorf("finish_reason 应为 stop，实际: %+v", msg.ResponseMeta)
	}
	if msg.ResponseMeta.Usage.TotalTokens != 30 {
		t.Errorf("usage 应为固定计量 10/20/30，实际: %+v", msg.ResponseMeta.Usage)
	}
	if m.GenerateCallCount() != 1 {
		t.Errorf("Generate 调用计数应为 1，实际: %d", m.GenerateCallCount())
	}
}

// TestMockGenerateToolCalls 验证预设工具调用响应。
func TestMockGenerateToolCalls(t *testing.T) {
	m := NewMockChatModel()
	m.SetToolCallResponse([]schema.ToolCall{{
		ID:       "call-1",
		Type:     "function",
		Function: schema.FunctionCall{Name: "get_weather", Arguments: `{"city":"北京"}`},
	}})

	msg, err := m.Generate(context.Background(), nil)
	if err != nil {
		t.Fatalf("Generate 不应返回错误: %v", err)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("工具调用不符: %+v", msg.ToolCalls)
	}
	if msg.ResponseMeta.FinishReason != "tool_calls" {
		t.Errorf("finish_reason 应为 tool_calls，实际: %q", msg.ResponseMeta.FinishReason)
	}
}

// TestMockScript 验证脚本化响应顺序消费、耗尽后重复最后一条。
func TestMockScript(t *testing.T) {
	m := NewMockChatModel()
	m.SetScript(
		MockReply{ToolCalls: []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "t1"}}}},
		MockReply{Content: "最终答案"},
	)

	first, err := m.Generate(context.Background(), nil)
	if err != nil {
		t.Fatalf("第一次 Generate 不应返回错误: %v", err)
	}
	if len(first.ToolCalls) != 1 {
		t.Errorf("第一次应返回工具调用，实际: %+v", first)
	}

	second, err := m.Generate(context.Background(), nil)
	if err != nil {
		t.Fatalf("第二次 Generate 不应返回错误: %v", err)
	}
	if second.Content != "最终答案" {
		t.Errorf("第二次应返回文本，实际: %q", second.Content)
	}

	// 耗尽后重复最后一条，agent loop 不会因脚本耗尽而报错。
	third, err := m.Generate(context.Background(), nil)
	if err != nil {
		t.Fatalf("第三次 Generate 不应返回错误: %v", err)
	}
	if third.Content != "最终答案" {
		t.Errorf("脚本耗尽后应重复最后一条，实际: %q", third.Content)
	}
	if m.GenerateCallCount() != 3 {
		t.Errorf("Generate 调用计数应为 3，实际: %d", m.GenerateCallCount())
	}
}

// TestMockError 验证预设错误：Generate 与 Stream 都返回该错误，nil 可清除。
func TestMockError(t *testing.T) {
	m := NewMockChatModel()
	wantErr := errors.New("mock 故障")
	m.SetError(wantErr)

	if _, err := m.Generate(context.Background(), nil); !errors.Is(err, wantErr) {
		t.Errorf("Generate 应返回预设错误，实际: %v", err)
	}
	if _, err := m.Stream(context.Background(), nil); !errors.Is(err, wantErr) {
		t.Errorf("Stream 应返回预设错误，实际: %v", err)
	}

	m.SetError(nil)
	if _, err := m.Generate(context.Background(), nil); err != nil {
		t.Errorf("清除错误后 Generate 不应返回错误: %v", err)
	}
}

// TestMockStreamChunks 验证流式分块：逐帧发送，usage 挂在最后一帧。
func TestMockStreamChunks(t *testing.T) {
	m := NewMockChatModel()
	m.SetStreamResponse([]string{"你好", "，", "世界"})

	sr, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatalf("Stream 不应返回错误: %v", err)
	}
	defer sr.Close()

	var chunks []string
	var usage *schema.TokenUsage
	for {
		frame, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("读取流帧失败: %v", err)
		}
		chunks = append(chunks, frame.Content)
		if frame.ResponseMeta != nil {
			usage = frame.ResponseMeta.Usage
		}
	}

	if len(chunks) != 3 || chunks[0] != "你好" || chunks[2] != "世界" {
		t.Errorf("流式分块不符: %v", chunks)
	}
	if usage == nil || usage.TotalTokens != 30 {
		t.Errorf("末帧应携带 usage，实际: %+v", usage)
	}
	if m.StreamCallCount() != 1 {
		t.Errorf("Stream 调用计数应为 1，实际: %d", m.StreamCallCount())
	}
}

// TestMockStreamSingleFrame 验证未设分块时流式输出整段响应（含工具调用）。
func TestMockStreamSingleFrame(t *testing.T) {
	m := NewMockChatModel()
	m.SetToolCallResponse([]schema.ToolCall{{
		ID: "call-1", Type: "function",
		Function: schema.FunctionCall{Name: "get_weather", Arguments: `{}`},
	}})

	sr, err := m.Stream(context.Background(), nil)
	if err != nil {
		t.Fatalf("Stream 不应返回错误: %v", err)
	}
	defer sr.Close()

	frame, err := sr.Recv()
	if err != nil {
		t.Fatalf("读取首帧失败: %v", err)
	}
	if len(frame.ToolCalls) != 1 {
		t.Errorf("单帧应携带工具调用，实际: %+v", frame)
	}
	if frame.ResponseMeta == nil || frame.ResponseMeta.Usage == nil {
		t.Errorf("单帧应携带 usage，实际: %+v", frame.ResponseMeta)
	}
	if _, err := sr.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("第二帧应为 EOF，实际: %v", err)
	}
}

// TestMockRoutedScript 验证对象形态脚本（多 agent 路由）：实例在首次调用
// 按输入消息内容子串选定剧本并锁定（后续调用不再重匹配）；无命中回退默认响应。
func TestMockRoutedScript(t *testing.T) {
	t.Setenv(MockScriptEnv, `{"scripts":[
		{"match":"你是查询助手","replies":[{"content":"查询结果：3 个产物"}]},
		{"match":"你是 Leader","replies":[
			{"tool_calls":[{"id":"c1","name":"ask_query","arguments":"{\"task\":\"查 artifact 列表\"}"}]},
			{"content":"总结：3 个产物。"}
		]}
	]}`)

	// leader 实例：首轮命中"你是 Leader"（系统提示），第二轮消费同剧本下一条。
	leader := NewMockChatModel()
	first, err := leader.Generate(context.Background(), []*schema.Message{
		schema.SystemMessage("你是 Leader。"), schema.UserMessage("查一下有哪些产物"),
	})
	if err != nil {
		t.Fatalf("leader 首轮调用失败: %v", err)
	}
	if len(first.ToolCalls) != 1 || first.ToolCalls[0].Function.Name != "ask_query" {
		t.Fatalf("leader 首轮应为 ask_query 工具调用，实际: %+v", first.ToolCalls)
	}
	second, err := leader.Generate(context.Background(), []*schema.Message{
		schema.SystemMessage("你是 Leader。"), schema.UserMessage("查一下有哪些产物"),
	})
	if err != nil {
		t.Fatalf("leader 次轮调用失败: %v", err)
	}
	if second.Content != "总结：3 个产物。" {
		t.Errorf("leader 次轮应消费锁定剧本的第二条，实际: %q", second.Content)
	}

	// query 实例：同样的环境脚本，按自身系统提示路由到查询剧本。
	query := NewMockChatModel()
	answer, err := query.Generate(context.Background(), []*schema.Message{
		schema.SystemMessage("你是查询助手。"), schema.UserMessage("查 artifact 列表"),
	})
	if err != nil {
		t.Fatalf("query 调用失败: %v", err)
	}
	if answer.Content != "查询结果：3 个产物" {
		t.Errorf("query 应命中查询剧本，实际: %q", answer.Content)
	}

	// 无命中实例回退默认响应，不受其他实例已锁定路由的影响。
	unmatched := NewMockChatModel()
	fallback, err := unmatched.Generate(context.Background(), []*schema.Message{
		schema.SystemMessage("你是审计助手。"), schema.UserMessage("检查"),
	})
	if err != nil {
		t.Fatalf("无命中实例调用失败: %v", err)
	}
	if fallback.Content != "（mock 模型默认响应）" {
		t.Errorf("无命中应回退默认响应，实际: %q", fallback.Content)
	}
}

func TestMockRoutedScriptPrefersLatestPlainMatch(t *testing.T) {
	t.Setenv(MockScriptEnv, `{"scripts":[
		{"match":"PLAN-A","replies":[{"content":"A"}]},
		{"match":"PLAN-B","replies":[{"content":"B"}]}
	]}`)
	m := NewMockChatModel()
	answer, err := m.Generate(context.Background(), []*schema.Message{
		schema.UserMessage("历史需求 PLAN-A"),
		schema.AssistantMessage("A 已完成", nil),
		schema.UserMessage("当前需求 PLAN-B"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Content != "B" {
		t.Fatalf("普通 match 必须优先当前消息，实际: %q", answer.Content)
	}
}

func TestMockRoutedScriptMatchAll(t *testing.T) {
	t.Setenv(MockScriptEnv, `{"scripts":[
		{"match":"PLAN-B","replies":[{"content":"planning"}]},
		{"match_all":["请把这个 Worker Task","EXEC-B"],"replies":[{"content":"subtasks"}]},
		{"match_all":["完成这个 Worker Task","EXEC-B"],"replies":[{"content":"Robot B"}]},
		{"match_all":["完成这个 Worker Task","EXEC-A"],"replies":[{"content":"Robot A"}]}
	]}`)
	m := NewMockChatModel()
	answer, err := m.Generate(context.Background(), []*schema.Message{
		schema.SystemMessage("你是 Robot Agent。"),
		schema.UserMessage("完成这个 Worker Task，目标是 EXEC-A"),
	})
	if err != nil {
		t.Fatalf("match_all 调用失败: %v", err)
	}
	if answer.Content != "Robot A" {
		t.Fatalf("match_all 应同时匹配运行阶段与任务标记，实际: %q", answer.Content)
	}
	// Workflow Runtime 会复用同一模型装配执行多个 Robot Task。第二个 Run
	// 必须重新匹配自己的任务标记。历史中既保留普通 PLAN-B，也保留同一
	// EXEC-B 的 Task Planning 请求；当前执行消息必须优先，且 match_all
	// 不能跨两条 user 消息拼接后重新命中 Planning 路由。
	answer, err = m.Generate(context.Background(), []*schema.Message{
		schema.SystemMessage("你是 Robot Agent。"),
		schema.UserMessage("历史请求 PLAN-B"),
		schema.UserMessage("请把这个 Worker Task 拆成 SubTask，目标是 EXEC-B"),
		schema.AssistantMessage("subtasks", nil),
		schema.UserMessage("完成这个 Worker Task，目标是 EXEC-B"),
	})
	if err != nil {
		t.Fatalf("同一模型跨 Run 重新路由失败: %v", err)
	}
	if answer.Content != "Robot B" {
		t.Fatalf("第二个 Run 应命中 Robot B，实际: %q", answer.Content)
	}
}
