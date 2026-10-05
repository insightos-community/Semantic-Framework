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
	"strings"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/mcpregistry"
)

// fakeMCPCaller 是记录调用参数的 MCPCaller 桩（断言路由：server/tool/args 原样透传）。
type fakeMCPCaller struct {
	// server 收到的 server 名。
	server string

	// toolName 收到的 server 侧工具名。
	toolName string

	// argsJSON 收到的参数 JSON（应原样透传）。
	argsJSON string

	// out 预设返回。
	out string
}

// CallTool 记录参数并返回预设结果。
func (f *fakeMCPCaller) CallTool(_ context.Context, server, toolName, argsJSON string) (string, error) {
	f.server, f.toolName, f.argsJSON = server, toolName, argsJSON
	return f.out, nil
}

// mcpTestEntry 构造测试目录条目。
func mcpTestEntry(fullName, server, toolName string) mcpregistry.Entry {
	return mcpregistry.Entry{
		FullName: fullName, Server: server, Tool: toolName,
		Description: "回显输入文本",
		SchemaJSON:  `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`,
		Risk:        "medium", SourceKind: mcpregistry.SourceKindMCP,
		Health: mcpregistry.HealthHealthy, UpdatedAt: time.Now(),
	}
}

// TestAdaptMCPTools 验证 MCP 条目适配：净化名/描述前缀/schema 与内置
// 适配器同一契约；InvokableRun 经 caller 路由到来源 server。
func TestAdaptMCPTools(t *testing.T) {
	caller := &fakeMCPCaller{out: `{"ok":true,"data":"echo: hi"}`}
	tools, err := AdaptMCPTools([]mcpregistry.Entry{mcpTestEntry("test.echo", "test", "echo")}, caller)
	if err != nil {
		t.Fatalf("AdaptMCPTools 失败: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("应适配 1 个工具，实际: %d", len(tools))
	}

	info, err := tools[0].Info(context.Background())
	if err != nil {
		t.Fatalf("Info 失败: %v", err)
	}
	if info.Name != "test_echo" {
		t.Errorf("模型侧名应为净化名 test_echo，实际: %q", info.Name)
	}
	if !strings.HasPrefix(info.Desc, "[test.echo] ") {
		t.Errorf("描述应带原始全名前缀，实际: %q", info.Desc)
	}
	if info.ParamsOneOf == nil {
		t.Error("参数 schema 应非空")
	}
}

// TestAdaptMCPToolsRun 验证调用路由与结果透传（server/tool/args 原样发往 caller）。
func TestAdaptMCPToolsRun(t *testing.T) {
	caller := &fakeMCPCaller{out: `{"ok":true,"data":"echo: hi"}`}
	tools, err := AdaptMCPTools([]mcpregistry.Entry{mcpTestEntry("test.echo", "test", "echo")}, caller)
	if err != nil {
		t.Fatalf("AdaptMCPTools 失败: %v", err)
	}
	adapter, ok := tools[0].(*mcpToolAdapter)
	if !ok {
		t.Fatalf("适配结果应为 *mcpToolAdapter，实际: %T", tools[0])
	}
	out, err := adapter.InvokableRun(context.Background(), `{"text":"hi"}`)
	if err != nil {
		t.Fatalf("InvokableRun 失败: %v", err)
	}
	if caller.server != "test" || caller.toolName != "echo" || caller.argsJSON != `{"text":"hi"}` {
		t.Errorf("路由参数应原样透传，实际: server=%q tool=%q args=%q",
			caller.server, caller.toolName, caller.argsJSON)
	}
	if out != `{"ok":true,"data":"echo: hi"}` {
		t.Errorf("结果应原样返回，实际: %s", out)
	}
}

// TestAdaptMCPToolsConflict 验证净化后重名在适配期报错（契约冲突不带病上线）。
func TestAdaptMCPToolsConflict(t *testing.T) {
	entries := []mcpregistry.Entry{
		mcpTestEntry("test.echo", "test", "echo"),
		mcpTestEntry("test_echo", "test", "echo2"), // 净化后与 test.echo 同名
	}
	if _, err := AdaptMCPTools(entries, &fakeMCPCaller{}); err == nil ||
		!strings.Contains(err.Error(), "净化后冲突") {
		t.Fatalf("净化后重名应报错，实际: %v", err)
	}
}

// TestAdaptMCPToolsBadSchema 验证非法参数 schema 在适配期报错。
func TestAdaptMCPToolsBadSchema(t *testing.T) {
	entry := mcpTestEntry("test.echo", "test", "echo")
	entry.SchemaJSON = `{not-json`
	if _, err := AdaptMCPTools([]mcpregistry.Entry{entry}, &fakeMCPCaller{}); err == nil {
		t.Fatal("非法 schema 应报错")
	}
}

// TestMCPToolAdapterFullName 验证 toolNamed 契约（safety 净化名映射的输入）。
func TestMCPToolAdapterFullName(t *testing.T) {
	tools, err := AdaptMCPTools([]mcpregistry.Entry{mcpTestEntry("test.echo", "test", "echo")}, &fakeMCPCaller{})
	if err != nil {
		t.Fatalf("AdaptMCPTools 失败: %v", err)
	}
	named, ok := tools[0].(toolNamed)
	if !ok {
		t.Fatalf("MCP 适配器应实现 toolNamed，实际: %T", tools[0])
	}
	if named.fullName() != "test.echo" {
		t.Errorf("fullName 应为原始全名，实际: %q", named.fullName())
	}
}
