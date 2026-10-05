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
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/store"
	tooldef "insightos.cn/semantic-framework/internal/tool"
)

// AgentToolTaskParam 是委派工具的任务参数名：对外入参契约为 {task: string}
// （与安全门禁登记的参数 schema 是同一份协议，见 internal/agent/subagent）。
const AgentToolTaskParam = "task"

// AgentToolArtifactRefsParam 是委派工具可选的 ArtifactRef 参数名。
const AgentToolArtifactRefsParam = "artifact_refs"

// AgentToolDef 是 SubAgent 包装为委派工具时的对外呈现与运行约束。
type AgentToolDef struct {
	// ToolName 工具名（leader 模型侧函数名；profile 加载期已校验合法性）。
	ToolName string

	// ToolDescription 工具描述（leader 模型判断委派时机的依据）。
	ToolDescription string

	// Timeout 单次委派执行硬上限（工具 annotation.Timeout 语义）；≤0 不强制。
	Timeout time.Duration
}

// agentToolMarker 是委派工具的标记接口：构建期嵌套防御（depth ≤1）与
// 父 agent 事件冒泡（EmitInternalEvents）自动开启都靠它识别委派工具。
// 方法非导出：只有本包能实现该接口，外部无法伪造标记。
type agentToolMarker interface {
	isAgentTool()
}

// BuildAgentTool 把按 cfg 构建的 SubAgent 包装为 leader 可调用的委派工具
// （架构文档 04 §7.3 的 agent-as-tool 机制）：
//
//   - 上下文隔离默认：SubAgent 每次委派是一次独立 run，输入只有任务文本
//     和本轮明确共享的图片 ArtifactRef（不继承 leader 历史），其最终回复
//     文本作为工具结果回传 leader——leader 上下文不被委派过程污染；
//   - 事件冒泡：委派执行中的子 agent 事件经 eino EmitInternalEvents 转发进
//     父 agent 事件流（父 agent 工具集含委派工具时 buildChatAgent 自动开启，
//     见 agent.go），kernel 事件层转换为 EventSubAgent*（见 run.go）；
//   - 嵌套防御：cfg.Tools 不允许再含委派工具（depth ≤1）——嵌套委派让故障
//     定位、预算归因与中断恢复的组合复杂度爆炸，v1 禁止；
//   - 失败归一：委派执行失败/超时转换为结构化错误结果（与工具执行器同
//     语义），不拖垮 leader 的 run；中断错误原样上抛（内核靠错误类型识别，
//     转换会破坏跨 agent 边界的审批链路）。
//
// 这里仍由 adk.NewAgentTool 承担实际执行、事件冒泡和中断传播，只把输入
// schema 扩展为 {task, artifact_refs}。Eino 会把自定义 schema 的原始 JSON
// 作为子 Agent 用户消息，因此专用输入中间件负责还原任务文本，并把已通过
// 当前用户消息归属校验的图片按需读取为多模态内容。
func BuildAgentTool(ctx context.Context, cfg AgentConfig, def AgentToolDef) (tool.BaseTool, error) {
	if def.ToolName == "" || def.ToolDescription == "" {
		return nil, fmt.Errorf("AgentToolDef 的 ToolName/ToolDescription 必填（agent %q）", agentName(cfg.Name))
	}
	for _, t := range cfg.Tools {
		if _, ok := t.(agentToolMarker); ok {
			return nil, fmt.Errorf("SubAgent %q 的工具集不允许包含委派工具（委派深度 ≤1）", agentName(cfg.Name))
		}
	}
	cfg.AgentToolInput = true
	agent, err := buildChatAgent(ctx, cfg)
	if err != nil {
		return nil, err
	}
	inner, ok := adk.NewAgentTool(ctx, agent,
		adk.WithAgentInputSchema(agentToolInputSchema())).(tool.InvokableTool)
	if !ok {
		// 防御性分支：eino 的 agent-tool 必然实现 InvokableTool（同步调用形态）。
		return nil, fmt.Errorf("SubAgent %q 的委派工具不支持同步调用", agentName(cfg.Name))
	}
	return &agentTool{
		inner:     inner,
		def:       def,
		agentName: agentName(cfg.Name),
		store:     cfg.Store,
	}, nil
}

// agentTool 是委派工具的内核适配层：对外 {task, artifact_refs} 契约、
// Artifact 白名单、超时强制和失败归一；执行仍委托给 Eino AgentTool。
type agentTool struct {
	// inner 是使用自定义输入 schema 的 Eino AgentTool。
	inner tool.InvokableTool

	// def 对外呈现与运行约束。
	def AgentToolDef

	// agentName SubAgent 名（成员实例 ID）：父事件流里冒泡事件的归因基准
	// （AgentName 匹配），runner 据此建立 工具名→SubAgent 的事件映射。
	agentName string

	// store 用于校验显式 ArtifactRef 的用户归属。当前消息附件仍走 context
	// 白名单；用户在后续消息中明确给出既有 Artifact ID 时则按 OwnerID 校验。
	store *store.Store
}

// isAgentTool 实现 agentToolMarker（嵌套防御与冒泡开启的识别标记）。
func (t *agentTool) isAgentTool() {}

// hasAgentTool 报告工具清单是否含委派工具（buildChatAgent 自动开启
// EmitInternalEvents 的判定输入）。
func hasAgentTool(tools []tool.BaseTool) bool {
	for _, t := range tools {
		if _, ok := t.(agentToolMarker); ok {
			return true
		}
	}
	return false
}

// agentToolMapping 建立 委派工具名 → SubAgent 名 的映射：runner 事件层据此
// 把委派工具的结果事件归因到来源成员（工具名 ≠ Agent 名，如 ask_query→query-1）。
func agentToolMapping(tools []tool.BaseTool) map[string]string {
	var mapping map[string]string
	for _, t := range tools {
		if at, ok := t.(*agentTool); ok {
			if mapping == nil {
				mapping = make(map[string]string)
			}
			mapping[at.def.ToolName] = at.agentName
		}
	}
	return mapping
}

// Info 返回模型侧工具契约：task 必填，artifact_refs 为可选图片引用数组。
func (t *agentTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name:        t.def.ToolName,
		Desc:        t.def.ToolDescription,
		ParamsOneOf: agentToolInputSchema(),
	}, nil
}

func agentToolInputSchema() *schema.ParamsOneOf {
	return schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
		AgentToolTaskParam: {
			Type: schema.String, Desc: "委派给该助手的任务描述（一次性说清背景与期望产物）",
			Required: true,
		},
		AgentToolArtifactRefsParam: {
			Type: schema.Array, ElemInfo: &schema.ParameterInfo{Type: schema.String},
			Desc: "需要共享给该助手的图片 ArtifactRef；必须属于当前用户",
		},
	})
}

// delegationArgs 是委派工具的结构化入参。
type delegationArgs struct {
	// Task 委派任务文本。
	Task string `json:"task"`

	// ArtifactRefs 是显式选择的图片引用；为空时自动继承当前用户消息图片。
	ArtifactRefs []string `json:"artifact_refs,omitempty"`
}

// InvokableRun 执行一次委派：校验任务与 Artifact 白名单后委托 Eino
// AgentTool。超时硬上限（annotation.Timeout 语义）经 goroutine+select 强制——与工具
// 执行器同语义：超时立即返回结构化 TIMEOUT，不尊重 ctx 的实现也拖不住
// 调用方（迟到的结果写入缓冲 channel，goroutine 不泄漏）。
//
// Go error 仅两类（与执行器约定一致）：运行级取消（父 ctx 取消/超时）与
// 中断错误（审批链路，原样上抛）；委派自身的失败一律结构化（模型可读、可换方案）。
func (t *agentTool) InvokableRun(ctx context.Context, argumentsInJSON string,
	opts ...tool.Option) (string, error) {

	var args delegationArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return tooldef.ErrorResult(tooldef.CodeToolError,
			fmt.Sprintf("委派参数不是合法 JSON: %v", err), false), nil
	}
	if strings.TrimSpace(args.Task) == "" {
		return tooldef.ErrorResult(tooldef.CodeToolError, "委派任务 task 不能为空", false), nil
	}
	allowedRefs := runArtifactRefs(ctx)
	if len(args.ArtifactRefs) == 0 {
		args.ArtifactRefs = allowedRefs
	} else if invalid := t.invalidArtifactRefs(ctx, args.ArtifactRefs, allowedRefs); len(invalid) > 0 {
		return tooldef.ErrorResult(tooldef.CodeToolError,
			fmt.Sprintf("ArtifactRef 不存在或不属于当前用户: %s", strings.Join(invalid, ", ")), false), nil
	}
	// 使用 Eino 自定义 AgentTool schema：原始 JSON 由子 Agent 输入中间件转换，
	// 既保留官方事件冒泡/中断机制，也不把 Leader 私有历史整体传入。
	innerArgs, err := json.Marshal(args)
	if err != nil {
		return tooldef.ErrorResult(tooldef.CodeToolError, fmt.Sprintf("委派参数转换失败: %v", err), false), nil
	}

	callCtx := ctx
	cancel := context.CancelFunc(func() {})
	if t.def.Timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, t.def.Timeout)
	}
	defer cancel()

	type outcome struct {
		out string
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		out, runErr := t.inner.InvokableRun(callCtx, string(innerArgs), opts...)
		ch <- outcome{out, runErr}
	}()

	select {
	case r := <-ch:
		return t.normalizeOutcome(ctx, r.out, r.err)
	case <-callCtx.Done():
		if ctx.Err() != nil {
			// 父 ctx 取消/超时：运行级取消，作为 Go 错误传播。
			return "", ctx.Err()
		}
		return tooldef.ErrorResult(tooldef.CodeTimeout,
			fmt.Sprintf("委派执行超过 %s 硬上限", t.def.Timeout), true), nil
	}
}

// invalidArtifactRefs 校验显式图片引用：当前消息附件已经由 HTTP/WS 上传链路
// 完成归属验证，可直接放行；其他引用必须能在 Store 中查到且 OwnerID 与
// 当前 Run 用户一致。这样既支持用户在后续轮次引用既有 Artifact ID，也不
// 允许模型猜测其他用户的 ID 越权读取。
func (t *agentTool) invalidArtifactRefs(ctx context.Context, refs, allowed []string) []string {
	set := make(map[string]struct{}, len(allowed))
	for _, ref := range allowed {
		set[ref] = struct{}{}
	}
	scope, hasScope := tooldef.ExecutionScopeFromContext(ctx)
	var invalid []string
	for _, ref := range refs {
		if _, ok := set[ref]; !ok {
			if !hasScope || t.store == nil {
				invalid = append(invalid, ref)
				continue
			}
			artifact, err := t.store.GetArtifactMeta(ref)
			if err != nil || artifact.OwnerID == "" || artifact.OwnerID != scope.OwnerID {
				invalid = append(invalid, ref)
			}
		}
	}
	return invalid
}

// normalizeOutcome 把委派执行的返回归一为工具结果：中断与运行级取消原样
// 上抛，自身超时/失败转结构化错误结果。
func (t *agentTool) normalizeOutcome(ctx context.Context, out string, err error) (string, error) {
	if err == nil {
		// Eino AgentTool 返回的是 SubAgent 最终 Assistant Content 原文。
		// 对于把思考嵌入 <think> 标签的兼容服务，这里必须只把可见正文
		// 交给 Leader；推理过程已由冒泡 reasoning.delta 独立展示和归档。
		return visibleModelContent(out), nil
	}
	// 中断错误（含子 agent 审批的 CompositeInterrupt）原样上抛：
	// 内核靠错误类型识别中断，转换会破坏跨 agent 边界的恢复链路。
	if _, ok := compose.IsInterruptRerunError(err); ok {
		return "", err
	}
	if errors.Is(err, context.Canceled) {
		return "", err
	}
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		// 自身超时（父 ctx 仍存活）：结构化 TIMEOUT。
		return tooldef.ErrorResult(tooldef.CodeTimeout,
			fmt.Sprintf("委派执行超过 %s 硬上限", t.def.Timeout), true), nil
	}
	if ctx.Err() != nil {
		return "", err
	}
	return tooldef.ErrorResult(tooldef.CodeToolError, err.Error(), false), nil
}
