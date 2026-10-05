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
	"net"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	modelopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// retryTestModel 是可按调用顺序返回错误的最小 Eino 模型，用于验证重试
// 次数和端点内重放边界，不依赖真实网络。
type retryTestModel struct {
	mu             sync.Mutex
	generateErrors []error
	streamErrors   []error
	generateCalls  int
	streamCalls    int
	lastInput      []*schema.Message
}

func (m *retryTestModel) Generate(_ context.Context, input []*schema.Message,
	_ ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	index := m.generateCalls
	m.generateCalls++
	m.lastInput = input
	if index < len(m.generateErrors) && m.generateErrors[index] != nil {
		return nil, m.generateErrors[index]
	}
	return schema.AssistantMessage("ok", nil), nil
}

func (m *retryTestModel) Stream(_ context.Context, input []*schema.Message,
	_ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	index := m.streamCalls
	m.streamCalls++
	m.lastInput = input
	if index < len(m.streamErrors) && m.streamErrors[index] != nil {
		return nil, m.streamErrors[index]
	}
	return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("ok", nil)}), nil
}

func TestSafeRetryModelDropsReasoningOnlyAssistantHistory(t *testing.T) {
	inner := &retryTestModel{}
	toolCall := schema.AssistantMessage("", []schema.ToolCall{{
		ID: "call-1", Type: "function",
		Function: schema.FunctionCall{Name: "map_query", Arguments: `{}`},
	}})
	_, err := WithSafeModelRetry(inner).Generate(context.Background(), []*schema.Message{
		schema.UserMessage("执行"),
		{Role: schema.Assistant, ReasoningContent: "不会进入下一轮历史"},
		toolCall,
		schema.ToolMessage("{}", "call-1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inner.lastInput) != 3 || inner.lastInput[1] != toolCall {
		t.Fatalf("只应移除非法空assistant，实际: %+v", inner.lastInput)
	}
}

// timeoutNetworkError 模拟尚未建立响应时的网络超时。
type timeoutNetworkError struct{}

func (timeoutNetworkError) Error() string   { return "network timeout" }
func (timeoutNetworkError) Timeout() bool   { return true }
func (timeoutNetworkError) Temporary() bool { return true }

var _ net.Error = timeoutNetworkError{}

// TestSafeRetryModelGenerate 验证 429/可恢复 5xx/网络超时只重试一次，
// 400 和 401 直接返回，不会造成无意义的第二次请求。
func TestSafeRetryModelGenerate(t *testing.T) {
	tests := []struct {
		name      string
		firstErr  error
		wantCalls int
		wantErr   bool
	}{
		{name: "限流", firstErr: &modelopenai.APIError{HTTPStatusCode: 429}, wantCalls: 2},
		{name: "服务不可用", firstErr: &modelopenai.APIError{HTTPStatusCode: 503}, wantCalls: 2},
		{name: "Claude 限流", firstErr: &anthropic.Error{StatusCode: 429}, wantCalls: 2},
		{name: "网络超时", firstErr: timeoutNetworkError{}, wantCalls: 2},
		{name: "参数错误", firstErr: &modelopenai.APIError{HTTPStatusCode: 400}, wantCalls: 1, wantErr: true},
		{name: "鉴权错误", firstErr: &modelopenai.APIError{HTTPStatusCode: 401}, wantCalls: 1, wantErr: true},
		{name: "能力未实现", firstErr: &modelopenai.APIError{HTTPStatusCode: 501}, wantCalls: 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := &retryTestModel{generateErrors: []error{tt.firstErr}}
			message, err := WithSafeModelRetry(inner).Generate(context.Background(),
				[]*schema.Message{schema.UserMessage("test")})
			if (err != nil) != tt.wantErr {
				t.Fatalf("错误状态不符: message=%+v err=%v", message, err)
			}
			if inner.generateCalls != tt.wantCalls {
				t.Fatalf("模型调用次数=%d，期望=%d", inner.generateCalls, tt.wantCalls)
			}
		})
	}
}

// TestSafeRetryModelOnlyOnce 验证第二次请求失败后直接返回，不形成无限重试。
func TestSafeRetryModelOnlyOnce(t *testing.T) {
	transient := &modelopenai.APIError{HTTPStatusCode: 503}
	inner := &retryTestModel{generateErrors: []error{transient, transient, transient}}
	_, err := WithSafeModelRetry(inner).Generate(context.Background(),
		[]*schema.Message{schema.UserMessage("test")})
	if err == nil || inner.generateCalls != 2 {
		t.Fatalf("应只调用两次并返回第二次错误: calls=%d err=%v", inner.generateCalls, err)
	}
}

// TestSafeRetryModelStream 验证仅建立流失败时重试；已成功建立的流不会由
// 包装器读取或重放，调用方仍可收到第一帧。
func TestSafeRetryModelStream(t *testing.T) {
	inner := &retryTestModel{streamErrors: []error{
		&modelopenai.APIError{HTTPStatusCode: 408}, nil,
	}}
	reader, err := WithSafeModelRetry(inner).Stream(context.Background(),
		[]*schema.Message{schema.UserMessage("test")})
	if err != nil {
		t.Fatalf("建立流重试失败: %v", err)
	}
	defer reader.Close()
	message, err := reader.Recv()
	if err != nil || message.Content != "ok" || inner.streamCalls != 2 {
		t.Fatalf("流结果不符: message=%+v err=%v calls=%d", message, err, inner.streamCalls)
	}
}

// TestSafeRetryModelHonorsCancellation 验证退避期间取消会立即结束且不发起
// 第二次请求，避免停止操作被重试逻辑拖延。
func TestSafeRetryModelHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	inner := &retryTestModel{generateErrors: []error{timeoutNetworkError{}}}
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := WithSafeModelRetry(inner).Generate(ctx,
		[]*schema.Message{schema.UserMessage("test")})
	if !errors.Is(err, context.Canceled) || inner.generateCalls != 1 {
		t.Fatalf("取消后不应重试: calls=%d err=%v", inner.generateCalls, err)
	}
}
