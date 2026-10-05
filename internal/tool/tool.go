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

package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// 风险等级取值（annotations.risk，架构文档 05 §3），安全管线据此决定审批级别。
const (
	// RiskLow 低风险（只读/无副作用）。
	RiskLow = "low"

	// RiskMedium 中风险（读产物内容等）。
	RiskMedium = "medium"

	// RiskHigh 高风险（写产物/副作用操作，需人工审批）。
	RiskHigh = "high"

	// RiskCritical 临界风险（最高审批级别，高危物理操作预留）。
	RiskCritical = "critical"
)

// Annotations 是工具的行为注解（架构文档 05 §3 的 annotations 约定）。
type Annotations struct {
	// Risk 风险等级（low/medium/high/critical）；安全管线的审批级别输入。
	Risk string

	// Timeout 单次执行硬上限；≤0 时用执行器默认超时。
	Timeout time.Duration

	// Streaming 长时工具标记（执行中持续上报进度）；v1 工具均为 false。
	Streaming bool

	// Idempotent 幂等标记：安全取消后可直接重试。
	Idempotent bool
}

// Definition 是工具对 Agent 暴露的契约（名称/描述/参数 schema/命名空间）。
type Definition struct {
	// Name 工具全名：<命名空间>.<动作> 两级（如 artifact.put）。
	Name string

	// Namespace 命名空间（如 artifact），权限过滤/审批名单的统一维度。
	Namespace string

	// Description 能力描述（告诉模型何时/为何使用）。
	Description string

	// ParametersJSON 参数的 jsonschema 文本（object 根），模型侧参数契约
	// 与安全管线 L2 校验共用同一份。
	ParametersJSON string

	// Annotations 行为注解。
	Annotations Annotations
}

// Tool 是工具的统一接口：契约定义 + 执行。
// Run 的入参与返回都是 JSON 文本（与模型工具调用协议一致）：
// 成功返回经 OKResult 组装的结果；失败返回 *Error（或普通 error，
// 执行器统一转换为结构化错误）。
type Tool interface {
	// Def 返回工具契约。
	Def() Definition

	// Run 执行一次调用：argsJSON 为参数 JSON 文本，返回结果 JSON 文本。
	Run(ctx context.Context, argsJSON string) (string, error)
}

// Registry 是进程内工具注册表：内置工具启动时注册，并发安全。
type Registry struct {
	// mu 保护 tools。
	mu sync.RWMutex

	// tools 按全名索引的工具。
	tools map[string]Tool
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register 注册一个工具；契约非法（缺名/缺命名空间/缺描述）或重名时报错。
// 启动期注册的失败应直接暴露——契约不完整说明实现缺漏，不静默放过。
func (r *Registry) Register(t Tool) error {
	def := t.Def()
	if def.Name == "" || def.Namespace == "" || def.Description == "" {
		return fmt.Errorf("工具契约不完整（name/namespace/description 必填）: %+v", def)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[def.Name]; exists {
		return fmt.Errorf("工具 %q 重复注册", def.Name)
	}
	r.tools[def.Name] = t
	return nil
}

// Get 按全名查询工具；第二个返回值表示是否存在。
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// List 返回全部工具的契约（按名称排序，输出稳定）。
func (r *Registry) List() []Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]Definition, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, t.Def())
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}

// ByNamespace 返回指定命名空间的工具契约（按名称排序）。
func (r *Registry) ByNamespace(namespace string) []Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var defs []Definition
	for _, t := range r.tools {
		if def := t.Def(); def.Namespace == namespace {
			defs = append(defs, def)
		}
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}

// MatchNamespaces 返回命中命名空间模式的全部工具契约（按名称排序）。
// 模式形如 "artifact.*"（前缀匹配整个命名空间）或精确工具名；
// 空模式列表不匹配任何工具（未声明工具范围的角色拿不到工具）。
func (r *Registry) MatchNamespaces(patterns []string) []Definition {
	var defs []Definition
	for _, def := range r.List() {
		if NamespaceMatch(patterns, def.Name) {
			defs = append(defs, def)
		}
	}
	return defs
}

// NamespaceMatch 判断工具全名是否命中任一命名空间模式
// （"artifact.*" 命中 artifact 命名空间下的全部工具；否则精确匹配）。
func NamespaceMatch(patterns []string, toolName string) bool {
	for _, p := range patterns {
		if len(p) > 1 && p[len(p)-1] == '*' {
			// "artifact.*" → 前缀 "artifact."，命中该命名空间全部工具。
			if prefix := p[:len(p)-1]; len(toolName) >= len(prefix) && toolName[:len(prefix)] == prefix {
				return true
			}
			continue
		}
		if p == toolName {
			return true
		}
	}
	return false
}

// Error 是工具的结构化错误：工具 Run 返回 *Error 时，执行器原样序列化
// 其 code/retryable（普通 error 统一归为 TOOL_ERROR 且不可重试）。
// 为什么错误走类型而不是字符串前缀：Agent 需要稳定的机器可读码
// 来决定重试/换方案（架构文档 05 §3 结果结构化约定）。
type Error struct {
	// Code 机器可读错误码（如 DIVIDE_BY_ZERO / PARAM_VIOLATION）。
	Code string

	// Message 人类可读错误描述（给模型，让它一次修对）。
	Message string

	// Retryable 瞬态错误标记：true 时 Agent 可直接重试。
	Retryable bool
}

// Error 返回错误的文本形态（实现 error 接口）。
func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// resultEnvelope 是工具结果的线上结构（{ok, data?} / {ok, error}）。
type resultEnvelope struct {
	OK    bool       `json:"ok"`
	Data  any        `json:"data,omitempty"`
	Error *errorBody `json:"error,omitempty"`
}

// errorBody 是结构化错误的负载。
type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// OKResult 组装成功结果 JSON：{"ok":true,"data":...}。
// data 必须可 JSON 序列化；序列化失败（实现缺漏）返回错误而不是静默降级。
func OKResult(data any) (string, error) {
	out, err := json.Marshal(resultEnvelope{OK: true, Data: data})
	if err != nil {
		return "", fmt.Errorf("序列化工具结果失败: %w", err)
	}
	return string(out), nil
}

// ErrorResult 组装结构化错误 JSON：{"ok":false,"error":{code,message,retryable}}。
// 执行器与安全管线共用同一出口，保证线上错误格式唯一。
func ErrorResult(code, message string, retryable bool) string {
	out, err := json.Marshal(resultEnvelope{
		OK: false, Error: &errorBody{Code: code, Message: message, Retryable: retryable},
	})
	if err != nil {
		// errorBody 全是字符串/布尔字段，理论不可达；兜底返回最小合法 JSON。
		return `{"ok":false,"error":{"code":"INTERNAL","message":"结果序列化失败","retryable":false}}`
	}
	return string(out)
}

// errorResultPrefix 是结构化错误结果信封的固定前缀：resultEnvelope 的
// ok 字段恒在序列化首位（encoding/json 按字段序输出），失败必为 false。
const errorResultPrefix = `{"ok":false`

// IsErrorResult 报告工具输出是否为结构化错误结果（{"ok":false...} 信封）。
// 失败一律经 ErrorResult 归一（线上格式唯一），消费方（如深度路由的
// "上一轮工具失败"判定）前缀判定即可，无需完整解析大结果。
func IsErrorResult(output string) bool {
	return strings.HasPrefix(strings.TrimSpace(output), errorResultPrefix)
}
