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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// echoArgs 是 echo 工具的入参（schema 由 SDK 按结构体推断）。
type echoArgs struct {
	Text string `json:"text" jsonschema:"要回显的文本"`
}

// newTestMCPServer 起一个真实 streamable HTTP MCP server（SDK server 端），
// 注册三个工具：echo（正常）、fail（执行报错）、verbose（超长描述）。
// 返回 server 端 SDK 对象（测试可热加工具触发 list-changed）与底层
// httptest server（测试可关停模拟故障）；endpoint 为其 URL。
func newTestMCPServer(t *testing.T) (*sdkmcp.Server, *httptest.Server) {
	t.Helper()

	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "test-mcp-server", Version: "v0.0.1"}, nil)

	sdkmcp.AddTool(srv, &sdkmcp.Tool{Name: "echo", Description: "回显输入文本"},
		func(_ context.Context, _ *sdkmcp.CallToolRequest, in echoArgs) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "echo: " + in.Text}},
			}, nil, nil
		})

	sdkmcp.AddTool(srv, &sdkmcp.Tool{Name: "fail", Description: "总是执行失败"},
		func(_ context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, any, error) {
			return nil, nil, errors.New("boom")
		})

	// 多字节字符验证按 rune 截断（3000 个汉字 = 9000 字节）。
	sdkmcp.AddTool(srv, &sdkmcp.Tool{Name: "verbose", Description: strings.Repeat("文", 3000)},
		func(_ context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "ok"}},
			}, nil, nil
		})

	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)
	return srv, httpSrv
}

// connectTestServer 连接到测试 server。
func connectTestServer(t *testing.T, endpoint string) *Client {
	t.Helper()
	client, err := Connect(context.Background(), ServerConfig{
		Name:      "test",
		Transport: TransportHTTP,
		Endpoint:  endpoint,
	})
	if err != nil {
		t.Fatalf("Connect 失败: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

func TestListToolsTruncatesDescription(t *testing.T) {
	_, httpSrv := newTestMCPServer(t)
	client := connectTestServer(t, httpSrv.URL)

	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools 失败: %v", err)
	}
	byName := make(map[string]ToolInfo, len(tools))
	for _, ti := range tools {
		byName[ti.Name] = ti
	}
	if len(byName) != 3 {
		t.Fatalf("工具数 = %d, 期望 3", len(byName))
	}

	// verbose：3000 rune 截断到 2048（按字符而非字节）。
	if got := len([]rune(byName["verbose"].Description)); got != maxDescriptionRunes {
		t.Fatalf("verbose 描述长度 = %d rune, 期望 %d", got, maxDescriptionRunes)
	}
	// echo：短描述原样保留。
	if byName["echo"].Description != "回显输入文本" {
		t.Fatalf("echo 描述 = %q, 期望原样保留", byName["echo"].Description)
	}
	// echo：schema 非空且是合法 JSON，含推断出的 text 属性。
	var schema map[string]any
	if err := json.Unmarshal([]byte(byName["echo"].InputSchemaJSON), &schema); err != nil {
		t.Fatalf("echo InputSchemaJSON 不是合法 JSON: %v", err)
	}
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["text"]; !ok {
		t.Fatalf("echo schema 缺少 text 属性: %s", byName["echo"].InputSchemaJSON)
	}
}

func TestCallTool(t *testing.T) {
	_, httpSrv := newTestMCPServer(t)
	client := connectTestServer(t, httpSrv.URL)

	out, err := client.CallTool(context.Background(), "echo", `{"text":"hello"}`)
	if err != nil {
		t.Fatalf("CallTool 失败: %v", err)
	}
	if out != "echo: hello" {
		t.Fatalf("输出 = %q, 期望 %q", out, "echo: hello")
	}
	if client.Broken() {
		t.Fatal("正常调用后连接不应标记 broken")
	}
}

func TestCallToolErrors(t *testing.T) {
	_, httpSrv := newTestMCPServer(t)
	client := connectTestServer(t, httpSrv.URL)

	// 工具自身报错（IsError）：错误文本透出，连接不置 broken。
	if _, err := client.CallTool(context.Background(), "fail", ""); err == nil {
		t.Fatal("fail 工具应返回错误")
	} else if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("错误 = %q, 期望含 server 给出的 boom", err)
	}
	if client.Broken() {
		t.Fatal("工具错误不应标记连接 broken")
	}

	// 未知工具：协议错误，连接不置 broken。
	if _, err := client.CallTool(context.Background(), "nosuch", ""); err == nil {
		t.Fatal("未知工具应返回错误")
	}
	if client.Broken() {
		t.Fatal("协议错误不应标记连接 broken")
	}

	// 非法参数 JSON：本地报错（不经 server）。
	if _, err := client.CallTool(context.Background(), "echo", `{bad json`); err == nil {
		t.Fatal("非法 argsJSON 应返回错误")
	} else if !strings.Contains(err.Error(), "不是合法 JSON") {
		t.Fatalf("错误 = %q, 期望提示非法 JSON", err)
	}
}

func TestCallToolConnErrorMarksBroken(t *testing.T) {
	// 关停后 SDK 可能返回 connection refused / connection closed / 被 %v
	// 包成文本的 EOF，分类必须覆盖这些形态。多轮是为了在 -race 下碰到各分支。
	for i := 0; i < 20; i++ {
		t.Run(fmt.Sprintf("round-%d", i), func(t *testing.T) {
			// 用裸 http.Server 而非 httptest：http.Server.Close 不等待在途请求
			// （httptest.Close 会被 SDK 服务端 SSE 挂起响应阻塞），可模拟进程猝死。
			rs := newRestartableServer(t)
			client := connectTestServer(t, rs.endpoint())
			rs.stop()
			if _, err := client.CallTool(context.Background(), "ping", ""); err == nil {
				t.Fatal("server 关停后调用应失败")
			} else if !client.Broken() {
				t.Fatalf("连接类错误后应标记 broken，实际错误: %v", err)
			}
		})
	}
}

func TestOnToolsChanged(t *testing.T) {
	srv, httpSrv := newTestMCPServer(t)
	client := connectTestServer(t, httpSrv.URL)

	fired := make(chan string, 16)
	client.OnToolsChanged(func() {
		select {
		case fired <- "changed":
		default:
		}
	})

	// 独立 SSE 推送通道建立有先后：循环加热加工具直至收到通知（去测试抖动）。
	deadline := time.Now().Add(10 * time.Second)
	for {
		sdkmcp.AddTool(srv, &sdkmcp.Tool{Name: "late", Description: "后加工具"},
			func(_ context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, any, error) {
				return &sdkmcp.CallToolResult{
					Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "late"}},
				}, nil, nil
			})
		select {
		case <-fired:
			return // 收到 tools/list_changed，桥接生效
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("10s 内未收到 tools/list_changed 通知")
		}
		time.Sleep(200 * time.Millisecond)
		srv.RemoveTools("late")
	}
}

func TestConnectValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  ServerConfig
	}{
		{"缺少 name", ServerConfig{Transport: TransportHTTP, Endpoint: "http://x"}},
		{"未知传输", ServerConfig{Name: "x", Transport: "ws", Endpoint: "http://x"}},
		{"http 缺 endpoint", ServerConfig{Name: "x", Transport: TransportHTTP}},
		{"stdio 缺 command", ServerConfig{Name: "x", Transport: TransportStdio}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Connect(context.Background(), tc.cfg); err == nil {
				t.Fatal("应返回校验错误")
			}
		})
	}
}
