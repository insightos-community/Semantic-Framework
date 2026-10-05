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

package mcp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeClock 是可手动推进的时钟（注入 PoolOptions.Now 快进退短路窗口）。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

// now 返回当前假时钟读数。
func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// advance 推进假时钟。
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// restartableServer 是可关停并在原地址重启的 MCP HTTP server
// （模拟进程崩溃后拉起：地址不变，连接全断）。
type restartableServer struct {
	// addr 固定监听地址（127.0.0.1:port）。
	addr string

	// mu 保护 httpSrv。
	mu sync.Mutex

	// httpSrv 当前在跑的 HTTP server；关停后为 nil。
	httpSrv *http.Server
}

// newRestartableServer 在随机端口启动 MCP server（工具 ping → pong）。
func newRestartableServer(t *testing.T) *restartableServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	rs := &restartableServer{addr: ln.Addr().String()}
	rs.serve(t, ln)
	t.Cleanup(rs.stop)
	return rs
}

// serve 在给定 listener 上运行 SDK streamable HTTP server。
func (rs *restartableServer) serve(t *testing.T, ln net.Listener) {
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "pool-test-server", Version: "v0.0.1"}, nil)
	sdkmcp.AddTool(srv, &sdkmcp.Tool{Name: "ping", Description: "ping"},
		func(_ context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "pong"}},
			}, nil, nil
		})
	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil)
	httpSrv := &http.Server{Handler: handler}
	rs.mu.Lock()
	rs.httpSrv = httpSrv
	rs.mu.Unlock()
	go func() { _ = httpSrv.Serve(ln) }()
}

// stop 关停 server（listener 与活动连接全部断开）。
func (rs *restartableServer) stop() {
	rs.mu.Lock()
	httpSrv := rs.httpSrv
	rs.httpSrv = nil
	rs.mu.Unlock()
	if httpSrv != nil {
		_ = httpSrv.Close()
	}
}

// restart 在原地址重启 server。
func (rs *restartableServer) restart(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", rs.addr)
	if err != nil {
		t.Fatalf("原地址 %s 重新监听失败: %v", rs.addr, err)
	}
	rs.serve(t, ln)
}

// endpoint 返回 MCP endpoint URL。
func (rs *restartableServer) endpoint() string {
	return "http://" + rs.addr + "/mcp"
}

// poolConfig 生成指向测试 server 的连接配置。
func (rs *restartableServer) poolConfig(name string) ServerConfig {
	return ServerConfig{Name: name, Transport: TransportHTTP, Endpoint: rs.endpoint()}
}

// newTestPool 创建带假时钟的 Pool 并注册清理。
func newTestPool(t *testing.T, clock *fakeClock) *Pool {
	t.Helper()
	pool := NewPool(nil, &PoolOptions{
		CircuitOpenDuration: time.Minute,
		Now:                 clock.now,
	})
	t.Cleanup(pool.Close)
	return pool
}

func TestPoolGetReusesConnection(t *testing.T) {
	rs := newRestartableServer(t)
	pool := newTestPool(t, &fakeClock{t: time.Now()})
	cfg := rs.poolConfig("map")

	c1, err := pool.Get(context.Background(), cfg)
	if err != nil {
		t.Fatalf("首次 Get 失败: %v", err)
	}
	c2, err := pool.Get(context.Background(), cfg)
	if err != nil {
		t.Fatalf("二次 Get 失败: %v", err)
	}
	if c1 != c2 {
		t.Fatal("同配置应复用同一连接实例")
	}

	// 配置不同（name 变）即新 server：新连接。
	c3, err := pool.Get(context.Background(), rs.poolConfig("sim"))
	if err != nil {
		t.Fatalf("异配置 Get 失败: %v", err)
	}
	if c3 == c1 {
		t.Fatal("不同配置不应复用连接")
	}
}

func TestPoolConfigKeyNormalizesEnv(t *testing.T) {
	rs := newRestartableServer(t)
	pool := newTestPool(t, &fakeClock{t: time.Now()})

	cfgA := rs.poolConfig("map")
	cfgA.Env = []string{"A=1", "B=2"}
	cfgB := rs.poolConfig("map")
	cfgB.Env = []string{"B=2", "A=1"} // 仅顺序不同

	c1, err := pool.Get(context.Background(), cfgA)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	c2, err := pool.Get(context.Background(), cfgB)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if c1 != c2 {
		t.Fatal("env 顺序不影响 memoize 键")
	}
}

func TestPoolEvictsAndRebuilds(t *testing.T) {
	rs := newRestartableServer(t)
	pool := newTestPool(t, &fakeClock{t: time.Now()})
	cfg := rs.poolConfig("map")

	c1, err := pool.Get(context.Background(), cfg)
	if err != nil {
		t.Fatalf("首次 Get 失败: %v", err)
	}

	// 崩溃后原地拉起：旧连接 ping 失败被剔除，重建一次成功（不进短路）。
	rs.stop()
	rs.restart(t)

	c2, err := pool.Get(context.Background(), cfg)
	if err != nil {
		t.Fatalf("重建 Get 失败: %v", err)
	}
	if c2 == c1 {
		t.Fatal("失效连接应被剔除重建为新实例")
	}
	out, err := c2.CallTool(context.Background(), "ping", "")
	if err != nil || out != "pong" {
		t.Fatalf("重建后调用 = %q, %v; 期望 pong", out, err)
	}
}

func TestPoolBrokenClientRebuild(t *testing.T) {
	rs := newRestartableServer(t)
	pool := newTestPool(t, &fakeClock{t: time.Now()})
	cfg := rs.poolConfig("map")

	c1, err := pool.Get(context.Background(), cfg)
	if err != nil {
		t.Fatalf("首次 Get 失败: %v", err)
	}

	// 调用侧发现连接类错误置 broken；重启后 Get 应剔除重建而非发旧连接。
	rs.stop()
	if _, err := c1.CallTool(context.Background(), "ping", ""); err == nil {
		t.Fatal("server 关停后调用应失败")
	} else if !c1.Broken() {
		t.Fatalf("连接类错误后 c1 应标记 broken，实际错误: %v", err)
	}
	rs.restart(t)

	c2, err := pool.Get(context.Background(), cfg)
	if err != nil {
		t.Fatalf("重建 Get 失败: %v", err)
	}
	if c2 == c1 {
		t.Fatal("broken 连接应被剔除重建")
	}
}

func TestPoolCircuitBreaker(t *testing.T) {
	rs := newRestartableServer(t)
	clock := &fakeClock{t: time.Now()}
	pool := newTestPool(t, clock)
	cfg := rs.poolConfig("map")

	if _, err := pool.Get(context.Background(), cfg); err != nil {
		t.Fatalf("首次 Get 失败: %v", err)
	}

	// 二次失败进短路：旧连接 ping 失败（一次），重建拨号失败（二次）。
	rs.stop()
	_, err := pool.Get(context.Background(), cfg)
	if !errors.Is(err, ErrServerUnavailable) {
		t.Fatalf("重建失败应短路返回 ErrServerUnavailable, 实际: %v", err)
	}

	// 短路窗口内：直接失败（即便 server 已恢复也不试连）。
	rs.restart(t)
	if _, err := pool.Get(context.Background(), cfg); !errors.Is(err, ErrServerUnavailable) {
		t.Fatalf("短路窗口内应直接返回 ErrServerUnavailable, 实际: %v", err)
	}

	// 窗口到点：半开放行一次试连，成功即恢复并正常复用。
	clock.advance(2 * time.Minute)
	c1, err := pool.Get(context.Background(), cfg)
	if err != nil {
		t.Fatalf("半开试连应成功: %v", err)
	}
	c2, err := pool.Get(context.Background(), cfg)
	if err != nil || c2 != c1 {
		t.Fatalf("恢复后应正常复用连接: %v", err)
	}
}

func TestPoolHalfOpenFailureReopens(t *testing.T) {
	rs := newRestartableServer(t)
	clock := &fakeClock{t: time.Now()}
	pool := newTestPool(t, clock)
	cfg := rs.poolConfig("map")

	if _, err := pool.Get(context.Background(), cfg); err != nil {
		t.Fatalf("首次 Get 失败: %v", err)
	}
	rs.stop()
	if _, err := pool.Get(context.Background(), cfg); !errors.Is(err, ErrServerUnavailable) {
		t.Fatalf("应短路, 实际: %v", err)
	}

	// 半开试连仍失败：重新短路整周期。
	clock.advance(2 * time.Minute)
	if _, err := pool.Get(context.Background(), cfg); !errors.Is(err, ErrServerUnavailable) {
		t.Fatalf("半开失败应再次短路, 实际: %v", err)
	}
	// 再进窗口：恢复 server 也要等下个周期。
	rs.restart(t)
	if _, err := pool.Get(context.Background(), cfg); !errors.Is(err, ErrServerUnavailable) {
		t.Fatalf("再短路窗口内应直接失败, 实际: %v", err)
	}
	clock.advance(2 * time.Minute)
	if _, err := pool.Get(context.Background(), cfg); err != nil {
		t.Fatalf("第二个半开周期应恢复: %v", err)
	}
}

func TestPoolFirstConnectFailureNoCircuit(t *testing.T) {
	rs := newRestartableServer(t)
	addr := rs.addr
	rs.stop() // 先有地址但无服务：模拟 server 从未起来

	pool := newTestPool(t, &fakeClock{t: time.Now()})
	cfg := rs.poolConfig("map")
	cfg.Endpoint = "http://" + addr + "/mcp"

	// 首次连接失败：返回拨号错误本身，不短路（每次 Get 都允许重试）。
	_, err := pool.Get(context.Background(), cfg)
	if err == nil {
		t.Fatal("无 server 时首次 Get 应失败")
	}
	if errors.Is(err, ErrServerUnavailable) {
		t.Fatalf("首次连接失败不应短路, 实际: %v", err)
	}

	// server 起来后立即可连。
	rs.restart(t)
	if _, err := pool.Get(context.Background(), cfg); err != nil {
		t.Fatalf("server 恢复后 Get 应成功: %v", err)
	}
}
