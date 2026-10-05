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
	"time"

	"insightos.cn/semantic-framework/internal/tool"
)

// 目录条目来源（Entry.SourceKind）取值。本版只有 MCP 一种来源；
// 内置工具目录在 internal/tool.Registry，REST 目录视图按来源分组时合并。
const (
	// SourceKindMCP 条目来自 MCP server 的 tools/list 发现。
	SourceKindMCP = "mcp"
)

// 条目健康状态（Entry.Health）取值（架构 16 §4.3 降级语义）。
const (
	// HealthHealthy server 可达且最近一次对账成功，工具可注入可调用。
	HealthHealthy = "healthy"

	// HealthUnavailable server 失联（连接/对账失败），工具不注入、
	// 调用返回结构化不可用错误；条目保留（warning 不清仓，见 doc.go 第 4 点）。
	HealthUnavailable = "unavailable"
)

// defaultRisk 是 server 未配置 risk 覆盖时的条目风险等级：
// MCP 工具默认按中风险对待（读产物同级），写操作类工具应由
// mcp_servers[].risk 显式提级（high 触发 L4 人工审批）。
const defaultRisk = tool.RiskMedium

// defaultSchemaJSON 是 server 未提供 inputSchema 时的兜底参数契约
// （空 object：jsonschema 编译与 eino 适配都要求合法 schema 文本）。
const defaultSchemaJSON = `{"type":"object"}`

// Entry 是 MCP 工具目录条目——目录层的强类型契约 v1。
// 为什么不用 map[string]any：见 doc.go 第 1 点（agent_ori 猜谜教训）。
type Entry struct {
	// FullName 工具全名：<server>.<tool>（如 test.echo）。
	// 经 kernel.SafeToolName 净化后进模型（test_echo）。
	FullName string

	// Server 来源 server 名，即工具的命名空间（角色过滤/审批名单维度）。
	Server string

	// Tool server 侧原工具名（调用时发往 server 的名字，不带前缀）。
	Tool string

	// Description 工具描述（client 侧已按 2048 rune 截断）；
	// server 未提供时回退为 FullName（模型侧必须有可读描述）。
	Description string

	// SchemaJSON 参数 jsonschema 文本（object 根）；
	// server 未提供 inputSchema 时为 defaultSchemaJSON。
	SchemaJSON string

	// Risk 风险等级（annotations.risk）：server 配置覆盖值，缺省 medium。
	Risk string

	// SourceKind 条目来源，恒为 SourceKindMCP。
	SourceKind string

	// Health 健康状态（HealthHealthy | HealthUnavailable）。
	Health string

	// UpdatedAt 条目内容（描述/schema/风险）最后一次变化的时间（UTC）。
	UpdatedAt time.Time
}

// Definition 把目录条目投影为工具体系的契约（internal/tool.Definition）：
// Agent 工具链适配（kernel.AdaptMCPTools）与安全门禁（L2 参数校验、
// L4 审批判断）共用同一份投影，保证模型视图与门禁清单一致。
func (e Entry) Definition() tool.Definition {
	return tool.Definition{
		Name:           e.FullName,
		Namespace:      e.Server,
		Description:    e.Description,
		ParametersJSON: e.SchemaJSON,
		Annotations:    tool.Annotations{Risk: e.Risk},
	}
}
