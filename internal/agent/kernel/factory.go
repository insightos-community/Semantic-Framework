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
	"fmt"
	"time"

	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"

	"insightos.cn/semantic-framework/pkg/llm"
)

// componentOpenAI / componentClaude / componentMock 是当前支持的模型驱动名。
// 其他驱动（gemini/ollama/ark）在实现前不开放，NewChatModel 遇到未知
// 驱动直接报错，避免配置成功后到首轮对话才静默降级。
const (
	componentOpenAI = "openai"
	componentClaude = "claude"
	componentMock   = "mock"

	// 模型服务偶发不返回时，不能永久占住同一 Task Context 和 Robot 资源。
	// 这是单次 HTTP 请求边界，不是新的 RunBudget；端点可用 options.timeout_seconds 覆盖。
	defaultModelRequestTimeout = DefaultModelRequestTimeoutSeconds * time.Second

	// 消费侧兜底：存量端点（手改配置或物化机制上线前创建）没有 max_tokens 时，
	// OpenAI 兼容服务（如 llama.cpp 的 n_predict）可能无限生成。端点
	// options.max_tokens 仍可覆盖该默认。
	defaultMaxOutputTokens = DefaultMaxOutputTokens

	// DefaultModelRequestTimeoutSeconds / DefaultMaxOutputTokens 是 settings
	// API 为新增 OpenAI 兼容端点物化默认 options 时使用的值（timeout_seconds /
	// max_tokens），与上面的消费侧兜底同源，避免两处默认值漂移。
	DefaultModelRequestTimeoutSeconds = 300
	// layout001 的真实 DeepSeek 规划已观测到单次调用需要约 9.3k completion
	// tokens；8192 会在工具调用正文出现前截断纯思考。16384 仍是明确的生成
	// 护栏，同时为包含地图查询和多 Task 规划的正常请求保留足够输出空间。
	DefaultMaxOutputTokens = 16384

	// ComponentOpenAI 是 openai 驱动的导出名：bootstrap 物化新端点默认
	// options 时按驱动分支，与消费侧同源引用而不是写字面量。
	ComponentOpenAI = componentOpenAI
)

// RuntimeDefaults 是设置页展示用的运行时默认值，不写入配置树。
// 存量端点 options 为空时消费侧仍使用这些值；前端不得再写死 8192。
func RuntimeDefaults() map[string]any {
	return map[string]any{
		"openai": map[string]any{
			"timeout_seconds": DefaultModelRequestTimeoutSeconds,
			"max_tokens":      DefaultMaxOutputTokens,
		},
		"claude": map[string]any{
			"max_tokens": DefaultMaxOutputTokens,
		},
	}
}

// Model 是 kernel 对外的模型句柄类型（eino BaseChatModel 的别名）。
// 为什么用别名：调用方（runtime/bootstrap）以 kernel.Model 引用即可，
// 无需 import eino——ACL 边界（eino 只出现在 kernel）在 import 层面保持干净。
type Model = model.BaseChatModel

// Tool 是 kernel 对外的工具句柄类型（eino tool.BaseTool 的别名，同理）。
type Tool = tool.BaseTool

// NewChatModel 按注册表条目构建内核模型。apiKey 由调用方从
// llm.Registry.APIKey 取得（密钥只经环境变量注入，不进配置结构）；
// mock 驱动忽略 apiKey。构建只建立客户端，不发起真实请求。
func NewChatModel(ctx context.Context, entry llm.Provider, apiKey string) (model.BaseChatModel, error) {
	switch entry.Component {
	case componentOpenAI:
		return newOpenAIModel(ctx, entry, apiKey)
	case componentClaude:
		return newClaudeModel(ctx, entry, apiKey)
	case componentMock:
		return NewMockChatModel(), nil
	default:
		return nil, fmt.Errorf("模型端点 %q 的驱动 %q 未知：当前支持 %q / %q / %q",
			entry.Name, entry.Component, componentOpenAI, componentClaude, componentMock)
	}
}

// RequiresAPIKey 返回指定驱动是否需要 API key 才能工作。
// bootstrap 与 doctor 用它做启动前 key 检查（mock 无需密钥）。
func RequiresAPIKey(component string) bool {
	return component == componentOpenAI || component == componentClaude
}

// KnownComponent 返回 component 是否为内核已支持的模型驱动名。
// 配置热重载与 doctor 用它做 fail-closed 校验：未知驱动在启动/重载时
// 直接报出，而不是等到第一次调用才失败。
func KnownComponent(component string) bool {
	switch component {
	case componentOpenAI, componentClaude, componentMock:
		return true
	}
	return false
}

// newClaudeModel 使用 Eino Claude 原生组件接入 Anthropic Messages API。
// BaseURL 为空时沿用官方 SDK 默认地址；非空时用于代理或测试端点。
func newClaudeModel(ctx context.Context, entry llm.Provider, apiKey string) (model.BaseChatModel, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("模型端点 %q 缺少 API key：请设置环境变量 %s",
			entry.Name, llm.APIKeyEnv(entry.Name))
	}
	if entry.Model == "" {
		return nil, fmt.Errorf("模型端点 %q 缺少 model", entry.Name)
	}

	cfg := &claude.Config{APIKey: apiKey, Model: entry.Model, MaxTokens: 4096}
	if entry.BaseURL != "" {
		baseURL := entry.BaseURL
		cfg.BaseURL = &baseURL
	}
	if err := applyClaudeOptions(cfg, entry); err != nil {
		return nil, err
	}
	chatModel, err := claude.NewChatModel(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("构建模型端点 %q 的 Claude 模型失败: %w", entry.Name, err)
	}
	return WithAutoContinuation(chatModel, entry.Name), nil
}

// applyClaudeOptions 只接收 Eino Claude 原生组件已经明确支持的通用参数。
// reasoning_effort 不是 Anthropic 的等价接口，不能在这里做含义不准确的映射。
func applyClaudeOptions(cfg *claude.Config, entry llm.Provider) error {
	for key, value := range entry.Options {
		switch key {
		case "temperature":
			v, err := toFloat64(entry.Name, key, value)
			if err != nil {
				return err
			}
			temperature := float32(v)
			cfg.Temperature = &temperature
		case "max_tokens":
			v, err := toInt(entry.Name, key, value)
			if err != nil {
				return err
			}
			if v <= 0 {
				return fmt.Errorf("模型端点 %q 的 options.%s 必须大于 0", entry.Name, key)
			}
			cfg.MaxTokens = v
		default:
			return fmt.Errorf("模型端点 %q 的 Claude options 含未识别键 %q", entry.Name, key)
		}
	}
	return nil
}

// newOpenAIModel 构建 OpenAI 兼容端点的模型，options 映射到客户端配置。
func newOpenAIModel(ctx context.Context, entry llm.Provider, apiKey string) (model.BaseChatModel, error) {
	if entry.BaseURL == "" {
		return nil, fmt.Errorf("模型端点 %q 缺少 base_url", entry.Name)
	}
	if apiKey == "" {
		return nil, fmt.Errorf("模型端点 %q 缺少 API key：请设置环境变量 %s",
			entry.Name, llm.APIKeyEnv(entry.Name))
	}

	cfg := &openai.ChatModelConfig{
		APIKey:  apiKey,
		BaseURL: entry.BaseURL,
		Model:   entry.Model,
		Timeout: defaultModelRequestTimeout,
	}
	if err := applyOptions(cfg, entry); err != nil {
		return nil, err
	}
	if cfg.MaxTokens == nil {
		maxTokens := defaultMaxOutputTokens
		cfg.MaxTokens = &maxTokens
	}

	cm, err := openai.NewChatModel(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("构建模型端点 %q 的内核模型失败: %w", entry.Name, err)
	}
	return WithAutoContinuation(cm, entry.Name), nil
}

// applyOptions 把端点的 options（yaml 解析出的标量）映射到客户端配置。
// 未识别的键直接报错：静默忽略会掩盖配置拼写错误（配置必有消费方）。
func applyOptions(cfg *openai.ChatModelConfig, entry llm.Provider) error {
	for key, value := range entry.Options {
		switch key {
		case "temperature":
			v, err := toFloat64(entry.Name, key, value)
			if err != nil {
				return err
			}
			f := float32(v)
			cfg.Temperature = &f
		case "max_tokens":
			v, err := toInt(entry.Name, key, value)
			if err != nil {
				return err
			}
			cfg.MaxTokens = &v
		case "reasoning_effort":
			v, ok := value.(string)
			if !ok || (v != "low" && v != "medium" && v != "high") {
				return fmt.Errorf("模型端点 %q 的 options.%s 应为 low/medium/high，实际: %v", entry.Name, key, value)
			}
			cfg.ReasoningEffort = openai.ReasoningEffortLevel(v)
		case "timeout_seconds":
			v, err := toInt(entry.Name, key, value)
			if err != nil {
				return err
			}
			if v <= 0 {
				return fmt.Errorf("模型端点 %q 的 options.%s 必须大于 0", entry.Name, key)
			}
			cfg.Timeout = time.Duration(v) * time.Second
		default:
			return fmt.Errorf("模型端点 %q 的 options 含未识别键 %q", entry.Name, key)
		}
	}
	return nil
}

// ValidateOptions 校验端点 options 的键与值是否被对应驱动支持。
// settings API 在保存配置时调用它做预校验，把非法键提前到保存时 400，
// 而不是等到第一次会话构建模型才失败。mock 驱动不消费 options，不校验。
func ValidateOptions(component, name string, options map[string]any) error {
	switch component {
	case componentOpenAI:
		return applyOptions(&openai.ChatModelConfig{}, llm.Provider{Name: name, Component: component, Options: options})
	case componentClaude:
		return applyClaudeOptions(&claude.Config{}, llm.Provider{Name: name, Component: component, Options: options})
	default:
		return nil
	}
}

// toFloat64 把 yaml 标量（int/float64）转换为 float64。
func toFloat64(provider, key string, value any) (float64, error) {
	switch v := value.(type) {
	case float64:
		return v, nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	default:
		return 0, fmt.Errorf("模型端点 %q 的 options.%s 应为数值，实际: %T", provider, key, value)
	}
}

// toInt 把 yaml 标量（int/float64）转换为 int。
func toInt(provider, key string, value any) (int, error) {
	switch v := value.(type) {
	case int:
		return v, nil
	case int64:
		return int(v), nil
	case float64:
		if v == float64(int(v)) {
			return int(v), nil
		}
		return 0, fmt.Errorf("模型端点 %q 的 options.%s 应为整数，实际: %v", provider, key, v)
	default:
		return 0, fmt.Errorf("模型端点 %q 的 options.%s 应为整数，实际: %T", provider, key, value)
	}
}
