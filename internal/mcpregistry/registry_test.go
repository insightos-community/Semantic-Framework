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

package mcpregistry

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/mcp"
)

// echoArgs 是 echo 工具的入参（schema 由 SDK 按结构体推断）。
type echoArgs struct {
	Text string `json:"text" jsonschema:"要回显的文本"`
}

// testMCPServer 是可关停并在原地址重启的 MCP HTTP server
// （模拟进程崩溃后拉起：地址不变，连接全断），工具集可热增删
// （AddTool/RemoveTools 触发 list-changed），echo 调用计数。
// 模式复用自 pkg/mcp 的 restartableServer（test 包内不可导入，按仓内惯例拷贝）。
type testMCPServer struct {
	// addr 固定监听地址（127.0.0.1:port）。
	addr string

	// sdk server 端 SDK 对象（跨重启复用，工具集不丢）。
	sdk *sdkmcp.Server

	// mu 保护 httpSrv。
	mu sync.Mutex

	// httpSrv 当前在跑的 HTTP server；关停后为 nil。
	httpSrv *http.Server

	// echoCalls echo 工具被调次数（断言调用路径真实到达 server）。
	echoCalls int32
}

// newTestMCPServer 在随机端口启动 MCP server（echo/weather 两工具）。
func newTestMCPServer(t *testing.T) *testMCPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	s := &testMCPServer{addr: ln.Addr().String()}
	s.sdk = sdkmcp.NewServer(&sdkmcp.Implementation{Name: "registry-test-server", Version: "v0.0.1"}, nil)
	s.addEchoTool()
	sdkmcp.AddTool(s.sdk, &sdkmcp.Tool{Name: "weather", Description: "查询城市天气"},
		func(_ context.Context, _ *sdkmcp.CallToolRequest, in struct {
			City string `json:"city" jsonschema:"城市名"`
		}) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: in.City + "：晴，25°C"}},
			}, nil, nil
		})
	s.serve(t, ln)
	t.Cleanup(s.stop)
	return s
}

// addEchoTool 注册 echo 工具（描述可变——reconcile updated 判定用）。
func (s *testMCPServer) addEchoTool(desc ...string) {
	description := "回显输入文本"
	if len(desc) > 0 {
		description = desc[0]
	}
	sdkmcp.AddTool(s.sdk, &sdkmcp.Tool{Name: "echo", Description: description},
		func(_ context.Context, _ *sdkmcp.CallToolRequest, in echoArgs) (*sdkmcp.CallToolResult, any, error) {
			atomic.AddInt32(&s.echoCalls, 1)
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "echo: " + in.Text}},
			}, nil, nil
		})
}

// serve 在给定 listener 上运行 SDK streamable HTTP server。
func (s *testMCPServer) serve(t *testing.T, ln net.Listener) {
	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return s.sdk }, nil)
	httpSrv := &http.Server{Handler: handler}
	s.mu.Lock()
	s.httpSrv = httpSrv
	s.mu.Unlock()
	go func() { _ = httpSrv.Serve(ln) }()
}

// stop 关停 server（listener 与活动连接全部断开）。
func (s *testMCPServer) stop() {
	s.mu.Lock()
	httpSrv := s.httpSrv
	s.httpSrv = nil
	s.mu.Unlock()
	if httpSrv != nil {
		_ = httpSrv.Close()
	}
}

// restart 在原地址重启 server（同一 SDK 对象，工具集保持）。
func (s *testMCPServer) restart(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		t.Fatalf("原地址 %s 重新监听失败: %v", s.addr, err)
	}
	s.serve(t, ln)
}

// endpoint 返回 MCP endpoint URL。
func (s *testMCPServer) endpoint() string {
	return "http://" + s.addr + "/mcp"
}

// spec 生成指向本 server 的同步输入。
func (s *testMCPServer) spec(name string) ServerSpec {
	return ServerSpec{
		Config: mcp.ServerConfig{Name: name, Transport: mcp.TransportHTTP, Endpoint: s.endpoint()},
	}
}

// echoCallCount 返回 echo 工具被调次数。
func (s *testMCPServer) echoCallCount() int {
	return int(atomic.LoadInt32(&s.echoCalls))
}

// newTestRegistry 创建短短路窗口的连接池 + 目录（恢复路径不等 15min 默认短路）。
func newTestRegistry(t *testing.T) (*mcp.Pool, *Registry) {
	t.Helper()
	pool := mcp.NewPool(nil, &mcp.PoolOptions{CircuitOpenDuration: 100 * time.Millisecond})
	t.Cleanup(pool.Close)
	return pool, NewRegistry(pool, nil)
}

// waitFor 轮询 cond 直到成立或超时（5s），超时报测试失败。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待超时: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// entryOf 断言条目存在并返回（测试快捷路径）。
func entryOf(t *testing.T, reg *Registry, fullName string) Entry {
	t.Helper()
	e, ok := reg.Get(fullName)
	if !ok {
		t.Fatalf("目录应含条目 %q", fullName)
	}
	return e
}

// TestSyncServerReconcile 验证 reconcile 三态：首同步全 added；
// 无变化再同步三态全空（幂等，UpdatedAt 保留）；描述变化 → updated；
// 工具下线 → removed。
func TestSyncServerReconcile(t *testing.T) {
	server := newTestMCPServer(t)
	_, reg := newTestRegistry(t)
	spec := server.spec("test")

	// ① 首同步：两工具全 added，契约字段完整。
	report, err := reg.SyncServer(context.Background(), spec)
	if err != nil {
		t.Fatalf("首同步失败: %v", err)
	}
	if len(report.Added) != 2 || report.Added[0] != "test.echo" || report.Added[1] != "test.weather" ||
		len(report.Updated) != 0 || len(report.Removed) != 0 {
		t.Fatalf("首同步应全 added，实际: %+v", report)
	}
	echo := entryOf(t, reg, "test.echo")
	if echo.Server != "test" || echo.Tool != "echo" || echo.Description != "回显输入文本" ||
		echo.Risk != "medium" || echo.SourceKind != SourceKindMCP || echo.Health != HealthHealthy ||
		echo.UpdatedAt.IsZero() {
		t.Errorf("条目契约不符: %+v", echo)
	}
	firstUpdatedAt := echo.UpdatedAt

	// ② 幂等：无变化再同步三态全空，UpdatedAt 保留。
	report, err = reg.SyncServer(context.Background(), spec)
	if err != nil {
		t.Fatalf("二次同步失败: %v", err)
	}
	if !report.empty() {
		t.Errorf("无变化对账应三态全空，实际: %+v", report)
	}
	if got := entryOf(t, reg, "test.echo").UpdatedAt; got != firstUpdatedAt {
		t.Errorf("无变化条目 UpdatedAt 应保留: %v → %v", firstUpdatedAt, got)
	}

	// ③ updated：echo 描述变化（SDK 不支持原地改，先删后加同名新描述）。
	server.sdk.RemoveTools("echo")
	server.addEchoTool("回显输入文本 v2")
	report, err = reg.SyncServer(context.Background(), spec)
	if err != nil {
		t.Fatalf("三次同步失败: %v", err)
	}
	if len(report.Updated) != 1 || report.Updated[0] != "test.echo" ||
		len(report.Added) != 0 || len(report.Removed) != 0 {
		t.Fatalf("描述变化应为 updated，实际: %+v", report)
	}
	if got := entryOf(t, reg, "test.echo"); got.Description != "回显输入文本 v2" || !got.UpdatedAt.After(firstUpdatedAt) {
		t.Errorf("updated 条目应刷新描述与 UpdatedAt: %+v", got)
	}

	// ④ removed：weather 下线。
	server.sdk.RemoveTools("weather")
	report, err = reg.SyncServer(context.Background(), spec)
	if err != nil {
		t.Fatalf("四次同步失败: %v", err)
	}
	if len(report.Removed) != 1 || report.Removed[0] != "test.weather" {
		t.Fatalf("工具下线应为 removed，实际: %+v", report)
	}
	if _, ok := reg.Get("test.weather"); ok {
		t.Error("removed 条目不应留在目录")
	}
}

// TestSyncServerRiskOverride 验证 per-server 风险覆盖进入条目契约。
func TestSyncServerRiskOverride(t *testing.T) {
	server := newTestMCPServer(t)
	_, reg := newTestRegistry(t)
	spec := server.spec("test")
	spec.Risk = "high"

	if _, err := reg.SyncServer(context.Background(), spec); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if got := entryOf(t, reg, "test.echo").Risk; got != "high" {
		t.Errorf("risk 覆盖应生效，实际: %q", got)
	}
}

// TestSyncServerWarningKeepsEntries 验证 warning 不清仓：server 失联后
// 对账失败，旧条目保留且标 unavailable；恢复后标 healthy。
func TestSyncServerWarningKeepsEntries(t *testing.T) {
	server := newTestMCPServer(t)
	_, reg := newTestRegistry(t)
	spec := server.spec("test")

	if _, err := reg.SyncServer(context.Background(), spec); err != nil {
		t.Fatalf("首同步失败: %v", err)
	}

	// 失联：对账失败返回错误，但条目保留、标 unavailable。
	server.stop()
	if _, err := reg.SyncServer(context.Background(), spec); err == nil {
		t.Fatal("server 失联后对账应返回错误")
	}
	if got := len(reg.List()); got != 2 {
		t.Fatalf("失联不应清空条目（warning 不清仓），实际: %d 条", got)
	}
	if got := entryOf(t, reg, "test.echo").Health; got != HealthUnavailable {
		t.Errorf("失联后条目应标 unavailable，实际: %q", got)
	}
	// unavailable 条目不进入注入视图。
	if got := reg.MatchNamespaces([]string{"test.*"}); len(got) != 0 {
		t.Errorf("unavailable 条目不应命中注入过滤，实际: %d 条", len(got))
	}

	// 恢复（等短路窗口过半开试连）：条目回 healthy，注入视图恢复。
	server.restart(t)
	time.Sleep(150 * time.Millisecond)
	if _, err := reg.SyncServer(context.Background(), spec); err != nil {
		t.Fatalf("恢复后对账失败: %v", err)
	}
	if got := entryOf(t, reg, "test.echo").Health; got != HealthHealthy {
		t.Errorf("恢复后条目应标 healthy，实际: %q", got)
	}
	if got := reg.MatchNamespaces([]string{"test.*"}); len(got) != 2 {
		t.Errorf("恢复后注入视图应含 2 条，实际: %d", len(got))
	}
}

// TestMarkHealth 验证外部健康标记（provider 心跳语义入口）：
// unavailable 不删条目、可恢复。
func TestMarkHealth(t *testing.T) {
	server := newTestMCPServer(t)
	_, reg := newTestRegistry(t)
	if _, err := reg.SyncServer(context.Background(), server.spec("test")); err != nil {
		t.Fatalf("首同步失败: %v", err)
	}

	reg.MarkUnavailable("test")
	if got := entryOf(t, reg, "test.echo").Health; got != HealthUnavailable {
		t.Errorf("MarkUnavailable 后应为 unavailable，实际: %q", got)
	}
	if got := len(reg.List()); got != 2 {
		t.Errorf("标记不应删条目，实际: %d 条", got)
	}

	reg.MarkHealthy("test")
	if got := entryOf(t, reg, "test.echo").Health; got != HealthHealthy {
		t.Errorf("MarkHealthy 后应为 healthy，实际: %q", got)
	}
}

// TestMatchNamespaces 验证注入过滤的命名空间语义（"test.*" 前缀 /
// 精确全名 / 空模式不匹配）。
func TestMatchNamespaces(t *testing.T) {
	server := newTestMCPServer(t)
	_, reg := newTestRegistry(t)
	if _, err := reg.SyncServer(context.Background(), server.spec("test")); err != nil {
		t.Fatalf("首同步失败: %v", err)
	}

	if got := reg.MatchNamespaces([]string{"test.*"}); len(got) != 2 {
		t.Errorf(`"test.*" 应命中 2 条，实际: %d`, len(got))
	}
	if got := reg.MatchNamespaces([]string{"test.echo"}); len(got) != 1 || got[0].FullName != "test.echo" {
		t.Errorf("精确全名应命中 1 条，实际: %+v", got)
	}
	if got := reg.MatchNamespaces([]string{"other.*"}); len(got) != 0 {
		t.Errorf("异命名空间不应命中，实际: %d", len(got))
	}
	if got := reg.MatchNamespaces(nil); len(got) != 0 {
		t.Errorf("空模式不应命中（未声明工具范围的角色拿不到工具），实际: %d", len(got))
	}
}

// TestCallTool 验证调用路径：结果归一为 OK 信封、真实到达 server；
// 工具错误归一为 TOOL_ERROR（不重试）；未知 server 归一 MCP_SERVER_UNKNOWN。
func TestCallTool(t *testing.T) {
	server := newTestMCPServer(t)
	_, reg := newTestRegistry(t)
	spec := server.spec("test")
	if _, err := reg.SyncServer(context.Background(), spec); err != nil {
		t.Fatalf("首同步失败: %v", err)
	}

	out, err := reg.CallTool(context.Background(), "test", "echo", `{"text":"hello"}`)
	if err != nil {
		t.Fatalf("CallTool 返回 Go error: %v", err)
	}
	if out != `{"ok":true,"data":"echo: hello"}` {
		t.Errorf("调用结果应归一为 OK 信封，实际: %s", out)
	}
	if got := server.echoCallCount(); got != 1 {
		t.Errorf("echo 应被真实调用 1 次，实际: %d", got)
	}

	// 协议错误（未知工具）：server 健康，归一 TOOL_ERROR 不重试。
	out, err = reg.CallTool(context.Background(), "test", "bogus", `{}`)
	if err != nil {
		t.Fatalf("工具错误不应返回 Go error: %v", err)
	}
	if !containsAll(out, `"ok":false`, tool.CodeToolError, `"retryable":false`) {
		t.Errorf("未知工具应归一 TOOL_ERROR，实际: %s", out)
	}
	if got := entryOf(t, reg, "test.echo").Health; got != HealthHealthy {
		t.Errorf("协议错误不应影响健康状态，实际: %q", got)
	}

	// 未知 server：归一 MCP_SERVER_UNKNOWN。
	out, _ = reg.CallTool(context.Background(), "ghost", "echo", `{}`)
	if !containsAll(out, `"ok":false`, CodeServerUnknown) {
		t.Errorf("未知 server 应归一 MCP_SERVER_UNKNOWN，实际: %s", out)
	}
}

// TestCallToolUnavailable 验证失联后的调用路径：结构化 MCP_SERVER_UNAVAILABLE
// （retryable），server 标 unavailable；恢复后调用自愈标 healthy。
func TestCallToolUnavailable(t *testing.T) {
	server := newTestMCPServer(t)
	_, reg := newTestRegistry(t)
	spec := server.spec("test")
	if _, err := reg.SyncServer(context.Background(), spec); err != nil {
		t.Fatalf("首同步失败: %v", err)
	}

	server.stop()
	out, err := reg.CallTool(context.Background(), "test", "echo", `{"text":"hi"}`)
	if err != nil {
		t.Fatalf("失联调用不应返回 Go error: %v", err)
	}
	if !containsAll(out, `"ok":false`, CodeServerUnavailable, `"retryable":true`) {
		t.Errorf("失联调用应归一 MCP_SERVER_UNAVAILABLE 且可重试，实际: %s", out)
	}
	if got := entryOf(t, reg, "test.echo").Health; got != HealthUnavailable {
		t.Errorf("调用失联应标 unavailable，实际: %q", got)
	}

	server.restart(t)
	time.Sleep(150 * time.Millisecond) // 等短路窗口过半开试连
	out, err = reg.CallTool(context.Background(), "test", "echo", `{"text":"back"}`)
	if err != nil || out != `{"ok":true,"data":"echo: back"}` {
		t.Fatalf("恢复后调用 = %s, %v", out, err)
	}
	if got := entryOf(t, reg, "test.echo").Health; got != HealthHealthy {
		t.Errorf("调用成功应自愈标 healthy，实际: %q", got)
	}
}

// TestRemoveServer 验证配置删除路径：条目与同步输入一并移除
// （与失联标 unavailable 严格区分）。
func TestRemoveServer(t *testing.T) {
	server := newTestMCPServer(t)
	_, reg := newTestRegistry(t)
	if _, err := reg.SyncServer(context.Background(), server.spec("test")); err != nil {
		t.Fatalf("首同步失败: %v", err)
	}

	reg.RemoveServer("test")
	if got := len(reg.List()); got != 0 {
		t.Errorf("RemoveServer 应清空该 server 条目，实际: %d 条", got)
	}
	// spec 一并移除：调用归一 MCP_SERVER_UNKNOWN 而不是不可用。
	out, _ := reg.CallTool(context.Background(), "test", "echo", `{}`)
	if !containsAll(out, CodeServerUnknown) {
		t.Errorf("移除后调用应归一 MCP_SERVER_UNKNOWN，实际: %s", out)
	}
}

// containsAll 报告 s 是否包含全部子串（断言辅助）。
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
