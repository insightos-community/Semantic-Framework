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

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/llm"
	"insightos.cn/semantic-framework/pkg/log"
)

// defaultAgentName 是未指定名称时的 Agent 默认名。
const defaultAgentName = "semantic-agent"

// AgentConfig 定义 BuildAgent 的输入：按角色 profile 展开的单 agent 编排参数。
type AgentConfig struct {
	// Name Agent 名（角色名），用于链路/计量归因；空时使用 defaultAgentName。
	Name string

	// Role 角色的交互模式（coordinator 等），计量归因用。
	Role string

	// Description 能力描述（供上级 Agent 判断分派，单 agent 场景仅作元信息）。
	Description string

	// Instruction AGENT.md 渲染正文：系统提示 S1 的主体，
	// 由 context middleware 组装注入（不经 adk Instruction 字段，职责见 middleware.go）。
	Instruction string

	// SafetyDoc 全局 SAFETY 总则正文；非空时附加到系统提示尾部。
	SafetyDoc string

	// Model 内核模型（由 NewChatModel 构建）。
	Model model.BaseChatModel

	// ModelName 归因模型名（回调未携带模型名时 TraceHandler 用它兜底）。
	ModelName string

	// Tools 本轮可用的工具清单（模型侧直接可见）。启用 ToolSearch 时本
	// 清单只含 pinned 常驻工具（与 DynamicTools 互斥切分，见下）。
	Tools []tool.BaseTool

	// ToolSearch 启用 ToolSearch 动态检索（middleware 栈 toolsearch 位，
	// 架构文档 05 §6）：DynamicTools 初始对模型隐藏，经 tool_search 元
	// 工具检索后逐轮累积可见。false 时 DynamicTools 应为空（动态集已
	// 经 Tools 全量注入——显式开关判定与切分在 runtime.buildToolchain）。
	ToolSearch bool

	// DynamicTools ToolSearch 的可检索工具集（非 pinned 的命名空间工具）：
	// 不经 ToolsConfig 注入，由 toolsearch middleware 挂进 tools 节点并
	// 管理可见性；与 Tools 互斥（同一工具不能同时在两处，否则 tools
	// 节点重复）。ToolSearch 为 false 或本清单为空时不挂接 toolsearch
	// middleware（nil/关闭跳过）。
	DynamicTools []tool.BaseTool

	// Safety 工具调用安全门禁（middleware 栈 safety 位，由 internal/security
	// 实现）；nil 时不挂接安全管线（工具调用直接放行，仅测试路径）。
	Safety ToolCallGuard

	// ToolPolicy 是运行目的相关的第二层工具门禁。它对内置工具、MCP 工具
	// 以及 Filesystem Middleware 注入的只读工具全部生效。Planning Run
	// 使用 fail-closed allowlist，不能只依赖模型侧工具可见性。
	ToolPolicy ToolCallGuard

	// ReturnDirectly 中的工具完成后直接结束当前 ReAct Run。它只用于像
	// robot.run 这样“accepted 后职责已经完成”的工具，避免为展示性尾句再次
	// 调用模型；物理终态仍由 Robot Execution 事件驱动。
	ReturnDirectly map[string]bool

	// MaxTurns ReAct 最大轮次（≤0 时用内核默认值 20）。
	MaxTurns int

	// ContextTokens 上下文 token 预算：summarization middleware 的触发阈值
	// 取其 60%；≤0 时不启用历史压缩。
	ContextTokens int

	// Store 元数据存储：非空时 Runner 为每次 Run 动态创建 TraceHandler，
	// 使缓存的 Agent 装配不携带任何 Run/Trace 状态。
	Store *store.Store

	// SkillStore 技能存储读取端口（middleware 栈 skill 位的技能源，
	// 由 internal/skill 实现）；nil 或快照为空时不挂接 skill middleware
	// （无技能形态，与存量行为一致）。
	SkillStore SkillStore

	// WorkspaceRoot 当前会话 Project 的宿主工作区根目录；非空时挂接 Eino
	// Filesystem Middleware，并仅向模型暴露 /workspace 虚拟根下的只读工具。
	WorkspaceRoot string

	// AgentToolInput 表示该 Agent 作为短时 AgentTool 运行：输入使用
	// {task, artifact_refs}，并在模型调用前转换为隔离任务与多模态内容。
	AgentToolInput bool

	// Price 端点单价，计量成本估算用。
	Price llm.Price

	// Purpose 计量用途（如 chat / task）。
	Purpose string

	// Logger 结构化日志器；Store 非空时必填（TraceHandler 依赖）。
	Logger *log.Logger
}

// BuildAgent 用 adk.ChatModelAgent + adk.Runner 构建单 agent 编排：
// 按 middleware 栈统一顺序挂接（见 middleware.go），返回 kernel 封装的
// 运行器（eino 类型不外泄，调用方只面向 Runner/Message/Event）。
func BuildAgent(ctx context.Context, cfg AgentConfig) (*Runner, error) {
	agent, err := buildChatAgent(ctx, cfg)
	if err != nil {
		return nil, err
	}

	// EnableStreaming：对话通道需要流式增量下行（message.delta）。
	// CheckPointStore：挂接元数据存储时启用断点持久化——危险工具中断
	// （安全门禁 L4 审批）把断点随 run_sessions 存，应答后经 Resume 恢复。
	runnerCfg := adk.RunnerConfig{Agent: agent, EnableStreaming: true}
	if cfg.Store != nil {
		runnerCfg.CheckPointStore = newCheckPointStore(cfg.Store)
	}
	runner := adk.NewRunner(ctx, runnerCfg)
	return &Runner{
		inner: runner, name: agentName(cfg.Name),
		traceConfig:   newRunnerTraceConfig(cfg),
		subAgentTools: agentToolMapping(cfg.Tools),
	}, nil
}

// agentName 返回配置的 Agent 名，空时用内核默认名。
func agentName(name string) string {
	if name == "" {
		return defaultAgentName
	}
	return name
}

// buildChatAgent 按配置构建挂接好 middleware 栈的可复用 ChatModelAgent。
//
// TraceHandler 不在这里创建：同一 Runner 会服务多次 Run，处理器必须在
// Runner.Run 时按 Run 动态创建，不能被缓存进 Agent。
func buildChatAgent(ctx context.Context, cfg AgentConfig) (adk.Agent, error) {
	if cfg.Model == nil {
		return nil, fmt.Errorf("AgentConfig.Model 不能为空")
	}
	if cfg.Store != nil && cfg.Logger == nil {
		return nil, fmt.Errorf("AgentConfig.Store 非空时 Logger 必填（TraceHandler 依赖）")
	}

	stack, err := buildMiddlewareStack(ctx, cfg)
	if err != nil {
		return nil, err
	}

	maxTurns := cfg.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 20 // adk 默认值，显式写出避免依赖上游默认变更
	}
	returnDirectly := make(map[string]bool, len(cfg.ReturnDirectly))
	for name, enabled := range cfg.ReturnDirectly {
		if enabled {
			// ToolsConfig看到的是提供给模型的安全工具名；运行配置使用的是
			// 产品契约名。若这里不统一，robot.run会变成robot_run，物理执行
			// 已accepted后Agent仍会再次调用模型，既浪费时间又可能重复动作。
			returnDirectly[SafeToolName(name)] = true
		}
	}
	toolsCfg := adk.ToolsConfig{
		ToolsNodeConfig: compose.ToolsNodeConfig{Tools: cfg.Tools},
		ReturnDirectly:  returnDirectly,
	}
	// 工具集含委派工具时开启子 agent 事件冒泡（EmitInternalEvents）：adk 把
	// 父 flow 的事件 generator 作为 tool option 传给委派工具，子 agent 事件
	// 随之转发进父事件流（eino agent_tool.go/chatmodel.go），kernel 事件层
	// 转换为 EventSubAgent*（见 run.go）。该开关只被 agent-tool 消费（普通
	// 工具不读这个 option），含委派工具时开启是唯一合理形态，故按内容自动
	// 开启而不增配置面。
	if hasAgentTool(cfg.Tools) {
		toolsCfg.EmitInternalEvents = true
	}
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:          agentName(cfg.Name),
		Description:   cfg.Description,
		Model:         cfg.Model,
		ToolsConfig:   toolsCfg,
		MaxIterations: maxTurns,
		Handlers:      stack,
		// Instruction 留空：S1 系统提示由 context middleware 组装注入。
	})
	if err != nil {
		return nil, fmt.Errorf("构建 ChatModelAgent 失败: %w", err)
	}
	return agent, nil
}
