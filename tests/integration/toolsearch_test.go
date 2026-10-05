package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"insightos.cn/semantic-framework/internal/bootstrap"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/log"
)

// ToolSearch 动态检索的集成测试：真实装配（bootstrap.Wire：内置 5 + 技能目录
// 2 工具）+ 集成测试补注册的 demo.* 工具 → 自建 leader
// profile（tool_search: true，pinned: [system.time]）→ mock 脚本"先检索
// tool_search 再调目标工具"经 WS 对话全链路跑通。
//
// 断言路径说明（与 skill_test 同一取舍）：mock 模型实例在 runtime 内部构建，
// ToolInfos 可见性（动态集初始隐藏/检索后累积可见）的内容级断言在 eino 侧
// 已有覆盖，挂接条件与 pinned 直通在 kernel/runtime 单测；集成层验证装配链
// 与数据流——探测工具的执行计数证明"动态工具经检索后真实出现在 tools 节点
// 并可调用"（mock 脚本的调用序列不依赖工具是否存在：未知工具只产生错误
// 工具结果、run 照样走完，执行计数是区分两形态的外部可观测信号），检索
// 轮次经 trace 落库（择可测路径：TraceHandler 只记录模型跨度，检索表现
// 为发起 tool_search 的首轮模型调用与后续轮次都进 trace_spans）。

// probeTool 是检索链路的探测工具（demo 命名空间）：执行计数证明动态工具
// 经 tool_search 检索后真实可调用。
type probeTool struct {
	// hits 执行计数（原子，run 在后台 goroutine 执行）。
	hits int32
}

// Def 返回 demo.probe 的契约。
func (t *probeTool) Def() tool.Definition {
	return tool.Definition{
		Name: "demo.probe", Namespace: "demo", Description: "检索链路探测工具：回显文本。",
		ParametersJSON: `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`,
		Annotations:    tool.Annotations{Risk: tool.RiskLow, Idempotent: true},
	}
}

// Run 执行 demo.probe：计数并回显。
func (t *probeTool) Run(_ context.Context, _ string) (string, error) {
	atomic.AddInt32(&t.hits, 1)
	return tool.OKResult(map[string]any{"text": "probed"})
}

// fillerTool 是把动态集推过阈值的占位工具（demo 命名空间，永不被调用）。
type fillerTool struct {
	// name 工具全名（demo.t01..）。
	name string
}

// waitToolSearchMetering 轮询等待指定模型的计量记录达到预期数量。
// 流式回调在独立 goroutine 中落库，使用带截止时间的轮询可以避免固定等待
// 带来的竞态，同时不再依赖已经删除的 Depth 路由测试文件提供公共函数。
func waitToolSearchMetering(t *testing.T, st *store.Store, model string, want int) []store.Metering {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		records, err := st.QueryMetering(store.MeteringFilter{Model: model})
		if err != nil {
			t.Fatalf("QueryMetering 失败: %v", err)
		}
		if len(records) == want {
			return records
		}
		if time.Now().After(deadline) {
			t.Fatalf("5s 内模型 %q 的计量记录未达到 %d 条，当前: %d", model, want, len(records))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Def 返回占位工具的契约。
func (t fillerTool) Def() tool.Definition {
	return tool.Definition{
		// 较长描述让整组 schema 稳定超过 10% 预算；这比把上下文预算压到
		// 极小值更贴近真实“大工具目录”，也不会误触发历史摘要模型调用。
		Name: t.name, Namespace: "demo",
		Description:    "占位工具 " + t.name + strings.Repeat("，用于大目录预算测试", 40),
		ParametersJSON: `{"type":"object","properties":{},"additionalProperties":false}`,
		Annotations:    tool.Annotations{Risk: tool.RiskLow},
	}
}

// Run 执行占位工具：返回固定成功结果。
func (t fillerTool) Run(_ context.Context, _ string) (string, error) {
	return tool.OKResult(map[string]any{"ok": true})
}

// writeToolsearchProfiles 在临时目录写一套最小 leader profile：开启
// tool_search，pinned [system.time]，命名空间覆盖内置/技能目录/demo 工具。
// 为什么自建 profile 而不改共享 configs：隔离 demo.* 授权与 mock 模型配置，
// 不让集成探测工具进入共享角色契约。
func writeToolsearchProfiles(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	leader := filepath.Join(dir, "leader")
	if err := os.MkdirAll(leader, 0o755); err != nil {
		t.Fatalf("创建 profile 目录失败: %v", err)
	}
	role := `name: leader
mode: coordinator
description: ToolSearch 集成测试角色
model: mock
tools:
  namespaces: [system.*, artifact.*, skill.*, demo.*]
  pinned: [system.time]
  tool_search: true
limits:
  # 结合长工具描述，稳定触发“schema 超过上下文 10%”的集成测试分支。
  context_tokens: 10000
  max_turns: 10
interrupt:
  approval_required: []
`
	if err := os.WriteFile(filepath.Join(leader, "role.yaml"), []byte(role), 0o644); err != nil {
		t.Fatalf("写入 role.yaml 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(leader, "AGENT.md"),
		[]byte("# Role\n你是 Leader。"), 0o644); err != nil {
		t.Fatalf("写入 AGENT.md 失败: %v", err)
	}
	return dir
}

// toolsearchScript 生成检索场景的 mock 脚本：第一轮 tool_search 检索
// （关键词 probe 命中 demo_probe），第二轮调用 demo_probe，第三轮总结。
func toolsearchScript(t *testing.T) string {
	t.Helper()
	script, err := json.Marshal([]map[string]any{
		{"tool_calls": []map[string]string{{
			"id": "call-1", "name": "tool_search",
			"arguments": `{"query":"probe"}`,
		}}},
		{"tool_calls": []map[string]string{{
			"id": "call-2", "name": "demo_probe",
			"arguments": `{"text":"hi"}`,
		}}},
		{"content": "已通过 tool_search 检索并调用探测工具。"},
	})
	if err != nil {
		t.Fatalf("构造 mock 脚本失败: %v", err)
	}
	return string(script)
}

// waitSpans 轮询等待指定链路的跨度记录达到 want 条（流式时机的跨度在
// 独立 goroutine 落库，轮询规避竞态而非引入固定睡眠）。
func waitSpans(t *testing.T, st *store.Store, traceID string, want int) []store.Span {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		spans, err := st.QuerySpans(traceID)
		if err != nil {
			t.Fatalf("QuerySpans 失败: %v", err)
		}
		if len(spans) >= want {
			return spans
		}
		if time.Now().After(deadline) {
			t.Fatalf("5s 内链路 %q 的跨度未达到 %d 条，当前: %d", traceID, want, len(spans))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestToolSearchRun 验证 ToolSearch 装配链全链路：开关显式开启 → middleware
// 挂接 → 脚本化
// 检索-调用序列跑通（探测工具真实执行）→ 三轮模型调用全部进 trace。
func TestToolSearchRun(t *testing.T) {
	t.Setenv("SEMANTIC_ADMIN_PASSWORD", "test-admin-pass")
	t.Setenv("SEMANTIC_MOCK_SCRIPT", toolsearchScript(t))

	profilesDir := writeToolsearchProfiles(t)
	skillsDir, err := filepath.Abs(filepath.Join("..", "..", "configs", "skills"))
	if err != nil {
		t.Fatalf("解析技能目录失败: %v", err)
	}
	cfg := config.Default()
	cfg.Server.HTTPAddr = freeAddr(t)
	cfg.Server.WSAddr = freeAddr(t)
	cfg.Store.SQLitePath = filepath.Join(t.TempDir(), "toolsearch.db")
	storetest.SeedMigratedAt(t, cfg.Store.SQLitePath)
	cfg.LLM.Default = "mock"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"mock": {Component: "mock", Model: "mock-model", Capabilities: []string{"text", "tool_call"}},
	}
	cfg.Agents.ProfilesDir = profilesDir
	cfg.Agents.TeamsDir = filepath.Join(t.TempDir(), "no-teams")
	cfg.Skills.Dir = skillsDir
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})

	app, err := bootstrap.Wire(cfg, logger)
	if err != nil {
		t.Fatalf("Wire 装配失败: %v", err)
	}

	// 补注册 demo.* 探测工具：注册表并发安全，buildToolchain
	// 在首条消息构建运行器时才按 profile 过滤，此刻注册对该会话生效。
	probe := &probeTool{}
	if err := app.ToolRegistry().Register(probe); err != nil {
		t.Fatalf("注册探测工具失败: %v", err)
	}
	for i := 1; i <= 8; i++ {
		if err := app.ToolRegistry().Register(fillerTool{name: fmt.Sprintf("demo.t%02d", i)}); err != nil {
			t.Fatalf("注册占位工具失败: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- app.Run(ctx)
	}()
	defer func() {
		cancel()
		select {
		case err := <-runDone:
			if err != nil {
				t.Errorf("App.Run 应随 ctx 取消正常退出，实际返回: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Errorf("App.Run 未在 15s 内退出")
		}
	}()

	httpBase := "http://" + cfg.Server.HTTPAddr
	wsBase := "ws://" + cfg.Server.WSAddr
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(httpBase + "/api/v1/system/healthz")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("服务在 30s 内未就绪，最后一次错误: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	token := login(t, httpBase)
	sessionID, conn := createSessionAndDial(t, httpBase, wsBase, token)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	sendChatMessage(t, conn, sessionID, "帮我调用探测工具")
	done := awaitMessageDone(t, conn)
	if done.Error != "" {
		t.Fatalf("run 不应失败，实际错误: %q", done.Error)
	}
	if done.Turns != 3 {
		t.Errorf("应为 3 轮（检索 + 调用 + 总结），实际: %d", done.Turns)
	}
	if !strings.Contains(done.Text, "检索") {
		t.Errorf("最终文本不符: %q", done.Text)
	}

	// 探测工具真实执行：动态工具经检索后出现在 tools 节点并可调用
	// （若 middleware 未挂接，demo_probe 不在 tools 节点，未知工具只产生
	// 错误结果、计数为 0）。
	if hits := atomic.LoadInt32(&probe.hits); hits != 1 {
		t.Errorf("探测工具应执行 1 次，实际: %d", hits)
	}

	// 检索轮次进 trace：一个 Agent 根跨度、三轮模型调用与两次工具调用
	// 都属于 message.done 返回的精确 trace_id。
	records := waitToolSearchMetering(t, app.Store(), "mock-model", 3)
	spans := waitSpans(t, app.Store(), done.TraceID, 6)
	var roots, models, tools int
	for _, sp := range spans {
		switch sp.Kind {
		case "Agent":
			if sp.ParentID == "" {
				roots++
			}
		case "ChatModel":
			models++
		case "Tool":
			tools++
		}
	}
	if roots != 1 || models != 3 || tools != 2 || records[0].TraceID != done.TraceID {
		t.Fatalf("ToolSearch Trace 树不符: roots=%d models=%d tools=%d records=%+v spans=%+v",
			roots, models, tools, records, spans)
	}
}
