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
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/summarization"
	"github.com/cloudwego/eino/schema"
)

// TestContextMiddleware 验证自研 context middleware 的系统提示组装：
// AGENT.md 正文 + SAFETY 总则段拼接后前置注入，且重复调用幂等（只注入一次）。
func TestContextMiddleware(t *testing.T) {
	mw := newContextMiddleware("# Role\n你是 Leader。", "# 安全总则\n危险操作必须审批。")

	state := &adk.ChatModelAgentState{Messages: []*schema.Message{schema.UserMessage("你好")}}
	_, state, err := mw.BeforeModelRewriteState(context.Background(), state, &adk.ModelContext{})
	if err != nil {
		t.Fatalf("BeforeModelRewriteState 失败: %v", err)
	}
	if len(state.Messages) != 2 || state.Messages[0].Role != schema.System {
		t.Fatalf("系统提示应注入到消息头部，实际: %+v", state.Messages)
	}
	if !strings.Contains(state.Messages[0].Content, "你是 Leader。") ||
		!strings.Contains(state.Messages[0].Content, "危险操作必须审批。") {
		t.Errorf("系统提示应包含 AGENT.md 正文与 SAFETY 段，实际: %q", state.Messages[0].Content)
	}

	// 幂等：state 跨轮持久化后再次调用，系统提示不重复注入。
	_, state, err = mw.BeforeModelRewriteState(context.Background(), state, &adk.ModelContext{})
	if err != nil {
		t.Fatalf("二次 BeforeModelRewriteState 失败: %v", err)
	}
	if len(state.Messages) != 2 {
		t.Errorf("系统提示不应重复注入，实际消息数: %d", len(state.Messages))
	}
}

// TestSummarizationTrigger 验证 summarization middleware 的触发与压缩行为：
// token 计数超阈值时，历史被摘要模型生成的总结替换。
// 为什么直接测 middleware 而不走 BuildAgent：eino 默认 token 估算把工具
// JSON 也计入（中文按字节），经 BuildAgent 构造"恰好第二轮触发"依赖估算
// 细节，脆弱；自定义 TokenCounter 让触发条件确定。栈的装配顺序由
// TestAgentsMDInjection 等端到端路径覆盖。
func TestSummarizationTrigger(t *testing.T) {
	m := NewMockChatModel()
	m.SetResponse("历史总结")

	mw, err := summarization.New(context.Background(), &summarization.Config{
		Model:   m,
		Trigger: &summarization.TriggerCondition{ContextTokens: 24},
		TokenCounter: func(_ context.Context, input *summarization.TokenCounterInput) (int, error) {
			// 确定性计数：每条消息 10 tokens；阈值 24 → 3 条及以上即触发。
			return len(input.Messages) * 10, nil
		},
	})
	if err != nil {
		t.Fatalf("构建 summarization middleware 失败: %v", err)
	}

	state := &adk.ChatModelAgentState{Messages: []*schema.Message{
		schema.SystemMessage("系统提示"),
		schema.UserMessage("之前的问题"),
		schema.AssistantMessage("之前的回答", nil),
	}}
	_, got, err := mw.BeforeModelRewriteState(context.Background(), state, &adk.ModelContext{})
	if err != nil {
		t.Fatalf("BeforeModelRewriteState 失败: %v", err)
	}
	if m.GenerateCallCount() != 1 {
		t.Fatalf("超阈值应触发一次摘要调用，实际 Generate=%d", m.GenerateCallCount())
	}
	// 压缩结果：系统消息保留，历史被单条摘要替换。
	if len(got.Messages) != 2 || got.Messages[0].Role != schema.System {
		t.Fatalf("压缩后应为 [system, summary]，实际: %+v", got.Messages)
	}
	if !strings.Contains(got.Messages[1].Content, "历史总结") {
		t.Errorf("摘要消息应包含模型生成的总结，实际: %q", got.Messages[1].Content)
	}

	// 未超阈值不触发：两条消息（20 tokens < 24）。
	state2 := &adk.ChatModelAgentState{Messages: []*schema.Message{
		schema.UserMessage("问题"),
	}}
	_, got2, err := mw.BeforeModelRewriteState(context.Background(), state2, &adk.ModelContext{})
	if err != nil {
		t.Fatalf("二次 BeforeModelRewriteState 失败: %v", err)
	}
	if len(got2.Messages) != 1 || m.GenerateCallCount() != 1 {
		t.Errorf("未超阈值不应触发摘要，实际消息数=%d Generate=%d", len(got2.Messages), m.GenerateCallCount())
	}
}

// TestContextMiddlewareForeignSystemMessage 验证头部存在外来 system 消息时
// 的注入行为：skill middleware 的使用指引经 adk Instruction 通道占据头部
// system 位，context middleware 的幂等判定按内容识别——S1 仍插入到其前
// （[S1, 外来指引, user]），二次调用不重复注入。
func TestContextMiddlewareForeignSystemMessage(t *testing.T) {
	mw := newContextMiddleware("# Role\n你是 Leader。", "")

	state := &adk.ChatModelAgentState{Messages: []*schema.Message{
		schema.SystemMessage("# Skill 系统\n使用指引（外来 system 消息）"),
		schema.UserMessage("你好"),
	}}
	_, state, err := mw.BeforeModelRewriteState(context.Background(), state, &adk.ModelContext{})
	if err != nil {
		t.Fatalf("BeforeModelRewriteState 失败: %v", err)
	}
	if len(state.Messages) != 3 {
		t.Fatalf("应为 [S1, 外来指引, user] 3 条，实际: %d: %+v", len(state.Messages), state.Messages)
	}
	if state.Messages[0].Role != schema.System || !strings.Contains(state.Messages[0].Content, "你是 Leader。") {
		t.Errorf("S1 应插入到外来 system 消息之前，实际: %+v", state.Messages[0])
	}
	if !strings.Contains(state.Messages[1].Content, "外来 system 消息") {
		t.Errorf("外来指引应顺移到第二位，实际: %+v", state.Messages[1])
	}

	// 幂等：二次调用头部已是本 middleware 的 S1，不重复注入。
	_, state, err = mw.BeforeModelRewriteState(context.Background(), state, &adk.ModelContext{})
	if err != nil {
		t.Fatalf("二次 BeforeModelRewriteState 失败: %v", err)
	}
	if len(state.Messages) != 3 {
		t.Errorf("不应重复注入，实际消息数: %d", len(state.Messages))
	}
}
