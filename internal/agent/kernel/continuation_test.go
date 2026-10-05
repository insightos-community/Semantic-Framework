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
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// stubContinuationModel 按 calls 序号回放预置响应/流帧，并记录每次收到的
// 输入历史，用于验证续写行为。
type stubContinuationModel struct {
	mu           sync.Mutex
	calls        int
	genResponses []*schema.Message
	streamFrames [][]*schema.Message
	inputs       [][]*schema.Message
}

func (s *stubContinuationModel) Generate(ctx context.Context, input []*schema.Message,
	opts ...model.Option) (*schema.Message, error) {
	s.mu.Lock()
	index := s.calls
	s.calls++
	s.inputs = append(s.inputs, input)
	s.mu.Unlock()
	if index >= len(s.genResponses) {
		return nil, errors.New("no more stub responses")
	}
	return s.genResponses[index], nil
}

func (s *stubContinuationModel) Stream(ctx context.Context, input []*schema.Message,
	opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	s.mu.Lock()
	index := s.calls
	s.calls++
	s.inputs = append(s.inputs, input)
	frames := s.streamFrames[index]
	s.mu.Unlock()

	reader, writer := schema.Pipe[*schema.Message](0)
	go func() {
		defer writer.Close()
		for _, frame := range frames {
			if writer.Send(frame, nil) {
				return
			}
		}
	}()
	return reader, nil
}

func chunk(content string) *schema.Message {
	return &schema.Message{Role: schema.Assistant, Content: content}
}

func finishChunk(content, finish string) *schema.Message {
	return &schema.Message{
		Role:         schema.Assistant,
		Content:      content,
		ResponseMeta: &schema.ResponseMeta{FinishReason: finish},
	}
}

func drainContinuationStream(t *testing.T, reader *schema.StreamReader[*schema.Message]) (string, string, int) {
	t.Helper()
	defer reader.Close()
	text := strings.Builder{}
	finish := ""
	frames := 0
	for {
		frame, err := reader.Recv()
		if errors.Is(err, io.EOF) {
			return text.String(), finish, frames
		}
		if err != nil {
			t.Fatalf("读取拼接流失败: %v", err)
		}
		if frame == nil {
			continue
		}
		frames++
		text.WriteString(frame.Content)
		if frame.ResponseMeta != nil && frame.ResponseMeta.FinishReason != "" {
			finish = frame.ResponseMeta.FinishReason
		}
	}
}

func TestStreamAutoContinue(t *testing.T) {
	stub := &stubContinuationModel{streamFrames: [][]*schema.Message{
		{chunk("第一"), finishChunk("段", "length")},
		{chunk("第二"), finishChunk("段收尾。", "stop")},
	}}
	wrapped := WithAutoContinuation(stub, "stub")

	text, finish, frames := drainContinuationStream(t, mustStream(t, wrapped))
	if text != "第一段第二段收尾。" {
		t.Fatalf("续写后的正文不符: %q", text)
	}
	if finish != "stop" || frames != 4 {
		t.Fatalf("finish=%q frames=%d，期望 stop/4", finish, frames)
	}
	if stub.calls != 2 {
		t.Fatalf("内部调用次数 = %d，期望 2", stub.calls)
	}
	continued := stub.inputs[1]
	if len(continued) != 3 {
		t.Fatalf("续写历史长度 = %d，期望原始 1 条 + 助手 + 继续", len(continued))
	}
	if continued[1].Role != schema.Assistant || continued[1].Content != "第一段" {
		t.Errorf("续写历史应含截断的助手正文，实际: %+v", continued[1])
	}
	if continued[2].Role != schema.User || continued[2].Content != continuationPrompt {
		t.Errorf("续写历史最后应为合成继续指令，实际: %+v", continued[2])
	}
}

func TestStreamPassthroughWithoutTruncation(t *testing.T) {
	stub := &stubContinuationModel{streamFrames: [][]*schema.Message{
		{chunk("正常"), finishChunk("完成。", "stop")},
	}}
	wrapped := WithAutoContinuation(stub, "stub")

	text, finish, _ := drainContinuationStream(t, mustStream(t, wrapped))
	if text != "正常完成。" || finish != "stop" {
		t.Fatalf("透传内容不符: %q finish=%q", text, finish)
	}
	if stub.calls != 1 {
		t.Fatalf("未截断不应二次调用，实际 %d 次", stub.calls)
	}
}

func TestStreamNoContinueWithToolCalls(t *testing.T) {
	toolFrame := &schema.Message{
		Role:      schema.Assistant,
		ToolCalls: []schema.ToolCall{{ID: "call-1", Function: schema.FunctionCall{Name: "system.time"}}},
	}
	stub := &stubContinuationModel{streamFrames: [][]*schema.Message{
		{toolFrame, finishChunk("", "length")},
	}}
	wrapped := WithAutoContinuation(stub, "stub")

	if _, _, _ = drainContinuationStream(t, mustStream(t, wrapped)); stub.calls != 1 {
		t.Fatalf("含工具调用的截断不应续写，实际内部调用 %d 次", stub.calls)
	}
}

func TestStreamThinkingBudgetErrorAssert(t *testing.T) {
	stub := &stubContinuationModel{streamFrames: [][]*schema.Message{
		{chunk(""), finishChunk("", "length")},
	}}
	wrapped := WithAutoContinuation(stub, "stub")

	reader, err := wrapped.Stream(context.Background(),
		[]*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatalf("建立流失败: %v", err)
	}
	var runErr error
	for {
		_, err = reader.Recv()
		if errors.Is(err, io.EOF) {
			t.Fatal("应收到续写预算错误而不是正常结束")
		}
		if err != nil {
			runErr = err
			break
		}
	}
	reader.Close()
	if runErr == nil || !strings.Contains(runErr.Error(), "options.max_tokens") {
		t.Fatalf("错误应包含可操作的调参指引，实际: %v", runErr)
	}
}

func TestStreamContinuationLimit(t *testing.T) {
	stub := &stubContinuationModel{streamFrames: [][]*schema.Message{
		{finishChunk("A", "length")},
		{finishChunk("B", "length")},
		{finishChunk("C", "length")}, // 超出续写上限后原样返回
	}}
	wrapped := WithAutoContinuation(stub, "stub")

	text, finish, _ := drainContinuationStream(t, mustStream(t, wrapped))
	if text != "ABC" {
		t.Fatalf("达到上限后的正文不符: %q", text)
	}
	if finish != "length" || stub.calls != 3 {
		t.Fatalf("finish=%q calls=%d，期望 length/3（1 次原始 + 2 次续写）", finish, stub.calls)
	}
}

func TestGenerateAutoContinue(t *testing.T) {
	stub := &stubContinuationModel{genResponses: []*schema.Message{
		finishChunk("第一段", "length"),
		finishChunk("第二段。", "stop"),
	}}
	wrapped := WithAutoContinuation(stub, "stub")

	msg, err := wrapped.Generate(context.Background(),
		[]*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatalf("Generate 不应失败: %v", err)
	}
	if msg.Content != "第二段。" || stub.calls != 2 {
		t.Fatalf("续写结果不符: content=%q calls=%d", msg.Content, stub.calls)
	}
}

func TestGenerateThinkingBudgetError(t *testing.T) {
	stub := &stubContinuationModel{genResponses: []*schema.Message{
		finishChunk("", "length"),
	}}
	wrapped := WithAutoContinuation(stub, "stub")

	_, err := wrapped.Generate(context.Background(),
		[]*schema.Message{schema.UserMessage("hi")})
	if err == nil || !strings.Contains(err.Error(), "options.max_tokens") {
		t.Fatalf("思考占满预算应返回可操作错误，实际: %v", err)
	}
}

func mustStream(t *testing.T, m model.BaseChatModel) *schema.StreamReader[*schema.Message] {
	t.Helper()
	reader, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatalf("建立流失败: %v", err)
	}
	return reader
}
