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

package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/mcpregistry"
	"insightos.cn/semantic-framework/internal/security"
	"insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/mcp"
)

// 本文件验证 MCP 工具经 buildToolchain 注入的两条核心语义（R16 交付清单）：
// 命名空间过滤正确、MCP 工具经安全门禁 L2 校验（与内置工具同权）。

// newEchoMCPRegistry 起一个真实 MCP server（echo 工具，text 必填 string），
// 经连接池对账进目录，返回目录（调用方无清理负担，随 httptest 回收）。
func newEchoMCPRegistry(t *testing.T) *mcpregistry.Registry {
	t.Helper()
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "runtime-test-server", Version: "v0.0.1"}, nil)
	sdkmcp.AddTool(srv, &sdkmcp.Tool{Name: "echo", Description: "回显输入文本"},
		func(_ context.Context, _ *sdkmcp.CallToolRequest, in struct {
			Text string `json:"text" jsonschema:"要回显的文本"`
		}) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "echo: " + in.Text}},
			}, nil, nil
		})
	httpSrv := httptest.NewServer(sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil))
	t.Cleanup(httpSrv.Close)

	pool := mcp.NewPool(nil, nil)
	t.Cleanup(pool.Close)
	reg := mcpregistry.NewRegistry(pool, nil)
	_, err := reg.SyncServer(context.Background(), mcpregistry.ServerSpec{
		Config: mcp.ServerConfig{Name: "test", Transport: mcp.TransportHTTP, Endpoint: httpSrv.URL},
	})
	if err != nil {
		t.Fatalf("目录对账失败: %v", err)
	}
	return reg
}

// mcpToolService 构造只含 MCP 目录的服务（buildToolchain 的最小依赖）。
func mcpToolService(t *testing.T, reg *mcpregistry.Registry) *Service {
	t.Helper()
	fx := newTestFixture(t)
	return NewService(Deps{Logger: fx.logger, MCPRegistry: reg})
}

// TestBuildToolchainMCPNamespaceFilter 验证 MCP 条目的命名空间过滤：
// 命中的注入、未命中的不注入、unavailable 不注入。
func TestBuildToolchainMCPNamespaceFilter(t *testing.T) {
	reg := newEchoMCPRegistry(t)
	svc := mcpToolService(t, reg)

	// ① 命中 test.*：注入 test_echo（净化名），门禁清单含 test.echo。
	tools, _, safety, err := svc.buildToolchain(&profile.Profile{
		Name: "leader", Tools: profile.ToolsConfig{Namespaces: []string{"test.*"}},
	}, "leader")
	if err != nil {
		t.Fatalf("buildToolchain 失败: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("应注入 1 个 MCP 工具，实际: %d", len(tools))
	}
	info, err := tools[0].Info(context.Background())
	if err != nil {
		t.Fatalf("Info 失败: %v", err)
	}
	if info.Name != "test_echo" {
		t.Errorf("注入工具应为净化名 test_echo，实际: %q", info.Name)
	}
	if safety == nil {
		t.Fatal("命中工具时门禁应非空")
	}

	// ② 未命中命名空间：不注入、门禁为 nil。
	tools, _, safety, err = svc.buildToolchain(&profile.Profile{
		Name: "leader", Tools: profile.ToolsConfig{Namespaces: []string{"other.*"}},
	}, "leader")
	if err != nil {
		t.Fatalf("buildToolchain 失败: %v", err)
	}
	if len(tools) != 0 || safety != nil {
		t.Errorf("未命中命名空间应无工具无门禁，实际: tools=%d safety=%v", len(tools), safety)
	}

	// ③ server 失联：unavailable 条目不注入。
	reg.MarkUnavailable("test")
	tools, _, _, err = svc.buildToolchain(&profile.Profile{
		Name: "leader", Tools: profile.ToolsConfig{Namespaces: []string{"test.*"}},
	}, "leader")
	if err != nil {
		t.Fatalf("buildToolchain 失败: %v", err)
	}
	if len(tools) != 0 {
		t.Errorf("unavailable 条目不应注入，实际: %d", len(tools))
	}
}

// TestBuildToolchainMCPSafetyGate 验证 MCP 工具经安全门禁 L2 校验：
// 参数不满足 schema 被拦（PARAM_VIOLATION 结构化结果，不执行），
// 合法参数放行到达 caller。
func TestBuildToolchainMCPSafetyGate(t *testing.T) {
	reg := newEchoMCPRegistry(t)
	svc := mcpToolService(t, reg)

	_, _, guard, err := svc.buildToolchain(&profile.Profile{
		Name: "leader", Tools: profile.ToolsConfig{Namespaces: []string{"test.*"}},
	}, "leader")
	if err != nil {
		t.Fatalf("buildToolchain 失败: %v", err)
	}
	mw, ok := guard.(*security.Middleware)
	if !ok {
		t.Fatalf("门禁应为 *security.Middleware，实际: %T", guard)
	}

	// ① L2 拦截：text 应为 string，给 number → PARAM_VIOLATION，next 不执行。
	nextCalled := false
	next := func(context.Context, string) (string, error) {
		nextCalled = true
		return "", nil
	}
	out, err := mw.WrapToolCall(context.Background(),
		kernel.ToolCallMeta{Name: "test.echo", CallID: "c1"}, `{"text":123}`, next)
	if err != nil {
		t.Fatalf("L2 拦截不应返回 Go error: %v", err)
	}
	if nextCalled {
		t.Error("L2 拦截时不应执行工具")
	}
	if !strings.Contains(out, `"ok":false`) || !strings.Contains(out, "PARAM_VIOLATION") {
		t.Errorf("L2 拦截应归一 PARAM_VIOLATION，实际: %s", out)
	}

	// ② 合法参数放行：经 caller 真实到达 server 执行（risk=medium 免审批）。
	out, err = mw.WrapToolCall(context.Background(),
		kernel.ToolCallMeta{Name: "test.echo", CallID: "c2"}, `{"text":"hi"}`,
		func(ctx context.Context, args string) (string, error) {
			return reg.CallTool(ctx, "test", "echo", args)
		})
	if err != nil {
		t.Fatalf("放行路径失败: %v", err)
	}
	var envelope struct {
		OK   bool   `json:"ok"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil || !envelope.OK || envelope.Data != "echo: hi" {
		t.Errorf("放行结果应为 OK 信封且真实执行，实际: %s（err=%v）", out, err)
	}
}

// TestBuildToolchainMCPNameConflict 验证内置与 MCP 工具同名在构建期报错
// （安全门禁清单按名索引，静默覆盖会绕过审批名单）。
func TestBuildToolchainMCPNameConflict(t *testing.T) {
	reg := newEchoMCPRegistry(t)
	fx := newTestFixture(t)

	// 内置注册表注册同名工具 test.echo（如 server 命名为内置命名空间）。
	builtinReg := tool.NewRegistry()
	if err := builtinReg.Register(&stubTool{def: tool.Definition{
		Name: "test.echo", Namespace: "test", Description: "内置同名工具",
		ParametersJSON: `{"type":"object"}`,
	}}); err != nil {
		t.Fatalf("注册内置工具失败: %v", err)
	}
	svc := NewService(Deps{
		Logger: fx.logger, Registry: builtinReg, Executor: tool.NewExecutor(builtinReg, tool.ExecutorOptions{}),
		MCPRegistry: reg,
	})

	_, _, _, err := svc.buildToolchain(&profile.Profile{
		Name: "leader", Tools: profile.ToolsConfig{Namespaces: []string{"test.*"}},
	}, "leader")
	if err == nil || !strings.Contains(err.Error(), "工具名冲突") {
		t.Fatalf("同名冲突应报错，实际: %v", err)
	}
}

// stubTool 是最小 tool.Tool 实现（冲突测试用，Run 不会到达）。
type stubTool struct {
	def tool.Definition
}

// Def 返回工具契约。
func (s *stubTool) Def() tool.Definition { return s.def }

// Run 不应被调用（冲突在构建期暴露）。
func (s *stubTool) Run(context.Context, string) (string, error) { return "", nil }
