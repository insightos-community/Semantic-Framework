package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"insightos.cn/semantic-framework/internal/bootstrap"
	"insightos.cn/semantic-framework/internal/mcpregistry"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/log"
	"insightos.cn/semantic-framework/pkg/mcp"
)

// MCP 集成测试（R16 交付清单第 4 条）：内存 MCP test server（echo/weather）
// → mcp_servers 声明 → 起 App → 目录含 test.echo → mock leader 先检索再调
// test_echo（净化名）→ 经 pool 执行成功 → 停 server → 条目 unavailable 未清除 →
// 恢复 → healthy 且可再次调用。
//
// 对账节奏经 WireWithOptions 注入：关闭周期对账（FastRounds=0 + 1h 稳态），
// 上下线形态由显式 SyncServer 驱动，消除后台时序对断言的干扰；
// 连接池短路窗口缩到 2s（恢复路径不等 15min 默认值）。

// mcpEchoArgs 是 echo 工具的入参。
type mcpEchoArgs struct {
	Text string `json:"text" jsonschema:"要回显的文本"`
}

// mcpTestServer 是可关停并在原地址重启的 MCP HTTP server（echo/weather 两
// 工具，echo 调用计数）。模式复用自 pkg/mcp 的 restartableServer。
type mcpTestServer struct {
	// addr 固定监听地址。
	addr string

	// sdk server 端 SDK 对象（跨重启复用）。
	sdk *sdkmcp.Server

	// mu 保护 httpSrv。
	mu sync.Mutex

	// httpSrv 当前在跑的 HTTP server；关停后为 nil。
	httpSrv *http.Server

	// echoCalls echo 工具被调次数。
	echoCalls int32
}

// newMCPTestServer 在随机端口启动 MCP server。
func newMCPTestServer(t *testing.T) *mcpTestServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	s := &mcpTestServer{addr: ln.Addr().String()}
	s.sdk = sdkmcp.NewServer(&sdkmcp.Implementation{Name: "integration-test-mcp", Version: "v0.0.1"}, nil)
	sdkmcp.AddTool(s.sdk, &sdkmcp.Tool{Name: "echo", Description: "回显输入文本"},
		func(_ context.Context, _ *sdkmcp.CallToolRequest, in mcpEchoArgs) (*sdkmcp.CallToolResult, any, error) {
			atomic.AddInt32(&s.echoCalls, 1)
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "echo: " + in.Text}},
			}, nil, nil
		})
	sdkmcp.AddTool(s.sdk, &sdkmcp.Tool{Name: "weather", Description: "查询城市天气"},
		func(_ context.Context, _ *sdkmcp.CallToolRequest, in struct {
			City string `json:"city" jsonschema:"城市名"`
		}) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: in.City + "：晴，25°C"}},
			}, nil, nil
		})
	s.serve(ln)
	t.Cleanup(s.stop)
	return s
}

// serve 在给定 listener 上运行 SDK streamable HTTP server。
func (s *mcpTestServer) serve(ln net.Listener) {
	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return s.sdk }, nil)
	httpSrv := &http.Server{Handler: handler}
	s.mu.Lock()
	s.httpSrv = httpSrv
	s.mu.Unlock()
	go func() { _ = httpSrv.Serve(ln) }()
}

// stop 关停 server（listener 与活动连接全部断开，模拟进程猝死）。
func (s *mcpTestServer) stop() {
	s.mu.Lock()
	httpSrv := s.httpSrv
	s.httpSrv = nil
	s.mu.Unlock()
	if httpSrv != nil {
		_ = httpSrv.Close()
	}
}

// restart 在原地址重启 server。
func (s *mcpTestServer) restart(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		t.Fatalf("原地址 %s 重新监听失败: %v", s.addr, err)
	}
	s.serve(ln)
}

// endpoint 返回 MCP endpoint URL。
func (s *mcpTestServer) endpoint() string {
	return "http://" + s.addr + "/mcp"
}

// echoCallCount 返回 echo 工具被调次数。
func (s *mcpTestServer) echoCallCount() int {
	return int(atomic.LoadInt32(&s.echoCalls))
}

// writeMCPLeaderProfile 把真实 leader profile 复制到临时目录并把 test.*
// 加入 tools.namespaces（真实 profile 只放行 system.*/artifact.*/skill.*，
// 测试 server 的命名空间 test 需要显式授权——这本身就是命名空间过滤的验证）。
func writeMCPLeaderProfile(t *testing.T) string {
	t.Helper()
	srcDir, err := filepath.Abs(filepath.Join("..", "..", "configs", "agents", "leader"))
	if err != nil {
		t.Fatalf("解析 leader profile 目录失败: %v", err)
	}
	dstDir := filepath.Join(t.TempDir(), "agents", "leader")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatalf("创建临时 profile 目录失败: %v", err)
	}
	for _, name := range []string{"AGENT.md", "SAFETY.md"} {
		data, err := os.ReadFile(filepath.Join(srcDir, name))
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dstDir, name), data, 0o644); err != nil {
			t.Fatalf("写入 %s 失败: %v", name, err)
		}
	}
	role, err := os.ReadFile(filepath.Join(srcDir, "role.yaml"))
	if err != nil {
		t.Fatalf("读取 role.yaml 失败: %v", err)
	}
	// 测试只关心把 test.* 加入首个 namespaces 列表，不应依赖生产配置中
	// 还启用了哪些内置工具。否则新增 execute 等无关工具会让 MCP 验收失效。
	patched := strings.Replace(string(role), "namespaces: [", "namespaces: [test.*, ", 1)
	if patched == string(role) {
		t.Fatal("role.yaml 的 namespaces 行未命中替换（profile 结构变化？）")
	}
	if err := os.WriteFile(filepath.Join(dstDir, "role.yaml"), []byte(patched), 0o644); err != nil {
		t.Fatalf("写入 role.yaml 失败: %v", err)
	}
	return filepath.Dir(dstDir)
}

// startMCPApp 以指定 MCP server 启动装配后的服务（mock 模型 + 放行 test.*
// 的 leader profile + 关闭周期对账 + 2s 短路窗口）。
func startMCPApp(t *testing.T, dbPath, mcpEndpoint, profilesDir string) (httpBase, wsBase string, app *bootstrap.App, stop func()) {
	t.Helper()
	t.Setenv("SEMANTIC_ADMIN_PASSWORD", "test-admin-pass")

	cfg := config.Default()
	cfg.Server.HTTPAddr = freeAddr(t)
	cfg.Server.WSAddr = freeAddr(t)
	cfg.Store.SQLitePath = dbPath
	storetest.SeedMigratedAt(t, cfg.Store.SQLitePath)
	cfg.LLM.Default = "mock"
	cfg.Agents.ProfilesDir = profilesDir
	cfg.MCPServers = []config.MCPServerConfig{{
		Name: "test", Transport: mcp.TransportHTTP, Endpoint: mcpEndpoint, Enabled: true,
	}}
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})

	var err error
	app, err = bootstrap.WireWithOptions(cfg, logger, bootstrap.Options{
		MCPPool:   &mcp.PoolOptions{CircuitOpenDuration: 2 * time.Second},
		MCPSyncer: &mcpregistry.SyncerOptions{FastRounds: 0, SteadyInterval: time.Hour},
	})
	if err != nil {
		t.Fatalf("WireWithOptions 装配失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- app.Run(ctx)
	}()

	httpBase = fmt.Sprintf("http://%s", cfg.Server.HTTPAddr)
	wsBase = fmt.Sprintf("ws://%s", cfg.Server.WSAddr)
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(httpBase + "/api/v1/system/healthz")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("服务在 30s 内未就绪，最后一次错误: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	stop = func() {
		cancel()
		select {
		case err := <-runDone:
			if err != nil {
				t.Errorf("App.Run 应随 ctx 取消正常退出，实际返回: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Errorf("App.Run 未在 15s 内退出")
		}
	}
	return httpBase, wsBase, app, stop
}

// mcpSpec 生成指向集成测试 server 的目录同步输入（显式对账驱动用）。
func mcpSpec(server *mcpTestServer) mcpregistry.ServerSpec {
	return mcpregistry.ServerSpec{
		Config: mcp.ServerConfig{Name: "test", Transport: mcp.TransportHTTP, Endpoint: server.endpoint()},
	}
}

// toolsGet 查询工具目录（带鉴权），返回按 sources 分组的原始结构。
func toolsGet(t *testing.T, httpBase, token string) map[string][]map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, httpBase+"/api/v1/tools", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("查询工具目录失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查询工具目录应返回 200，实际: %d", resp.StatusCode)
	}
	var body struct {
		Sources []struct {
			Kind  string           `json:"kind"`
			Tools []map[string]any `json:"tools"`
		} `json:"sources"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析工具目录响应失败: %v", err)
	}
	out := make(map[string][]map[string]any, len(body.Sources))
	for _, src := range body.Sources {
		out[src.Kind] = src.Tools
	}
	return out
}

// findToolView 在来源组中按名查找工具视图。
func findToolView(tools []map[string]any, name string) map[string]any {
	for _, tv := range tools {
		if tv["name"] == name {
			return tv
		}
	}
	return nil
}

// TestMCPToolCatalogAndCall 是 R16 的端到端验收：目录发现 → 净化名调用 →
// 失联降级（不清仓）→ 恢复自愈。
func TestMCPToolCatalogAndCall(t *testing.T) {
	server := newMCPTestServer(t)
	profilesDir := writeMCPLeaderProfile(t)
	t.Setenv("SEMANTIC_MOCK_SCRIPT", mcpEchoScript(t))
	dbPath := filepath.Join(t.TempDir(), "mcp.db")
	httpBase, wsBase, app, stop := startMCPApp(t, dbPath, server.endpoint(), profilesDir)
	defer stop()

	token := login(t, httpBase)

	// ① 目录收敛：REST 目录的 mcp 组出现 test.echo（healthy），
	//    builtin 组同时存在（按来源分组）。
	waitForCondition(t, 5*time.Second, func() bool {
		groups := toolsGet(t, httpBase, token)
		tv := findToolView(groups["mcp"], "test.echo")
		return tv != nil && tv["health"] == "healthy" && tv["server"] == "test" &&
			tv["namespace"] == "test" && tv["risk"] == "medium" && tv["updated_at"] != nil &&
			len(groups["builtin"]) > 0
	}, "目录出现 test.echo")
	entry, ok := app.MCPRegistry().Get("test.echo")
	if !ok || entry.Health != mcpregistry.HealthHealthy || entry.Tool != "echo" {
		t.Fatalf("目录条目不符: %+v, ok=%v", entry, ok)
	}

	// ② mock leader 调 test_echo（净化名）→ 经门禁 L2 → pool 执行成功。
	sessionID, conn := createSessionAndDial(t, httpBase, wsBase, token)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	sendChatMessage(t, conn, sessionID, "用 echo 工具回显 hello")
	done := awaitMessageDone(t, conn)
	if done.Error != "" || done.Text != "已为你回显：echo: hello" {
		t.Errorf("message.done 不符: %+v", done)
	}
	if got := server.echoCallCount(); got != 1 {
		t.Errorf("echo 应经 pool 真实执行 1 次，实际: %d", got)
	}

	// ③ 停 server（猝死）→ 显式对账失败 → 条目标 unavailable 且未清除。
	server.stop()
	if _, err := app.MCPRegistry().SyncServer(context.Background(), mcpSpec(server)); err == nil {
		t.Fatal("server 猝死之后对账应返回错误")
	}
	entry, ok = app.MCPRegistry().Get("test.echo")
	if !ok {
		t.Fatal("失联不应清除条目（warning 不清仓）")
	}
	if entry.Health != mcpregistry.HealthUnavailable {
		t.Errorf("失联后条目应标 unavailable，实际: %q", entry.Health)
	}
	groups := toolsGet(t, httpBase, token)
	if tv := findToolView(groups["mcp"], "test.echo"); tv == nil || tv["health"] != "unavailable" {
		t.Errorf("REST 目录应展示 unavailable 条目，实际: %+v", tv)
	}
	// 调用路径降级：结构化 MCP_SERVER_UNAVAILABLE（可重试），不是 Go error。
	out, err := app.MCPRegistry().CallTool(context.Background(), "test", "echo", `{"text":"hi"}`)
	if err != nil {
		t.Fatalf("失联调用不应返回 Go error: %v", err)
	}
	if !strings.Contains(out, "MCP_SERVER_UNAVAILABLE") || !strings.Contains(out, `"retryable":true`) {
		t.Errorf("失联调用应归一 MCP_SERVER_UNAVAILABLE 且可重试，实际: %s", out)
	}

	// ④ 恢复 → 对账成功 → healthy，工具可再次调用。
	server.restart(t)
	time.Sleep(2100 * time.Millisecond) // 等短路窗口（2s）过半开试连
	if _, err := app.MCPRegistry().SyncServer(context.Background(), mcpSpec(server)); err != nil {
		t.Fatalf("恢复后对账失败: %v", err)
	}
	if entry, _ = app.MCPRegistry().Get("test.echo"); entry.Health != mcpregistry.HealthHealthy {
		t.Errorf("恢复后条目应标 healthy，实际: %q", entry.Health)
	}
	out, err = app.MCPRegistry().CallTool(context.Background(), "test", "echo", `{"text":"back"}`)
	if err != nil || !strings.Contains(out, "echo: back") {
		t.Errorf("恢复后调用 = %s, %v", out, err)
	}
}

// mcpEchoScript 生成 MCP 调用场景的 mock 脚本：第一轮检索 echo，第二轮调
// test_echo（净化名），第三轮总结（文本与测试断言对应）。
func mcpEchoScript(t *testing.T) string {
	t.Helper()
	// MCP 场景的工具集很小，应由 ToolsNode 直接装配；ToolSearch 的预算
	// 启用与检索链路由 toolsearch_test.go 单独覆盖。
	script, err := json.Marshal([]map[string]any{
		{"tool_calls": []map[string]string{{
			"id": "call-1", "name": "test_echo", "arguments": `{"text":"hello"}`,
		}}},
		{"content": "已为你回显：echo: hello"},
	})
	if err != nil {
		t.Fatalf("构造 mock 脚本失败: %v", err)
	}
	return string(script)
}
