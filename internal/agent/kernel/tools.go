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
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	einojsonschema "github.com/eino-contrib/jsonschema"

	tooldef "insightos.cn/semantic-framework/internal/tool"
)

// 本文件是工具体系（internal/tool）与内核（eino）之间的适配层：
// 我们的 tool.Tool 适配为 eino tool.BaseTool（schema 转换），
// 安全门禁（ToolCallGuard）与中断原语以 kernel 自有类型暴露，
// eino 类型不外泄（ACL 纪律，见 doc.go）。

// ToolCallMeta 是一次工具调用的元数据（安全门禁的判定输入）。
type ToolCallMeta struct {
	// Name 被调用的工具全名。
	Name string

	// CallID 本次调用的唯一标识（模型侧 tool_call_id）。
	CallID string
}

// ToolCallEndpoint 是被安全门禁包装的工具执行端点。
type ToolCallEndpoint func(ctx context.Context, argsJSON string) (string, error)

// ToolCallGuard 是工具调用的安全门禁契约（middleware 栈 safety 位，
// 由 internal/security 实现）：每次工具调用先经门禁，放行才执行。
// 实现方可返回结构化拒绝结果（作为工具结果返回给模型），
// 或用 InterruptToolCall 发起中断（暂停 run 等待人工审批）。
type ToolCallGuard interface {
	// WrapToolCall 包装一次工具调用：meta 为调用元数据，argsJSON 为参数
	// JSON 文本，next 为放行后的执行端点。
	WrapToolCall(ctx context.Context, meta ToolCallMeta, argsJSON string, next ToolCallEndpoint) (string, error)
}

// ModelRoundGuard is optional on ToolPolicy. It runs once before each logical
// model exchange, after the preceding tool batch has finished. Network retries
// of that same exchange are not new feedback rounds. It must never replay tools.
type ModelRoundGuard interface {
	BeforeModelRound(context.Context) error
}

// InterruptToolCall 在工具调用路径上发起中断：暂停当前 run 并把断点
// 持久化（Run 需携带 checkpoint ID），等待外部输入后经 Runner.Resume 恢复。
// info 是随中断传递的负载（如安全审批请求），会进 checkpoint（gob 编码，
// 自定义类型必须先 gob.Register）。
// 返回的错误必须原样返回给调用方，不得包装（内核靠错误类型识别中断）。
func InterruptToolCall(ctx context.Context, info any) error {
	return tool.Interrupt(ctx, info)
}

// ToolCallWasInterrupted 报告当前工具调用点是否曾中断过
// （区分首次执行与恢复执行：恢复时同一段代码会重跑）。
func ToolCallWasInterrupted(ctx context.Context) bool {
	wasInterrupted, _, _ := tool.GetInterruptState[any](ctx)
	return wasInterrupted
}

// ToolCallResumed 检查当前工具调用点是否作为恢复目标被重新执行，
// 并取回恢复数据（Runner.Resume 时按中断点 ID 注入）。
func ToolCallResumed[T any](ctx context.Context) (isTarget bool, hasData bool, data T) {
	return tool.GetResumeContext[T](ctx)
}

// AdaptTools 把工具契约清单适配为内核工具清单（eino tool.BaseTool）：
// 参数 jsonschema 文本转换为内核 ToolInfo，执行委托给执行器
// （超时/结构化错误在执行器内归一）。schema 非法在适配期报错（启动期暴露）。
//
// 模型侧工具名经 SafeToolName 净化（真实端点如 OpenAI/deepseek 要求函数名
// 匹配 ^[a-zA-Z0-9_-]+$）；净化后重名属于契约冲突，适配期直接报错。
func AdaptTools(defs []tooldef.Definition, executor *tooldef.Executor) ([]Tool, error) {
	tools := make([]Tool, 0, len(defs))
	seen := make(map[string]string, len(defs)) // 净化名 → 原始名（冲突检测）
	for _, def := range defs {
		safe := SafeToolName(def.Name)
		if prev, ok := seen[safe]; ok && prev != def.Name {
			return nil, fmt.Errorf("工具名净化后冲突: %q 与 %q 都映射为 %q", prev, def.Name, safe)
		}
		seen[safe] = def.Name
		adapter, err := newToolAdapter(def, executor)
		if err != nil {
			return nil, err
		}
		tools = append(tools, adapter)
	}
	return tools, nil
}

// SafeToolName 把领域工具名转换为模型端点可接受的合法函数名：
// 非 [a-zA-Z0-9_-] 字符统一替换为 '_'（如 artifact.put → artifact_put）。
// 为什么需要净化：OpenAI 兼容端点（deepseek 等）对 function.name 有严格
// 正则约束 ^[a-zA-Z0-9_-]+$，含 '.' 的名字会被 400 拒绝；净化是单向的，
// 还原由调用方持映射表完成（见 safetyMiddleware 与适配器 def.Name）。
func SafeToolName(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, name)
}

// toolNamed 是内置/MCP 两个工具适配器共用的契约名出口：
// middleware 栈 safety 位据此构建 净化名→原始名 映射（见 middleware.go）。
type toolNamed interface {
	// fullName 返回工具的原始全名（净化前）。
	fullName() string
}

// toolAdapter 是把我们的 tool.Tool 契约与执行器适配为 eino InvokableTool
// 的桥（Info 做 schema 转换，InvokableRun 委托执行器）。
type toolAdapter struct {
	// def 工具契约（名称/描述/参数 schema 文本）。
	def tooldef.Definition

	// schema 解析后的参数 schema（Info 直接复用，避免每次调用重解析）。
	schema *einojsonschema.Schema

	// executor 执行器（超时/结构化错误归一）。
	executor *tooldef.Executor
}

// newToolAdapter 创建单个工具的适配器：预解析参数 schema，非法即报错。
func newToolAdapter(def tooldef.Definition, executor *tooldef.Executor) (*toolAdapter, error) {
	js, err := parseToolSchema(def)
	if err != nil {
		return nil, err
	}
	return &toolAdapter{def: def, schema: js, executor: executor}, nil
}

// fullName 返回工具的原始全名（toolNamed 契约）。
func (a *toolAdapter) fullName() string {
	return a.def.Name
}

// Info 返回内核工具信息（模型侧看到的名称/描述/参数 schema）。
func (a *toolAdapter) Info(_ context.Context) (*schema.ToolInfo, error) {
	return toolInfo(a.def, a.schema), nil
}

// parseToolSchema 把工具契约的参数 jsonschema 文本预解析为内核 schema，
// 非法即报错（适配期暴露，不带病上线）。内置与 MCP 适配器共用同一份解析，
// 保证模型侧参数契约唯一（"复用 AdaptTools 的安全契约"的落点之一）。
func parseToolSchema(def tooldef.Definition) (*einojsonschema.Schema, error) {
	js := &einojsonschema.Schema{}
	if err := json.Unmarshal([]byte(def.ParametersJSON), js); err != nil {
		return nil, fmt.Errorf("工具 %q 的参数 schema 非法: %w", def.Name, err)
	}
	return js, nil
}

// toolInfo 组装模型侧工具信息：Name 必须是净化后的合法函数名（端点正则
// 约束）；描述前缀保留原始全名，便于模型把提示词中的命名空间
// （如 artifact.*）与函数名对应起来。两个适配器共用（净化契约唯一）。
func toolInfo(def tooldef.Definition, js *einojsonschema.Schema) *schema.ToolInfo {
	return &schema.ToolInfo{
		Name:        SafeToolName(def.Name),
		Desc:        fmt.Sprintf("[%s] %s", def.Name, def.Description),
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(js),
	}
}

// InvokableRun 执行一次工具调用：直接委托执行器（参数 JSON 原样透传，
// 校验是安全门禁 L2 的职责，适配层不重复）。
func (a *toolAdapter) InvokableRun(ctx context.Context, argumentsInJSON string,
	_ ...tool.Option) (string, error) {
	return a.executor.ExecuteOne(ctx, a.def.Name, argumentsInJSON)
}
