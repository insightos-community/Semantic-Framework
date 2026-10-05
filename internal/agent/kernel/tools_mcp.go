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
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	einojsonschema "github.com/eino-contrib/jsonschema"

	"insightos.cn/semantic-framework/internal/mcpregistry"
	tooldef "insightos.cn/semantic-framework/internal/tool"
)

// 本文件是 MCP 工具目录（internal/mcpregistry）与内核（eino）之间的
// 适配层：目录 Entry 适配为 eino tool.BaseTool，净化/描述/schema 契约
// 与内置工具适配器（tools.go）完全同源——模型看到的 MCP 工具与内置
// 工具形态一致，安全门禁（safety middleware）按原名无差别生效。

// MCPCaller 是 MCP 工具调用的执行后端（internal/mcpregistry.Registry
// 实现）：把调用路由到对应 server 的连接池执行，返回结构化结果文本
// （{"ok":...} 信封）；Go error 仅表示父 ctx 取消/超时（运行级取消，
// 向上传播）——与内置工具执行器的出口约定一致。
type MCPCaller interface {
	// CallTool 调用 server 侧工具，argsJSON 为参数 JSON 文本。
	CallTool(ctx context.Context, server, toolName, argsJSON string) (string, error)
}

// AdaptMCPTools 把 MCP 目录条目适配为内核工具清单（eino tool.BaseTool）：
// 契约取自 Entry.Definition()（与安全门禁共用同一份投影），参数 schema
// 预解析（非法在适配期报错），执行委托 caller（不经内置工具执行器——
// MCP 工具不走本地 registry，超时由 client 单次操作超时承载）。
//
// 净化契约与 AdaptTools 一致：模型侧名 = SafeToolName(FullName)
// （test.echo → test_echo）；净化后重名属于契约冲突，适配期直接报错。
func AdaptMCPTools(entries []mcpregistry.Entry, caller MCPCaller) ([]Tool, error) {
	tools := make([]Tool, 0, len(entries))
	seen := make(map[string]string, len(entries)) // 净化名 → 原始名（冲突检测）
	for _, entry := range entries {
		def := entry.Definition()
		safe := SafeToolName(def.Name)
		if prev, ok := seen[safe]; ok && prev != def.Name {
			return nil, fmt.Errorf("工具名净化后冲突: %q 与 %q 都映射为 %q", prev, def.Name, safe)
		}
		seen[safe] = def.Name
		adapter, err := newMCPToolAdapter(entry, caller)
		if err != nil {
			return nil, err
		}
		tools = append(tools, adapter)
	}
	return tools, nil
}

// mcpToolAdapter 是把 MCP 目录条目适配为 eino InvokableTool 的桥
// （Info 做 schema 转换，InvokableRun 经 caller 路由到 server）。
type mcpToolAdapter struct {
	// def 工具契约（Entry.Definition() 投影，与门禁清单一致）。
	def tooldef.Definition

	// schema 解析后的参数 schema（Info 直接复用）。
	schema *einojsonschema.Schema

	// server 来源 server 名（调用路由键）。
	server string

	// toolName server 侧原工具名（发往 server 的名字）。
	toolName string

	// caller 执行后端（目录的连接池出口）。
	caller MCPCaller
}

// newMCPToolAdapter 创建单个 MCP 工具的适配器：预解析参数 schema，非法即报错。
func newMCPToolAdapter(entry mcpregistry.Entry, caller MCPCaller) (*mcpToolAdapter, error) {
	def := entry.Definition()
	js, err := parseToolSchema(def)
	if err != nil {
		return nil, err
	}
	return &mcpToolAdapter{
		def: def, schema: js,
		server: entry.Server, toolName: entry.Tool,
		caller: caller,
	}, nil
}

// fullName 返回工具的原始全名（toolNamed 契约，供 safety 映射净化名）。
func (a *mcpToolAdapter) fullName() string {
	return a.def.Name
}

// Info 返回内核工具信息（与内置工具同一净化与描述契约）。
func (a *mcpToolAdapter) Info(_ context.Context) (*schema.ToolInfo, error) {
	return toolInfo(a.def, a.schema), nil
}

// InvokableRun 执行一次工具调用：经 caller 路由到来源 server 执行
// （参数 JSON 原样透传，L2 校验是安全门禁的职责，适配层不重复——
// 与 toolAdapter 同一纪律）。
func (a *mcpToolAdapter) InvokableRun(ctx context.Context, argumentsInJSON string,
	_ ...tool.Option) (string, error) {
	return a.caller.CallTool(ctx, a.server, a.toolName, argumentsInJSON)
}
