package kernel

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/cloudwego/eino/adk"
	einofs "github.com/cloudwego/eino/adk/middlewares/filesystem"
	"github.com/cloudwego/eino/adk/middlewares/patchtoolcalls"
	"github.com/cloudwego/eino/adk/middlewares/reduction"
	"github.com/cloudwego/eino/adk/middlewares/summarization"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/log"
)

// buildMiddlewareStack 按统一顺序装配 Agent 中间件：
//
//	1 patchtoolcalls  补齐中断后悬挂的工具调用
//	2 AgentTool 输入 仅把 SubAgent 的结构化任务转换成独立输入
//	3 skill           按需加载技能说明
//	4 filesystem      只读访问当前 Project 工作区
//	5 toolsearch      按需发现动态工具
//	6 safety          校验工具调用并处理执行确认
//	7 summarization   历史超过阈值时压缩
//	8 reduction       截断或外置过大的工具结果
//	9 context         注入一份角色提示和安全说明
//
// 顺序决定每个中间件看到的数据：历史先修复再压缩，动态加入的工具仍经过
// safety，系统提示最后补到消息头。Trace 不属于该栈；Runner 在每次 Run
// 开始时通过 Eino 回调单独挂载，避免缓存运行器复用 trace_id。
func buildMiddlewareStack(ctx context.Context, cfg AgentConfig) ([]adk.ChatModelAgentMiddleware, error) {

	stack := make([]adk.ChatModelAgentMiddleware, 0, 10)

	// 1. patchtoolcalls：历史中出现无对应工具结果的 tool call 时插入占位工具消息。
	patch, err := patchtoolcalls.New(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("构建 patchtoolcalls middleware 失败: %w", err)
	}
	stack = append(stack, patch)

	// AgentTool 输入适配只挂在子 Agent：Eino 保留自定义 schema 的原始
	// JSON，本中间件把它转换为任务文本和按需读取的图片内容。
	if cfg.AgentToolInput {
		stack = append(stack, &agentToolInputMiddleware{store: cfg.Store})
	}

	// 2. skill（eino）：技能系统渐进披露——清单摘要常驻 skill 工具描述
	// （每次 run 重新渲染，热更下一轮生效），命中后正文 inline 注入。
	// 无技能（store 为 nil 或快照为空）不挂接。
	skillMW, err := buildSkillMiddleware(ctx, cfg.SkillStore, cfg.logger())
	if err != nil {
		return nil, err
	}
	if skillMW != nil {
		stack = append(stack, skillMW)
		cfg.logger().Debug("skill middleware 已启用",
			"agent", cfg.Name, "skills", len(cfg.SkillStore.List()))
	}

	// 4. filesystem（eino）：会话 Agent 以 /workspace 虚拟根读取当前
	// Project。写入和命令执行继续走显式 execute 工具及其审批策略。
	var filesystemMW adk.ChatModelAgentMiddleware
	if cfg.WorkspaceRoot != "" {
		filesystemMW, err = buildFilesystemMiddleware(ctx, cfg.WorkspaceRoot, cfg.logger())
		if err != nil {
			return nil, err
		}
		stack = append(stack, filesystemMW)
	}

	// 5. toolsearch（eino）：动态工具检索——动态集经 middleware 注入 tools
	// 节点（可执行）但首轮起对模型隐藏，模型调 tool_search 元工具检索后
	// 命中工具追加回 ToolInfos（run 内累积，机制与缓存影响见 toolsearch.go）。
	// ToolSearch 关闭或动态集为空时不挂接。
	toolSearchMW, err := buildToolSearchMiddleware(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if toolSearchMW != nil {
		stack = append(stack, toolSearchMW)
		cfg.logger().Debug("toolsearch middleware 已启用",
			"agent", cfg.Name, "dynamic_tools", len(cfg.DynamicTools))
	}

	// 6. run policy：先执行运行目的相关的硬门禁。与 safety 不同，它也覆盖
	// Filesystem Middleware 注入的工具，Planning Run 因此能在执行点再次
	// 拒绝任何未列入只读清单的调用。
	if cfg.ToolPolicy != nil {
		nameMap := make(map[string]string, len(cfg.Tools)+len(cfg.DynamicTools))
		for _, t := range cfg.Tools {
			if a, ok := t.(toolNamed); ok {
				nameMap[SafeToolName(a.fullName())] = a.fullName()
			}
		}
		for _, t := range cfg.DynamicTools {
			if a, ok := t.(toolNamed); ok {
				nameMap[SafeToolName(a.fullName())] = a.fullName()
			}
		}
		stack = append(stack, &toolPolicyMiddleware{guard: cfg.ToolPolicy, nameMap: nameMap})
	}

	// 7. safety：每次工具调用先经安全门禁（L2 参数校验 → L4 审批判断），
	// 放行才进执行器。门禁为 nil 时不挂接（测试路径）。
	if cfg.Safety != nil {
		// 构建 净化名→原始名 映射：供门禁把 tCtx.Name（模型侧净化名）
		// 还原为契约原名（approval_required 命名空间与 risk 注解按原名匹配）。
		// 映射覆盖 Tools 与 DynamicTools：检索后才出现的动态工具调用
		// 同样经门禁，需要同样的原名还原；内置与 MCP 适配器都实现
		// toolNamed，映射对两类工具同源生效。
		nameMap := make(map[string]string, len(cfg.Tools)+len(cfg.DynamicTools))
		for _, t := range cfg.Tools {
			if a, ok := t.(toolNamed); ok {
				nameMap[SafeToolName(a.fullName())] = a.fullName()
			}
		}
		for _, t := range cfg.DynamicTools {
			if a, ok := t.(toolNamed); ok {
				nameMap[SafeToolName(a.fullName())] = a.fullName()
			}
		}
		// 豁免集：middleware 注入的框架内部工具（skill 加载工具——只读技能
		// 正文；tool_search 元工具——只读工具目录——均无注册表契约，L2/L4
		// 无判定依据）；未登记的未知工具仍按 UNKNOWN_TOOL 拒绝（装配缺漏
		// 的安全兜底不削弱）。
		exempt := make(map[string]struct{}, 6)
		if skillMW != nil {
			exempt[skillToolName] = struct{}{}
		}
		if toolSearchMW != nil {
			exempt[toolSearchToolName] = struct{}{}
		}
		if filesystemMW != nil {
			// 这四个工具的只读性与 Project 路径边界由
			// projectFilesystemBackend 保证，因此不需要注册表风险契约。
			for _, name := range []string{einofs.ToolNameLs, einofs.ToolNameReadFile,
				einofs.ToolNameGlob, einofs.ToolNameGrep} {
				exempt[name] = struct{}{}
			}
		}
		stack = append(stack, &safetyMiddleware{guard: cfg.Safety, nameMap: nameMap, exempt: exempt})
	}

	// 7. summarization：历史 token 超过预算 60% 时用当前模型压缩旧历史。
	// ContextTokens <= 0 表示未配置预算，不启用压缩（防御性分支，profile 默认值保证 > 0）。
	if cfg.ContextTokens > 0 {
		threshold := cfg.ContextTokens * 6 / 10
		summaryConfig := &summarization.Config{
			Model:   cfg.Model,
			Trigger: &summarization.TriggerCondition{ContextTokens: threshold},
		}
		// v0.2 只有 Conversation 的 Leader Context 持久保存摘要。短时
		// SubAgent 仍可在单次调用内压缩，但不能覆盖 Leader 的摘要边界。
		if cfg.Store != nil && !cfg.AgentToolInput && (cfg.Purpose == "chat" ||
			cfg.Purpose == store.RunKindTaskPlanning || cfg.Purpose == store.RunKindTaskExecution) {
			summaryConfig.Finalize = persistentSummaryFinalizer(cfg.Store, cfg.logger())
		}
		sum, err := summarization.New(ctx, summaryConfig)
		if err != nil {
			return nil, fmt.Errorf("构建 summarization middleware 失败: %w", err)
		}
		stack = append(stack, sum)
		cfg.logger().Debug("summarization middleware 已启用", "agent", cfg.Name, "threshold", threshold)
	}

	// 8. reduction：上下文瘦身（10 §5）——单条工具结果超 20000 字符截断
	// 外置（全文落 store artifacts，上下文留 引用+预览，artifact.get 可取回），
	// 总 token 估算超预算 60% 时外置旧工具轮次。外置依赖存储，无 Store
	// 不挂接（保留全量结果，与无观测形态一致）。
	if cfg.Store != nil {
		red, err := reduction.New(ctx, &reduction.Config{
			Backend:                 reductionBackend{st: cfg.Store},
			MaxLengthForTrunc:       reductionMaxLengthForTrunc,
			MaxTokensForClear:       int64(cfg.ContextTokens) * 6 / 10,
			ReadFileToolName:        reductionReadFileTool,
			GenTruncOffloadFilePath: genArtifactOffloadPath,
			GenClearOffloadFilePath: genArtifactOffloadPath,
		})
		if err != nil {
			return nil, fmt.Errorf("构建 reduction middleware 失败: %w", err)
		}
		stack = append(stack, red)
		cfg.logger().Debug("reduction middleware 已启用",
			"agent", cfg.Name, "max_length_for_trunc", reductionMaxLengthForTrunc)
	}

	// 系统提示只有一个注入入口：直接使用 Profile 已加载的 AGENT.md 正文。
	stack = append(stack, newContextMiddleware(cfg.Instruction, cfg.SafetyDoc))
	return stack, nil
}

// toolPolicyMiddleware 覆盖全部工具调用，不设置任何隐式豁免。具体策略按
// 原始工具名判断；Filesystem 工具没有名称映射，直接使用 Eino 稳定名称。
type toolPolicyMiddleware struct {
	adk.BaseChatModelAgentMiddleware
	guard   ToolCallGuard
	nameMap map[string]string
}

func (m *toolPolicyMiddleware) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState,
	_ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	if guard, ok := m.guard.(ModelRoundGuard); ok {
		if err := guard.BeforeModelRound(ctx); err != nil {
			return ctx, state, err
		}
	}
	return ctx, state, nil
}

func (m *toolPolicyMiddleware) WrapInvokableToolCall(_ context.Context,
	endpoint adk.InvokableToolCallEndpoint, tCtx *adk.ToolContext) (adk.InvokableToolCallEndpoint, error) {
	name := tCtx.Name
	if original, ok := m.nameMap[name]; ok {
		name = original
	}
	return func(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
		return m.guard.WrapToolCall(ctx, ToolCallMeta{Name: name, CallID: tCtx.CallID}, argsJSON,
			func(ctx context.Context, args string) (string, error) {
				return endpoint(ctx, args, opts...)
			})
	}, nil
}

// safetyMiddleware 是安全门禁的内核适配：把 ToolCallGuard（kernel 自有
// 契约，eino 类型不外泄）桥接到 adk 的工具调用包装钩子。
// 门禁逻辑（L2 校验/审批判断/中断发起）全在实现方（internal/security），
// 本适配器只做端点签名转换与工具名还原，无策略。
type safetyMiddleware struct {
	adk.BaseChatModelAgentMiddleware

	// guard 安全门禁实现。
	guard ToolCallGuard

	// nameMap 净化名 → 原始工具名（如 artifact_put → artifact.put）：
	// tCtx.Name 是模型侧的净化名，安全门禁需要原始名索引契约与命名空间。
	nameMap map[string]string

	// exempt 门禁豁免的工具名集：middleware 注入的框架内部工具（当前仅
	// skill 加载工具——只读技能正文、无注册表契约，L2 无 schema 可校验、
	// L4 无 risk 注解可判定）。仅限装配时显式登记的名字；未登记的未知
	// 工具仍按 UNKNOWN_TOOL 拒绝（装配缺漏的安全兜底不削弱）。
	exempt map[string]struct{}
}

// WrapInvokableToolCall 把门禁包到每次同步工具调用上：
// adk 端点签名（含 tool.Option 变参）转换为门禁的精简签名；
// 豁免集内的框架内部工具直接放行（不包门禁）；其余工具名先经 nameMap
// 还原为原始名再交给门禁（门禁按原始名匹配 approval_required 命名空间
// 与 risk 注解）。
func (m *safetyMiddleware) WrapInvokableToolCall(_ context.Context, endpoint adk.InvokableToolCallEndpoint,
	tCtx *adk.ToolContext) (adk.InvokableToolCallEndpoint, error) {
	if _, ok := m.exempt[tCtx.Name]; ok {
		return endpoint, nil
	}
	name := tCtx.Name
	if orig, ok := m.nameMap[name]; ok {
		name = orig
	}
	return func(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
		return m.guard.WrapToolCall(ctx,
			ToolCallMeta{Name: name, CallID: tCtx.CallID}, argsJSON,
			func(ctx context.Context, args string) (string, error) {
				return endpoint(ctx, args, opts...)
			})
	}, nil
}

// contextMiddleware 是唯一的 Agent 系统提示注入入口。
type contextMiddleware struct {
	adk.BaseChatModelAgentMiddleware
	systemPrompt string
}

func newContextMiddleware(instruction, safetyDoc string) *contextMiddleware {
	prompt := strings.TrimSpace(instruction)
	if safety := strings.TrimSpace(safetyDoc); safety != "" {
		if prompt != "" {
			prompt += "\n\n"
		}
		prompt += safety
	}
	return &contextMiddleware{systemPrompt: prompt}
}

// BeforeModelRewriteState 在每次 Run 的首轮把系统提示放到消息头部。
// 同一 Run 后续模型轮次会复用 state，内容判定保证不会重复注入。
func (m *contextMiddleware) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState,
	_ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	if m.systemPrompt == "" || (len(state.Messages) > 0 && state.Messages[0] != nil &&
		state.Messages[0].Role == schema.System && state.Messages[0].Content == m.systemPrompt) {
		return ctx, state, nil
	}
	messages := make([]*schema.Message, 0, len(state.Messages)+1)
	messages = append(messages, schema.SystemMessage(m.systemPrompt))
	messages = append(messages, state.Messages...)
	next := *state
	next.Messages = messages
	return ctx, &next, nil
}

// AGENT.md 在 Profile 加载时已经读取；Runtime 不再按文件路径重复加载。

// logger 返回配置中的日志器；未配置时返回丢弃日志器（单测便捷路径）。
func (cfg AgentConfig) logger() *log.Logger {
	if cfg.Logger != nil {
		return cfg.Logger
	}
	return log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
}
