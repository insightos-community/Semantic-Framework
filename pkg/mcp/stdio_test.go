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
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// stdioServerPath 是编译好的 stdio test server 路径（TestMain 构建一次，
// 全测试共享；go 工具链缺失或编译失败时为空，相关测试跳过）。
var stdioServerPath string

// TestMain 构建 testdata/stdio_server 到进程级临时目录（t.TempDir 随单个
// 测试结束即删，不能承载跨测试共享的二进制）。
func TestMain(m *testing.M) {
	code := func() int {
		if _, err := exec.LookPath("go"); err != nil {
			return m.Run() // 无 go 工具链：stdio 测试走 Skip
		}
		dir, err := os.MkdirTemp("", "mcp-stdio-test-server")
		if err != nil {
			return m.Run()
		}
		defer os.RemoveAll(dir)
		out := filepath.Join(dir, "stdio-test-server")
		cmd := exec.Command("go", "build", "-o", out, "./testdata/stdio_server")
		if data, err := cmd.CombinedOutput(); err != nil {
			// 编译失败同样降级为 Skip（输出留给排查）。
			println("stdio test server 编译失败:", err.Error(), string(data))
			return m.Run()
		}
		stdioServerPath = out
		return m.Run()
	}()
	os.Exit(code)
}

// stdioServerBin 返回编译好的 stdio test server 路径；不可用时跳过测试。
func stdioServerBin(t *testing.T) string {
	t.Helper()
	if stdioServerPath == "" {
		t.Skip("stdio test server 不可用（无 go 工具链或编译失败），跳过")
	}
	return stdioServerPath
}

func TestStdioTransport(t *testing.T) {
	bin := stdioServerBin(t)

	client, err := Connect(context.Background(), ServerConfig{
		Name:      "stdio-test",
		Transport: TransportStdio,
		Command:   bin,
	})
	if err != nil {
		t.Fatalf("stdio Connect 失败: %v", err)
	}

	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("stdio ListTools 失败: %v", err)
	}
	names := make(map[string]bool, len(tools))
	for _, ti := range tools {
		names[ti.Name] = true
	}
	if !names["ping"] || !names["getenv"] {
		t.Fatalf("工具清单 = %+v, 期望含 ping/getenv", tools)
	}

	out, err := client.CallTool(context.Background(), "ping", `{"text":"hello"}`)
	if err != nil {
		t.Fatalf("stdio CallTool 失败: %v", err)
	}
	if out != "pong: hello" {
		t.Fatalf("输出 = %q, 期望 %q", out, "pong: hello")
	}

	// Close 关闭 stdin 后子进程应自然退出（不残留孤儿进程）。
	client.Close()
}

func TestStdioEnvPassThrough(t *testing.T) {
	bin := stdioServerBin(t)

	// Env 以 "K=V" 形式追加到子进程环境：子进程的 getenv 工具读回验证。
	client, err := Connect(context.Background(), ServerConfig{
		Name:      "stdio-env",
		Transport: TransportStdio,
		Command:   bin,
		Env:       []string{"SEMANTIC_MCP_TEST_MARKER=r15"},
	})
	if err != nil {
		t.Fatalf("stdio Connect 失败: %v", err)
	}
	defer client.Close()

	out, err := client.CallTool(context.Background(), "getenv", `{"text":"SEMANTIC_MCP_TEST_MARKER"}`)
	if err != nil {
		t.Fatalf("getenv 调用失败: %v", err)
	}
	if out != "r15" {
		t.Fatalf("子进程读到的 SEMANTIC_MCP_TEST_MARKER = %q, 期望 r15", out)
	}
}
