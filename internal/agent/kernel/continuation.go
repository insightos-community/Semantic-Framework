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
	"fmt"
	"io"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// 截断续写：生成型任务（无工具调用的一轮输出）超过 max_tokens 时，
// OpenAI 兼容服务以 finish_reason=length 结束流。内核在这里感知截断并
// 自动续写，让 max_tokens 回归"防失控护栏"的角色，而不是要求用户按
// 任务规模手工调大。
//
// 两种截断形态分开处理：
//   - 正文非空截断：把截断的助手消息与合成"继续"指令追加进历史后重发，
//     流式帧经 Pipe 拼接，下游（adk/trace/持久化）无感知；最多续写
//     maxModelContinuations 次，超出后返回已有内容（超长交付物的正确
//     路径是 Workflow 拆分，不是无限续写）。
//   - 思考占满预算（正文为空的截断）：续写无意义——推理模型会从头
//     再思考且 validModelHistory 会丢弃纯思考占位消息，因此转为明确
//     报错并给出可操作的调参指引，避免静默空回复。
//
// 含工具调用的截断响应不续写：截断的 tool_calls 参数无法安全执行，
// 交由上游既有逻辑处理。
const (
	maxModelContinuations = 2
	continuationPrompt    = "你上一条回复因长度上限被截断。请从截断处直接继续输出剩余内容，不要重复已有内容，也不要输出任何前言。"
)

// WithAutoContinuation 为模型增加截断感知与自动续写。见上方说明。
// mock 驱动不经过此包装（无截断语义）。
func WithAutoContinuation(inner model.BaseChatModel, name string) model.BaseChatModel {
	return &autoContinueModel{inner: inner, name: name}
}

type autoContinueModel struct {
	inner model.BaseChatModel
	name  string
}

func (m *autoContinueModel) Generate(ctx context.Context, input []*schema.Message,
	opts ...model.Option) (*schema.Message, error) {
	messages := input
	for attempt := 0; ; attempt++ {
		msg, err := m.inner.Generate(ctx, messages, opts...)
		if err != nil {
			return nil, err
		}
		if !isLengthTruncated(msg) || hasToolCalls(msg) || attempt >= maxModelContinuations {
			return msg, nil
		}
		if strings.TrimSpace(msg.Content) == "" {
			return nil, m.thinkingBudgetError()
		}
		messages = appendContinuation(messages, msg.Content)
	}
}

func (m *autoContinueModel) Stream(ctx context.Context, input []*schema.Message,
	opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	reader, err := m.inner.Stream(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	pipeReader, pipeWriter := schema.Pipe[*schema.Message](0)
	go m.pump(ctx, input, opts, reader, pipeWriter)
	return pipeReader, nil
}

// pump 消费当前调用的流并转发给下游；发现非空截断时追加续写历史并接续
// 下一次调用的流。所有权约定：被本函数耗尽或中途放弃的内部 reader 由
// 本函数 Close（恰好一次）；最终交给下游的 reader 沿用既有所有权，
// 由下游关闭。
func (m *autoContinueModel) pump(ctx context.Context, input []*schema.Message,
	opts []model.Option, reader *schema.StreamReader[*schema.Message],
	pw *schema.StreamWriter[*schema.Message]) {
	defer pw.Close()
	messages := input
	for attempt := 0; ; attempt++ {
		content := strings.Builder{}
		hasToolCalls := false
		finish := ""
		for {
			frame, err := reader.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				reader.Close()
				pw.Send(nil, err)
				return
			}
			if frame == nil {
				continue
			}
			if len(frame.ToolCalls) > 0 {
				hasToolCalls = true
			}
			content.WriteString(frame.Content)
			if frame.ResponseMeta != nil && frame.ResponseMeta.FinishReason != "" {
				finish = frame.ResponseMeta.FinishReason
			}
			if pw.Send(frame, nil) {
				// Send 返回 true 表示流已关闭（下游取消/超时）：
				// 释放当前内部流后退出。
				reader.Close()
				return
			}
		}
		reader.Close()
		if finish != "length" || hasToolCalls || attempt >= maxModelContinuations {
			return
		}
		if strings.TrimSpace(content.String()) == "" {
			pw.Send(nil, m.thinkingBudgetError())
			return
		}
		messages = appendContinuation(messages, content.String())
		next, err := m.inner.Stream(ctx, messages, opts...)
		if err != nil {
			pw.Send(nil, err)
			return
		}
		reader = next
	}
}

// appendContinuation 追加截断的助手消息（只保留正文，丢弃推理内容）与
// 合成续写指令；不修改调用方的历史切片。
func appendContinuation(input []*schema.Message, content string) []*schema.Message {
	messages := make([]*schema.Message, 0, len(input)+2)
	messages = append(messages, input...)
	messages = append(messages, schema.AssistantMessage(content, nil), schema.UserMessage(continuationPrompt))
	return messages
}

func (m *autoContinueModel) thinkingBudgetError() error {
	return fmt.Errorf(
		"模型端点 %q 在生成上限内只产出了思考、没有正文（finish_reason=length）：请调大该端点 options.max_tokens，或降低 reasoning_effort",
		m.name)
}

func isLengthTruncated(msg *schema.Message) bool {
	return msg != nil && msg.ResponseMeta != nil && msg.ResponseMeta.FinishReason == "length"
}

func hasToolCalls(msg *schema.Message) bool {
	return msg != nil && len(msg.ToolCalls) > 0
}
