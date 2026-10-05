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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/pkg/llm"
)

// testOpenAIEntry 返回一个 openai 驱动的测试端点。
func testOpenAIEntry() llm.Provider {
	return llm.Provider{
		Name:         "deepseek-chat",
		Component:    componentOpenAI,
		BaseURL:      "https://api.deepseek.com/v1",
		Model:        "deepseek-chat",
		Capabilities: []string{"text", "tool_call"},
		Options:      map[string]any{"temperature": 0.7, "max_tokens": 4096},
		Price:        llm.Price{Prompt: 0.001, Completion: 0.002},
	}
}

// TestNewChatModelOpenAI 验证 openai 驱动的模型构建：只建立客户端，不发起请求。
func TestNewChatModelOpenAI(t *testing.T) {
	m, err := NewChatModel(context.Background(), testOpenAIEntry(), "sk-test")
	if err != nil {
		t.Fatalf("NewChatModel(openai) 不应返回错误: %v", err)
	}
	if m == nil {
		t.Fatal("NewChatModel(openai) 应返回非空模型")
	}
}

// TestOpenAIImageRequestOmitsDetail 验证通用图片消息经过 Eino 与底层
// OpenAI SDK 转换后，最终 HTTP JSON 仍省略可选 detail 字段。MiniMax 等
// 兼容端点会拒绝 detail:auto，因此不能只验证 kernel 的中间结构。
func TestOpenAIImageRequestOmitsDetail(t *testing.T) {
	bodyCh := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		bodyCh <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"MiniMax-M3","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	model, err := NewChatModel(context.Background(), llm.Provider{
		Name: "MiniMax-M3", Component: componentOpenAI,
		BaseURL: server.URL + "/v1", Model: "MiniMax-M3",
	}, "sk-test")
	if err != nil {
		t.Fatalf("构建本地兼容模型失败: %v", err)
	}
	imageData := "aW1hZ2U="
	message := &schema.Message{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
		{Type: schema.ChatMessagePartTypeText, Text: "分析图片"},
		{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
			MessagePartCommon: schema.MessagePartCommon{Base64Data: &imageData, MIMEType: "image/png"},
		}},
	}}
	if _, err := model.Generate(context.Background(), []*schema.Message{message}); err != nil {
		t.Fatalf("本地兼容请求失败: %v", err)
	}

	body := <-bodyCh
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("请求缺少 messages: %+v", body)
	}
	first, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatalf("首条消息结构非法: %+v", messages[0])
	}
	content, ok := first["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("图片消息 content 结构非法: %+v", first["content"])
	}
	imagePart, ok := content[1].(map[string]any)
	if !ok {
		t.Fatalf("图片 part 结构非法: %+v", content[1])
	}
	imageURL, ok := imagePart["image_url"].(map[string]any)
	if !ok {
		t.Fatalf("图片 part 缺少 image_url: %+v", imagePart)
	}
	if _, exists := imageURL["detail"]; exists {
		t.Fatalf("最终 HTTP 请求不应包含 image_url.detail: %+v", imageURL)
	}
}

// TestNewChatModelOpenAIMissingKey 验证缺少 API key 时报错并提示环境变量名。
func TestNewChatModelOpenAIMissingKey(t *testing.T) {
	_, err := NewChatModel(context.Background(), testOpenAIEntry(), "")
	if err == nil {
		t.Fatal("缺少 API key 应返回错误")
	}
	want := "SEMANTIC_LLM_API_KEY_DEEPSEEK_CHAT"
	if got := err.Error(); !strings.Contains(got, want) {
		t.Errorf("错误信息应提示环境变量 %s，实际: %s", want, got)
	}
}

// TestNewChatModelOpenAIMissingBaseURL 验证缺少 base_url 时报错。
func TestNewChatModelOpenAIMissingBaseURL(t *testing.T) {
	entry := testOpenAIEntry()
	entry.BaseURL = ""
	if _, err := NewChatModel(context.Background(), entry, "sk-test"); err == nil {
		t.Fatal("缺少 base_url 应返回错误")
	}
}

// TestNewChatModelMock 验证 mock 驱动返回 MockChatModel。
func TestNewChatModelMock(t *testing.T) {
	m, err := NewChatModel(context.Background(), llm.Provider{
		Name: "mock", Component: componentMock, Model: "mock",
	}, "")
	if err != nil {
		t.Fatalf("NewChatModel(mock) 不应返回错误: %v", err)
	}
	if _, ok := m.(*MockChatModel); !ok {
		t.Errorf("mock 驱动应返回 *MockChatModel，实际: %T", m)
	}
}

// TestNewChatModelClaude 验证 Claude 端点使用 Eino 原生组件构建，而不是
// 继续伪装成 OpenAI 兼容端点。
func TestNewChatModelClaude(t *testing.T) {
	chatModel, err := NewChatModel(context.Background(), llm.Provider{
		Name: "claude-sonnet", Component: componentClaude,
		BaseURL: "https://api.anthropic.com", Model: "claude-sonnet-test",
		Options: map[string]any{"max_tokens": 2048, "temperature": 0.2},
	}, "sk-ant-test")
	if err != nil {
		t.Fatalf("NewChatModel(claude) 不应返回错误: %v", err)
	}
	if _, ok := chatModel.(*autoContinueModel); !ok {
		t.Fatalf("claude 驱动应返回带续写包装的模型，实际: %T", chatModel)
	}
}

// TestNewChatModelClaudeRejectsReasoningEffort 验证不把 OpenAI 的推理档位
// 强行映射为 Claude thinking，防止配置含义被静默改变。
func TestNewChatModelClaudeRejectsReasoningEffort(t *testing.T) {
	_, err := NewChatModel(context.Background(), llm.Provider{
		Name: "claude-sonnet", Component: componentClaude, Model: "claude-sonnet-test",
		Options: map[string]any{"reasoning_effort": "high"},
	}, "sk-ant-test")
	if err == nil || !strings.Contains(err.Error(), "未识别键") {
		t.Fatalf("Claude 不应接受 reasoning_effort: %v", err)
	}
}

// TestNewChatModelUnknownComponent 验证未知驱动报错。
func TestNewChatModelUnknownComponent(t *testing.T) {
	if _, err := NewChatModel(context.Background(), llm.Provider{
		Name: "x", Component: "gemini", Model: "gemini-test",
	}, "sk-test"); err == nil {
		t.Fatal("未知驱动应返回错误")
	}
}

// TestApplyOpenAIRequestTimeout 验证模型请求有默认边界，并允许端点按实际延迟覆盖。
func TestApplyOpenAIRequestTimeout(t *testing.T) {
	cfg := &openai.ChatModelConfig{Timeout: defaultModelRequestTimeout}
	entry := testOpenAIEntry()
	entry.Options = map[string]any{"timeout_seconds": 75}
	if err := applyOptions(cfg, entry); err != nil {
		t.Fatalf("设置模型请求超时失败: %v", err)
	}
	if cfg.Timeout != 75*time.Second {
		t.Fatalf("模型请求超时 = %s，期望 75s", cfg.Timeout)
	}
}

// TestNewChatModelUnknownOption 验证 options 含未识别键时报错（不静默忽略）。
func TestNewChatModelUnknownOption(t *testing.T) {
	entry := testOpenAIEntry()
	entry.Options = map[string]any{"temperatur": 0.7} // 拼写错误
	if _, err := NewChatModel(context.Background(), entry, "sk-test"); err == nil {
		t.Fatal("未识别的 options 键应返回错误")
	}
}

// TestNewChatModelBadOptionType 验证 options 值类型非法时报错。
func TestNewChatModelBadOptionType(t *testing.T) {
	entry := testOpenAIEntry()
	entry.Options = map[string]any{"temperature": "hot"}
	if _, err := NewChatModel(context.Background(), entry, "sk-test"); err == nil {
		t.Fatal("temperature 为字符串应返回错误")
	}

	entry.Options = map[string]any{"max_tokens": 4096.5}
	if _, err := NewChatModel(context.Background(), entry, "sk-test"); err == nil {
		t.Fatal("max_tokens 为非整数应返回错误")
	}

	// yaml 整数值也可能解析为 float64，整数 float 应被接受。
	entry.Options = map[string]any{"max_tokens": float64(4096)}
	if _, err := NewChatModel(context.Background(), entry, "sk-test"); err != nil {
		t.Errorf("max_tokens 为整数 float64 不应报错: %v", err)
	}
}

// TestOpenAIMaxTokensFallback 验证存量端点未配置 max_tokens 时，消费侧
// 兜底 defaultMaxOutputTokens 生效——OpenAI 兼容服务（如 llama.cpp）在
// 请求不带 max_tokens 时可能无限生成。
func TestOpenAIMaxTokensFallback(t *testing.T) {
	bodyCh := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		bodyCh <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	model, err := NewChatModel(context.Background(), llm.Provider{
		Name: "local", Component: componentOpenAI,
		BaseURL: server.URL + "/v1", Model: "m",
	}, "sk-test")
	if err != nil {
		t.Fatalf("构建模型失败: %v", err)
	}
	message := &schema.Message{Role: schema.User, Content: "hi"}
	if _, err := model.Generate(context.Background(), []*schema.Message{message}); err != nil {
		t.Fatalf("请求失败: %v", err)
	}

	body := <-bodyCh
	if got, ok := body["max_tokens"].(float64); !ok || int(got) != defaultMaxOutputTokens {
		t.Fatalf("请求 max_tokens = %v，期望兜底 %d", body["max_tokens"], defaultMaxOutputTokens)
	}
}

// TestOpenAIMaxTokensExplicitOverridesFallback 验证端点显式配置的
// max_tokens 优先于消费侧兜底。
func TestOpenAIMaxTokensExplicitOverridesFallback(t *testing.T) {
	bodyCh := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		bodyCh <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	model, err := NewChatModel(context.Background(), llm.Provider{
		Name: "local", Component: componentOpenAI,
		BaseURL: server.URL + "/v1", Model: "m",
		Options: map[string]any{"max_tokens": 1234},
	}, "sk-test")
	if err != nil {
		t.Fatalf("构建模型失败: %v", err)
	}
	message := &schema.Message{Role: schema.User, Content: "hi"}
	if _, err := model.Generate(context.Background(), []*schema.Message{message}); err != nil {
		t.Fatalf("请求失败: %v", err)
	}

	body := <-bodyCh
	if got, ok := body["max_tokens"].(float64); !ok || int(got) != 1234 {
		t.Fatalf("请求 max_tokens = %v，期望显式值 1234", body["max_tokens"])
	}
}

// TestValidateOptions 验证 settings 保存时的 options 预校验：合法键通过，
// 未识别键/非法值报错，claude 驱动独立校验，mock 不校验。
func TestValidateOptions(t *testing.T) {
	if err := ValidateOptions(componentOpenAI, "ep", map[string]any{
		"timeout_seconds": 300, "max_tokens": 8192, "temperature": 0.7, "reasoning_effort": "low",
	}); err != nil {
		t.Fatalf("合法 openai options 不应报错: %v", err)
	}
	if err := ValidateOptions(componentOpenAI, "ep", map[string]any{"foo": 1}); err == nil {
		t.Fatal("openai 未识别键应报错")
	}
	if err := ValidateOptions(componentOpenAI, "ep", map[string]any{"timeout_seconds": 0}); err == nil {
		t.Fatal("timeout_seconds 为 0 应报错")
	}
	if err := ValidateOptions(componentClaude, "ep", map[string]any{"timeout_seconds": 300}); err == nil {
		t.Fatal("claude 不支持 timeout_seconds，应报错")
	}
	if err := ValidateOptions(componentClaude, "ep", map[string]any{"max_tokens": 4096}); err != nil {
		t.Fatalf("claude 合法 options 不应报错: %v", err)
	}
	if err := ValidateOptions(componentMock, "ep", map[string]any{"anything": 1}); err != nil {
		t.Fatalf("mock 不消费 options，不应报错: %v", err)
	}
}

func TestRuntimeDefaults(t *testing.T) {
	defaults := RuntimeDefaults()
	openai := defaults["openai"].(map[string]any)
	if openai["timeout_seconds"] != DefaultModelRequestTimeoutSeconds ||
		openai["max_tokens"] != DefaultMaxOutputTokens {
		t.Fatalf("openai 运行时默认值不符: %v", openai)
	}
	claude := defaults["claude"].(map[string]any)
	if _, ok := claude["timeout_seconds"]; ok {
		t.Fatalf("claude 不应包含 timeout_seconds: %v", claude)
	}
	if claude["max_tokens"] != DefaultMaxOutputTokens {
		t.Fatalf("claude max_tokens 不符: %v", claude)
	}
}
