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

package security

import (
	"context"
	"encoding/gob"
	"fmt"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/log"
)

// 安全管线错误码取值（结构化工具结果的 code 字段，线上协议的一部分）。
const (
	// CodeParamViolation L2 参数校验未通过（模型可修正参数后重试）。
	CodeParamViolation = "PARAM_VIOLATION"

	// CodeApprovalRejected L4 审批未通过（拒绝或超时按拒绝）。
	CodeApprovalRejected = "APPROVAL_REJECTED"

	// CodeUnknownTool 工具不在门禁清单（防御性分支，正常链路不可达）。
	CodeUnknownTool = "UNKNOWN_TOOL"
)

// ApprovalInfo 是 L4 审批中断的负载：随内核中断传递到运行时，
// 运行时据此发起交互请求（interaction.request）。
// 为什么负载是平铺字段而不是引用交互记录：中断发生时交互尚未创建
// （先持久化断点再下行请求，一致性顺序见 internal/interaction），
// 中断负载必须自包含审批所需的全部信息。
type ApprovalInfo struct {
	// Agent 发起审批的 Agent 名（成员实例 ID，如 query-1）：中断经
	// CompositeInterrupt 跨 agent 边界冒泡后，运行时的审批归因
	// （interaction.request 的 agent 字段）以此为准——发起方身份是
	// 审批信息的必要部分，而中断链上下文（InterruptCtx）不携带它。
	Agent string

	// Tool 待审批的工具全名。
	Tool string

	// Namespace 工具命名空间。
	Namespace string

	// Risk 风险等级（annotations.risk）。
	Risk string

	// Question 给用户的确认问题。
	Question string

	// ArgsJSON 本次调用的参数 JSON（审批载荷的一部分，供用户研判）。
	ArgsJSON string
}

func init() {
	// 中断负载会进内核 checkpoint（gob 编码），自定义类型必须注册。
	gob.Register(ApprovalInfo{})
}

// Middleware 是 safety middleware v1（kernel middleware 形态，挂接在
// middleware 栈 safety 位）：每次工具调用先经 L2 参数校验与 L4 审批判断。
// 实现 kernel.ToolCallGuard 契约——eino 类型不进本包（ACL 纪律），
// 中断/恢复原语经 kernel 转发。
type Middleware struct {
	// defs 门禁清单（按工具全名索引，与注入 agent 的工具一致）。
	defs map[string]tool.Definition

	// schemas 编译后的参数 jsonschema（L2 校验用，构建期编译）。
	schemas map[string]*jsonschema.Schema

	// approvalRequired 需人工审批的命名空间模式（profile.interrupt 配置）。
	approvalRequired []string

	// agent 门禁所属 Agent 的身份（成员实例 ID，如 query-1）：L4 审批
	// 中断负载的归因（ApprovalInfo.Agent）。同一角色 profile 可装配多个
	// 成员实例，归因以实例为准而非角色名。
	agent string

	// logger 结构化日志器。
	logger *log.Logger
}

// NewMiddleware 创建 safety middleware：编译全部工具参数 schema
// （非法 schema 启动期报错，不带病上线）。agent 是门禁所属 Agent 的
// 成员实例 ID（审批中断的归因身份，见 ApprovalInfo.Agent）。
func NewMiddleware(defs []tool.Definition, approvalRequired []string, agent string, logger *log.Logger) (*Middleware, error) {
	m := &Middleware{
		defs:             make(map[string]tool.Definition, len(defs)),
		schemas:          make(map[string]*jsonschema.Schema, len(defs)),
		approvalRequired: approvalRequired,
		agent:            agent,
		logger:           logger,
	}
	for _, def := range defs {
		sch, err := compileSchema(def.Name, def.ParametersJSON)
		if err != nil {
			return nil, err
		}
		m.defs[def.Name] = def
		m.schemas[def.Name] = sch
	}
	return m, nil
}

// compileSchema 把工具的参数 jsonschema 文本编译为校验器。
func compileSchema(name, schemaText string) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(schemaText))
	if err != nil {
		return nil, fmt.Errorf("工具 %q 的参数 schema 不是合法 JSON: %w", name, err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("tool.json", doc); err != nil {
		return nil, fmt.Errorf("工具 %q 的参数 schema 加载失败: %w", name, err)
	}
	sch, err := c.Compile("tool.json")
	if err != nil {
		return nil, fmt.Errorf("工具 %q 的参数 schema 编译失败: %w", name, err)
	}
	return sch, nil
}

// WrapToolCall 实现 kernel.ToolCallGuard：单次工具调用的安全序列——
// L2 参数校验 → L4 审批判断 → 放行执行。
// 拒绝（L2/L4）返回结构化工具结果（模型可读、可修正），不作为 Go 错误；
// 中断（L4 审批）返回的内核中断错误必须原样上抛（内核靠错误类型识别）。
func (m *Middleware) WrapToolCall(ctx context.Context, meta kernel.ToolCallMeta, argsJSON string,
	next kernel.ToolCallEndpoint) (string, error) {

	def, ok := m.defs[meta.Name]
	if !ok {
		// 防御性分支：adk 只允许调用注入清单内的工具，门禁清单与之一致，
		// 不一致说明装配缺漏——安全兜底按拒绝处理而不是放行。
		m.logger.Warn("工具不在门禁清单，按拒绝处理", "tool", meta.Name, "call_id", meta.CallID)
		return tool.ErrorResult(CodeUnknownTool, fmt.Sprintf("工具 %q 未登记", meta.Name), false), nil
	}

	// L2 参数校验：jsonschema 结构校验（类型/必填/枚举）。
	checkStart := time.Now()
	validationErr := m.validate(meta.Name, argsJSON)
	m.logger.Info("agent.contract.tool_check", "tool", meta.Name, "call_id", meta.CallID,
		"duration_ns", time.Since(checkStart).Nanoseconds(), "valid", validationErr == nil)
	if err := validationErr; err != nil {
		m.logger.Info("工具调用被参数校验拦截",
			"tool", meta.Name, "call_id", meta.CallID, "error", err.Error())
		return tool.ErrorResult(CodeParamViolation, err.Error(), false), nil
	}

	// L4 审批判断：risk ≥ high 或命名空间命中审批名单。
	if m.needsApproval(ctx, def) {
		return m.gateWithApproval(ctx, def, meta, argsJSON, next)
	}

	m.logger.Info("工具调用放行", "tool", meta.Name, "call_id", meta.CallID)
	return next(ctx, argsJSON)
}

// validate 执行 L2 结构校验：参数必须是合法 JSON 且满足工具 schema。
func (m *Middleware) validate(name, argsJSON string) error {
	if argsJSON == "" {
		argsJSON = "{}"
	}
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(argsJSON))
	if err != nil {
		return fmt.Errorf("参数不是合法 JSON: %w", err)
	}
	if err := m.schemas[name].Validate(inst); err != nil {
		return fmt.Errorf("参数不满足工具 %q 的 schema: %w", name, err)
	}
	return nil
}

// needsApproval 判断工具是否需要人工审批（09 §4：risk ≥ high 或命中审批名单）。
func (m *Middleware) needsApproval(ctx context.Context, def tool.Definition) bool {
	// 执行模式只调整两个显式执行工具，不能借 full 绕过 Artifact 或未来
	// 设备工具的独立安全规则。
	if scope, ok := tool.ExecutionScopeFromContext(ctx); ok {
		switch def.Name {
		case tool.ExecuteToolName:
			return scope.ExecutionMode != store.ExecutionModeAuto &&
				scope.ExecutionMode != store.ExecutionModeFull
		case tool.ExecuteHostToolName:
			return scope.ExecutionMode != store.ExecutionModeFull
		case "robot.stop":
			// 安全停止不是扩大物理操作范围：工具层仍会校验当前 Project 与明确的
			// execution_id，最终 stopped 也必须等待 Pilot/Robot SDK 的 hold 证据。
			// 如果在停止前再次等待人工审批，反而会延长活动物理动作的风险窗口。
			return false
		case "robot.run":
			// 用户批准的是 Workflow 中 Robot SubTask 的 Skill 范围，而不是每一次
			// 相同的 robot.run。这里只识别调度器注入的完整 Task Execution 边界；
			// robot.run 自身还会以持久化 Task/SubTask 数据精确校验 Robot、Skill
			// 名称和版本。普通对话、调试 Run 或缺失任一归属字段时仍必须审批。
			return scope.RunKind != store.RunKindTaskExecution ||
				scope.WorkflowID == "" || scope.TaskID == "" ||
				scope.SubtaskID == "" || scope.RobotID == ""
		}
	}
	if def.Annotations.Risk == tool.RiskHigh || def.Annotations.Risk == tool.RiskCritical {
		return true
	}
	return tool.NamespaceMatch(m.approvalRequired, def.Name)
}

// gateWithApproval 处理需审批的工具调用：
// 首次执行发起内核中断（暂停 run，负载自包含审批信息）；
// 恢复执行时按注入的审批结果决定放行/结构化拒绝。
func (m *Middleware) gateWithApproval(ctx context.Context, def tool.Definition, meta kernel.ToolCallMeta,
	argsJSON string, next kernel.ToolCallEndpoint) (string, error) {

	if !kernel.ToolCallWasInterrupted(ctx) {
		m.logger.Info("危险工具调用，发起审批中断",
			"tool", def.Name, "call_id", meta.CallID, "risk", def.Annotations.Risk)
		return "", kernel.InterruptToolCall(ctx, ApprovalInfo{
			Agent:     m.agent,
			Tool:      def.Name,
			Namespace: def.Namespace,
			Risk:      def.Annotations.Risk,
			Question: fmt.Sprintf("是否批准执行 %s（风险等级：%s）？",
				def.Name, def.Annotations.Risk),
			ArgsJSON: argsJSON,
		})
	}

	isTarget, hasData, approved := kernel.ToolCallResumed[bool](ctx)
	if !isTarget || !hasData {
		// 非本次恢复目标（同轮并行调用的其他工具被恢复）：保持中断等待。
		return "", kernel.InterruptToolCall(ctx, nil)
	}
	if !approved {
		m.logger.Info("审批未通过，取消工具调用", "tool", def.Name, "call_id", meta.CallID)
		return tool.ErrorResult(CodeApprovalRejected,
			fmt.Sprintf("操作未获批准（拒绝或超时），%s 未执行", def.Name), false), nil
	}

	m.logger.Info("审批通过，放行工具调用", "tool", def.Name, "call_id", meta.CallID)
	return next(ctx, argsJSON)
}
