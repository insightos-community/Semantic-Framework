package integration

// 本文件是 bootstrap ↔ kernel 接线的黑盒验证：作为独立测试入口直接使用
// 内核类型（eino schema/adk/callbacks）断言端到端行为。生产代码的 eino
// 依赖仍收敛在 internal/agent/kernel（ACL 纪律），本文件不参与生产依赖图。

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/bootstrap"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/log"
)

// wireLLMApp 按测试配置装配应用（不启动监听），测试结束关闭 store。
func wireLLMApp(t *testing.T) *bootstrap.App {
	t.Helper()
	t.Setenv("SEMANTIC_ADMIN_PASSWORD", "test-admin-pass")

	cfg := config.Default()
	cfg.Store.SQLitePath = filepath.Join(t.TempDir(), "test.db")
	storetest.SeedMigratedAt(t, cfg.Store.SQLitePath)
	cfg.LLM.Default = "mock" // 无 key 环境：默认模型走 mock 驱动
	cfg.Agents.ProfilesDir = profilesDirAbs(t)
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})

	app, err := bootstrap.Wire(cfg, logger)
	if err != nil {
		t.Fatalf("Wire 装配失败: %v", err)
	}
	t.Cleanup(func() { _ = app.Store().Close() })
	return app
}

// weatherInput 是天气查询工具的入参。
type weatherInput struct {
	// City 城市名。
	City string `json:"city" jsonschema:"description=城市名"`
}

// TestLLMKernelAgentRound 验证 bootstrap 装配后的完整 agent 链路：
// 注册表取 mock 端点 → factory 构建模型 → BuildAgent 编排 →
// 一轮含工具调用的对话 → trace_spans 与 metering 落库。
func TestLLMKernelAgentRound(t *testing.T) {
	app := wireLLMApp(t)
	ctx := context.Background()

	// 经装配好的注册表取得 mock 端点并构建模型。
	reg := app.LLMRegistry()
	entry, err := reg.Get("mock")
	if err != nil {
		t.Fatalf("注册表应包含 mock 端点: %v", err)
	}
	chatModel, err := kernel.NewChatModel(ctx, entry, reg.APIKey("mock"))
	if err != nil {
		t.Fatalf("构建 mock 模型失败: %v", err)
	}
	mock, ok := chatModel.(*kernel.MockChatModel)
	if !ok {
		t.Fatalf("mock 端点应构建出 *MockChatModel，实际: %T", chatModel)
	}

	// 脚本：第一轮要求调 get_weather，第二轮返回总结文本。
	mock.SetScript(
		kernel.MockReply{ToolCalls: []schema.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: schema.FunctionCall{
				Name:      "get_weather",
				Arguments: `{"city":"北京"}`,
			},
		}}},
		kernel.MockReply{Content: "北京今天晴，25°C。"},
	)

	var toolCalls int32
	weatherTool, err := utils.InferTool("get_weather", "查询城市天气",
		func(_ context.Context, in weatherInput) (string, error) {
			atomic.AddInt32(&toolCalls, 1)
			return "晴，25°C", nil
		})
	if err != nil {
		t.Fatalf("构建工具失败: %v", err)
	}

	// 新版 BuildAgent 在 Store 非空时内部创建 TraceHandler 挂接观测，
	// 调用方无需了解 eino 回调机制（见 kernel/agent.go）。
	runner, err := kernel.BuildAgent(ctx, kernel.AgentConfig{
		Name:        "it-agent",
		Instruction: "你是天气助手，回答前必须查询天气。",
		Model:       mock,
		ModelName:   entry.Model,
		Price:       entry.Price,
		Purpose:     "chat",
		Tools:       []tool.BaseTool{weatherTool},
		Store:       app.Store(),
		Logger:      log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
	})
	if err != nil {
		t.Fatalf("BuildAgent 失败: %v", err)
	}

	// 跑一轮对话，消费 kernel 事件流并拼接文本增量。
	es, err := runner.Run(ctx, nil, "北京天气怎么样？")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	var sb strings.Builder
	for {
		ev, ok := es.Next()
		if !ok {
			break
		}
		if ev.Kind == kernel.EventError {
			t.Fatalf("agent 运行出错: %v", ev.Err)
		}
		if ev.Kind == kernel.EventTextDelta {
			sb.WriteString(ev.Text)
		}
	}
	finalText := sb.String()

	if got := atomic.LoadInt32(&toolCalls); got != 1 {
		t.Errorf("工具应被执行 1 次，实际: %d", got)
	}
	if finalText != "北京今天晴，25°C。" {
		t.Errorf("响应文本不符: %q", finalText)
	}

	// trace_spans 与 metering 均落库（流式回调在 goroutine 中消费，轮询等待）。
	waitForCondition(t, 5*time.Second, func() bool {
		spans, err := app.Store().QuerySpans(es.TraceID())
		return err == nil && len(spans) == 4
	}, "trace_spans 应有 Agent 根、两条模型跨度和一条工具跨度")
	waitForCondition(t, 5*time.Second, func() bool {
		records, err := app.Store().QueryMetering(store.MeteringFilter{Model: "mock"})
		return err == nil && len(records) == 2
	}, "metering 应有 2 条计量记录")
}

// TestDeepSeekSmoke 真模型联调：deepseek-chat 流式 generate 一句话自我介绍，
// 断言响应非空且 metering 记录 usage > 0。
// 仅当 SEMANTIC_LLM_API_KEY_DEEPSEEK_CHAT 存在时运行（CI 无 key 自动跳过）。
func TestDeepSeekSmoke(t *testing.T) {
	if os.Getenv("SEMANTIC_LLM_API_KEY_DEEPSEEK_CHAT") == "" {
		t.Skip("未设置 SEMANTIC_LLM_API_KEY_DEEPSEEK_CHAT，跳过真模型联调")
	}
	app := wireLLMApp(t)

	reg := app.LLMRegistry()
	entry, err := reg.Get("deepseek-chat")
	if err != nil {
		t.Fatalf("注册表应包含 deepseek-chat 端点: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	chatModel, err := kernel.NewChatModel(ctx, entry, reg.APIKey("deepseek-chat"))
	if err != nil {
		t.Fatalf("构建 deepseek-chat 模型失败: %v", err)
	}

	// 真实模型经 ctx 回调管理器触发 TraceHandler（与 agent 内挂接路径
	// 不同，这里直接验证 eino-ext 模型自身的回调触发）。
	handler := kernel.NewTraceHandler(app.Store(),
		log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
		kernel.TraceOptions{
			Agent:   "deepseek-smoke",
			Purpose: "smoke",
			Model:   entry.Model,
			Price:   entry.Price,
		})
	ctx = callbacks.InitCallbacks(ctx, &callbacks.RunInfo{
		Name:      "deepseek-chat",
		Component: components.ComponentOfChatModel,
	}, handler)

	sr, err := chatModel.Stream(ctx, []*schema.Message{schema.UserMessage("用一句话介绍你自己")})
	if err != nil {
		t.Fatalf("流式调用失败: %v", err)
	}
	defer sr.Close()

	var sb strings.Builder
	for {
		frame, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("读取流帧失败: %v", err)
		}
		sb.WriteString(frame.Content)
	}
	if sb.Len() == 0 {
		t.Error("流式响应应非空")
	} else {
		t.Logf("deepseek-chat 流式响应: %s", sb.String())
	}

	// metering 落库且 usage > 0（流式回调在 goroutine 中消费，轮询等待）。
	waitForCondition(t, 10*time.Second, func() bool {
		records, err := app.Store().QueryMetering(store.MeteringFilter{Model: entry.Model})
		return err == nil && len(records) == 1 && records[0].TotalTokens > 0
	}, "metering 应有 1 条 usage>0 的记录")
}

// waitForCondition 以 20ms 间隔轮询 cond 直到为真或超时。
func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool, failMsg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待条件在 %s 内未满足：%s", timeout, failMsg)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
