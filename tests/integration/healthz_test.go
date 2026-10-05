package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/bootstrap"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/log"
	"insightos.cn/semantic-framework/pkg/version"
)

// freeAddr 申请一个空闲端口后立即释放，返回可用于测试服务的监听地址。
// 存在轻微的端口竞争窗口，但对本地集成测试足够可靠。
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请空闲端口失败: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("释放临时端口失败: %v", err)
	}
	return addr
}

// profilesDirAbs 返回仓库根下 configs/agents 的绝对路径（集成测试从
// tests/integration 子目录运行，profile 加载需要绝对路径）。
func profilesDirAbs(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "configs", "agents"))
	if err != nil {
		t.Fatalf("解析 profile 目录失败: %v", err)
	}
	return abs
}

// startApp 在真实端口上启动装配后的服务（HTTP + WS 双监听、临时库、
// 固定测试口令），返回两个 baseURL、App 句柄与停止函数。
// 通过轮询 healthz 等待服务就绪，避免睡眠带来的不稳定。
func startApp(t *testing.T) (httpBase, wsBase string, app *bootstrap.App, stop func()) {
	t.Helper()
	t.Setenv("SEMANTIC_ADMIN_PASSWORD", "test-admin-pass")

	cfg := config.Default()
	cfg.Server.HTTPAddr = freeAddr(t)
	cfg.Server.WSAddr = freeAddr(t)
	cfg.Store.SQLitePath = filepath.Join(t.TempDir(), "test.db")
	storetest.SeedMigratedAt(t, cfg.Store.SQLitePath)
	cfg.LLM.Default = "mock" // 无 key 环境：默认模型走 mock 驱动
	cfg.Agents.ProfilesDir = profilesDirAbs(t)
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})

	app, err := bootstrap.Wire(cfg, logger)
	if err != nil {
		t.Fatalf("Wire 装配失败: %v", err)
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
			t.Error("App.Run 在 ctx 取消后 5s 内未退出")
		}
	}
	return httpBase, wsBase, app, stop
}

// getJSON 发起 GET 请求并解析 JSON 响应体。
func getJSON(t *testing.T, url string) (int, map[string]string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s 失败: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析 %s 响应体失败: %v", url, err)
	}
	return resp.StatusCode, body
}

// TestHealthz 验证 /api/v1/system/healthz 返回 200 与 {"status":"ok"}。
func TestHealthz(t *testing.T) {
	httpBase, _, _, stop := startApp(t)
	defer stop()

	status, body := getJSON(t, httpBase+"/api/v1/system/healthz")
	if status != http.StatusOK {
		t.Errorf("healthz 状态码应为 200，实际: %d", status)
	}
	if body["status"] != "ok" {
		t.Errorf("healthz 响应体应为 {\"status\":\"ok\"}，实际: %v", body)
	}
}

// TestVersion 验证 /api/v1/system/version 返回 200 与当前版本号。
func TestVersion(t *testing.T) {
	httpBase, _, _, stop := startApp(t)
	defer stop()

	status, body := getJSON(t, httpBase+"/api/v1/system/version")
	if status != http.StatusOK {
		t.Errorf("version 状态码应为 200，实际: %d", status)
	}
	if body["version"] != version.String() {
		t.Errorf("version 响应体应包含版本号 %q，实际: %v", version.String(), body)
	}
}
