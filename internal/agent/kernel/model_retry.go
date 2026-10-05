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
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	modelopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

const modelRetryDelay = 250 * time.Millisecond

// WithSafeModelRetry 为 Eino ChatModel 增加一次保守的同端点重试。
//
// 包装器不会选择其他端点，也不会在流已经建立后重放请求：后一种情况无法
// 证明 Provider 没有输出，自动重试可能造成重复工具调用或重复外部副作用。
func WithSafeModelRetry(inner model.BaseChatModel) model.BaseChatModel {
	if inner == nil {
		return nil
	}
	if _, ok := inner.(*safeRetryModel); ok {
		return inner
	}
	return &safeRetryModel{inner: inner}
}

// safeRetryModel 只包装模型请求入口。工具调用仍由 Eino ToolsNode 执行，
// 因而这里不会绕过工具安全门禁，也不会形成跨模型回退。
type safeRetryModel struct {
	inner model.BaseChatModel
}

// GetType 保留底层模型的 Eino 组件标识，避免重试包装影响 Callback/Trace
// 对 Provider 组件类型的归因。
func (m *safeRetryModel) GetType() string {
	if typer, ok := m.inner.(components.Typer); ok {
		return typer.GetType()
	}
	return "unknown"
}

// IsCallbacksEnabled 透传底层模型的回调能力。底层已自行触发回调时，
// Eino 不应再为 safeRetryModel 包装一次；底层未实现时仍由 Eino 自动观测。
func (m *safeRetryModel) IsCallbacksEnabled() bool {
	return components.IsCallbacksEnabled(m.inner)
}

// Generate 在首次请求没有返回消息且错误可恢复时，仅重试同一模型一次。
func (m *safeRetryModel) Generate(ctx context.Context, input []*schema.Message,
	opts ...model.Option) (*schema.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	input = validModelHistory(input)
	message, err := m.inner.Generate(ctx, input, opts...)
	if err == nil || !isRetryableModelError(ctx, err) {
		return message, err
	}
	if err := waitModelRetry(ctx); err != nil {
		return nil, err
	}
	return m.inner.Generate(ctx, input, opts...)
}

// Stream 只重试“建立流失败”的情况。流一旦成功返回，后续即使在第一帧
// 前报错也交给调用方处理，因为通用接口无法证明 Provider 侧尚未生成输出。
func (m *safeRetryModel) Stream(ctx context.Context, input []*schema.Message,
	opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	input = validModelHistory(input)
	reader, err := m.inner.Stream(ctx, input, opts...)
	if err == nil || !isRetryableModelError(ctx, err) {
		return reader, err
	}
	if err := waitModelRetry(ctx); err != nil {
		return nil, err
	}
	return m.inner.Stream(ctx, input, opts...)
}

// validModelHistory 丢弃没有可见内容、工具调用或多模态输出的 assistant
// 占位消息。部分推理模型会在工具失败后留下只含 ReasoningContent 的中间消息，
// OpenAI 兼容接口不接受它作为历史；隐藏推理也不属于可恢复业务上下文。
func validModelHistory(input []*schema.Message) []*schema.Message {
	for index, message := range input {
		if message == nil || message.Role != schema.Assistant || message.Content != "" ||
			len(message.ToolCalls) != 0 || len(message.MultiContent) != 0 ||
			len(message.AssistantGenMultiContent) != 0 {
			continue
		}
		result := make([]*schema.Message, 0, len(input)-1)
		result = append(result, input[:index]...)
		for _, remaining := range input[index+1:] {
			if remaining != nil && remaining.Role == schema.Assistant && remaining.Content == "" &&
				len(remaining.ToolCalls) == 0 && len(remaining.MultiContent) == 0 &&
				len(remaining.AssistantGenMultiContent) == 0 {
				continue
			}
			result = append(result, remaining)
		}
		return result
	}
	return input
}

// waitModelRetry 提供很短的退避，并让用户停止或请求超时可以立即终止等待。
func waitModelRetry(ctx context.Context) error {
	timer := time.NewTimer(modelRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// isRetryableModelError 采用明确白名单，避免把鉴权、参数或能力不匹配错误
// 重试成第二次无意义请求。当前原生识别 OpenAI 兼容适配器的结构化错误；
// 未来新增 Provider 时应在此补充其结构化错误类型，而不是解析错误字符串。
func isRetryableModelError(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var apiErr *modelopenai.APIError
	if errors.As(err, &apiErr) {
		return retryableHTTPStatus(apiErr.HTTPStatusCode)
	}
	var anthropicErr *anthropic.Error
	if errors.As(err, &anthropicErr) {
		return retryableHTTPStatus(anthropicErr.StatusCode)
	}

	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// retryableHTTPStatus 只接纳请求超时、限流和明确可恢复的服务端错误。
// 501（未实现）和 505（协议不支持）属于能力或协议问题，不应重试。
func retryableHTTPStatus(status int) bool {
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
		return true
	}
	return status >= 500 && status <= 599 &&
		status != http.StatusNotImplemented && status != http.StatusHTTPVersionNotSupported
}
