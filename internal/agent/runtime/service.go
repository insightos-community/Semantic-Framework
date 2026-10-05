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
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/agent/subagent"
	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/interaction"
	"insightos.cn/semantic-framework/internal/mcpregistry"
	"insightos.cn/semantic-framework/internal/security"
	"insightos.cn/semantic-framework/internal/skill"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/llm"
	"insightos.cn/semantic-framework/pkg/log"
)

// agentRoleLeader 是未指定收件人时使用的协调者角色。
const agentRoleLeader = "leader"

const (
	interactionModeCollaboration          = "collaboration"
	interactionModePlan                   = "plan"
	runtimePurposeWorkflowSummary         = "workflow_summary"
	runtimePurposeRobotDecision           = "robot_decision"
	runtimePurposeRobotNavigationDecision = "robot_navigation_decision"
	runtimePurposeTaskRecovery            = "task_recovery"
)

// titleRuneLimit 是自动标题取首条消息的字数上限（前 20 字）。
const titleRuneLimit = 20

// runSummaryRuneLimit 是结构化日志中委派任务摘要的长度上限；完整内容
// 保存在 Assistant 运行元数据和 Trace 中。
const runSummaryRuneLimit = 80

// toolResultRuneLimit 是 dialogue/tool.result 的实时展示上限。工具大结果不应
// 无界挤占 WebSocket 缓冲与浏览器消息流；完整大产物应进入 artifact/Trace。
const toolResultRuneLimit = 16000

var (
	// ErrSessionNotFound 表示会话不存在或不属于当前用户（统一不暴露存在性）。
	ErrSessionNotFound = errors.New("会话不存在")

	// ErrEmptyMessage 表示消息文本为空。
	ErrEmptyMessage = errors.New("消息文本不能为空")
)

// ModelBuilder 按注册表条目构建内核模型；默认 kernel.NewChatModel，
// 测试注入脚本化模型（mock）。
type ModelBuilder func(ctx context.Context, entry llm.Provider, apiKey string) (kernel.Model, error)

type DirectRobotLifecycle interface {
	StopDirectRun(context.Context, string) error
	FinishDirectRun(context.Context, string) error
}

type RobotSkillContracts interface {
	DescribeSkillInput(context.Context, string, string, string) (map[string]any, error)
}

// Deps 是 Service 的依赖集合。
type Deps struct {
	// Profiles 角色 profile 加载器。
	Profiles *profile.Loader

	// LLM LLM 提供方注册表。
	LLM *llm.Registry

	// Store 元数据存储（会话/消息/run 落库 + trace 落库）。
	Store *store.Store

	// Bus 进程内事件总线（dialogue 事件出口）。
	Bus *event.Bus

	// Logger 结构化日志器。
	Logger *log.Logger

	// BuildModel 模型构建器；nil 时用 kernel.NewChatModel。
	BuildModel ModelBuilder

	// Registry 工具注册表：按 profile tools.namespaces 过滤注入 agent，
	// 并构建 safety middleware（L2/L4 判定输入）；nil 时无内置工具体系。
	Registry *tool.Registry

	// Executor 工具执行器（Registry 非空时必填）。
	Executor *tool.Executor

	// MCPRegistry MCP 工具目录（架构 16 §4）：healthy 条目经同一
	// namespaces 过滤与 safety 门禁注入 agent（与内置工具同权）；
	// nil 时无 MCP 工具体系。
	MCPRegistry *mcpregistry.Registry

	// Interaction 交互服务（危险工具审批的请求-应答闭环）；
	// nil 时审批中断直接失败（防御性分支）。
	Interaction *interaction.Service

	// Tools 额外注入的工具清单（不过滤、不经安全门禁的测试逃生舱）。
	Tools []kernel.Tool

	// SkillStore 技能存储（skill middleware 的技能源 + skills.dir 热重载的
	// Reload 目标）；nil 时为无技能形态（不挂接 skill middleware）。
	SkillStore *skill.Store

	// AllowHostExecution 是服务端宿主执行硬开关；false 时会话不能开启。
	AllowHostExecution bool
	DirectRobot        DirectRobotLifecycle
	SkillContracts     RobotSkillContracts
}

// sessionRuntime 只保存模型、工具和 middleware 的装配，不承担会话串行控制。
type sessionRuntime struct {
	// runner 该会话的 kernel 运行器（middleware 栈与 trace 已挂接）。
	runner *kernel.Runner

	// profile 构建时的角色 profile（envelope 归因用）。
	profile *profile.Profile

	// Purpose runtimes are Run-scoped; output and tool feedback share this cap.
	contractCorrections *contractCorrectionBudget

	// supportsVision 当前实际模型端点是否声明 image 能力。
	supportsVision bool

	// modelResolution 记录角色请求端点与运行器实际采用端点/模型的对应关系；
	// 端点缺密钥触发全局默认回退时，两者不同，必须随 run 归档供前端如实展示。
	modelResolution ModelResolution
}

type activeRun struct {
	userID    string
	projectID string
	sessionID string
	contextID string
	runID     string
	runtime   *sessionRuntime
	cancel    context.CancelFunc
	done      chan struct{}
}

// Service 是 AgentRuntime 的最小实现：对话闭环的领域服务。
// 实现 ws.MessageHandler 接口（上行 chat.message 的入口）。
type Service struct {
	// profiles 角色 profile 加载器。
	profiles *profile.Loader

	// llmReg LLM 注册表。
	llmReg *llm.Registry

	// st 元数据存储。
	st *store.Store

	// bus 事件总线。
	bus *event.Bus

	// logger 结构化日志器。
	logger *log.Logger

	// buildModel 模型构建器。
	buildModel ModelBuilder

	// registry 工具注册表。
	registry *tool.Registry

	// executor 工具执行器。
	executor *tool.Executor

	// mcpReg MCP 工具目录（nil 时无 MCP 工具注入）。
	mcpReg *mcpregistry.Registry

	// interaction 交互服务。
	interaction *interaction.Service

	// tools 额外注入的工具清单（测试逃生舱）。
	tools []kernel.Tool

	// skills 技能存储（nil 时无技能形态，不挂接 skill middleware）。
	skills *skill.Store

	// allowHostExecution 是服务端宿主执行硬开关。
	allowHostExecution bool
	directRobot        DirectRobotLifecycle
	skillContracts     RobotSkillContracts

	// mu 保护会话运行器与活动 Run。
	mu sync.Mutex

	// sessions 进程内会话运行态缓存（session_id → 运行器）。缓存仅持有
	// 模型、工具和 middleware 装配；TraceHandler、Run 状态和 Context
	// 保存边界都在每次 Run 开始时动态注入。
	sessions map[string]*sessionRuntime

	// sessionLocks 不随 Runner 缓存淘汰，保证同会话的所有收件人、首次模型
	// 装配与设置写入共用一个串行门闩。持锁本身不要求构建 Leader 模型。
	sessionLocks map[string]*sync.Mutex

	// runtimeGeneration 防止热重载发生在缓存未命中的装配期间时，旧装配
	// 在淘汰操作结束后重新进入缓存。只在 s.mu 内读写。
	runtimeGeneration uint64

	// activeRuns 当前会话正在执行的 run，用于用户显式中断。
	activeRuns map[string]activeRun

	// staleSessions 标记配置变化时仍在运行、需在本轮结束后淘汰的会话运行器。
	// 运行中的 Runner 不热切模型，保证单轮一致；下一轮必须重新构建。
	staleSessions map[string]struct{}

	// roster Agent 目录（Team 组建时注册，供 Agent 目录 API 消费）。
	roster roster

	// subAgents Team 启用 SubAgent 的注册表（AssembleTeam 时派生，
	// 之后只读；nil = 未组建 Team，leader 工具集无委派面）。
	subAgents *subagent.Registry

	// leaderID roster 中 leader 成员的登记 ID（AssembleTeam 设置，
	// 未组建 Team 时为空；HandleMessage 的状态迁移以此为键）。
	leaderID string
}

// NewService 创建 AgentRuntime 服务。
func NewService(deps Deps) *Service {
	buildModel := deps.BuildModel
	if buildModel == nil {
		buildModel = kernel.NewChatModel
	}
	return &Service{
		profiles:           deps.Profiles,
		llmReg:             deps.LLM,
		st:                 deps.Store,
		bus:                deps.Bus,
		logger:             deps.Logger,
		buildModel:         buildModel,
		registry:           deps.Registry,
		executor:           deps.Executor,
		mcpReg:             deps.MCPRegistry,
		interaction:        deps.Interaction,
		tools:              deps.Tools,
		skills:             deps.SkillStore,
		allowHostExecution: deps.AllowHostExecution,
		directRobot:        deps.DirectRobot,
		skillContracts:     deps.SkillContracts,
		sessions:           make(map[string]*sessionRuntime),
		sessionLocks:       make(map[string]*sync.Mutex),
		activeRuns:         make(map[string]activeRun),
		staleSessions:      make(map[string]struct{}),
		roster:             roster{entries: make(map[string]AgentInfo)},
	}
}

// SetDirectRobotLifecycle 在启动装配阶段连接已有 Robot 执行服务。
func (s *Service) SetDirectRobotLifecycle(lifecycle DirectRobotLifecycle) {
	s.directRobot = lifecycle
}

// SetRobotSkillContracts is startup wiring, before accepting Agent Runs.
func (s *Service) SetRobotSkillContracts(contracts RobotSkillContracts) {
	s.skillContracts = contracts
}

// SetProfiles 原子替换角色 profile 加载器（agents.profiles_dir 热重载用）。
// 已缓存但空闲的运行器立即淘汰；运行中的会话完成当前轮后淘汰，
// 保证下一轮经新加载器构建且不会在单轮中途热切配置。
func (s *Service) SetProfiles(loader *profile.Loader) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.profiles = loader
	s.invalidateModelRuntimesLocked("")
}

// SetSkillStore 原子替换技能存储（skills.dir 热重载的补建路径用——启动时
// 技能目录不可用、运行中经配置热重载补建存储）。已缓存的会话运行器持有
// 旧装配（无 skill middleware）不受影响；新会话经新存储构建。
// 同对象的快照热更（store.Reload）不需要本方法：middleware 每次 run 重新
// 读快照渲染清单（见 kernel skill.go），下一轮 run 即生效。
func (s *Service) SetSkillStore(store *skill.Store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skills = store
}

// SkillStore 返回当前技能存储（技能库 REST 的读取面）：与 SetSkillStore
// 同一把锁——skills.dir 热重载的补建路径在运行中替换实例，读者始终拿到
// 最新一份；nil 表示无技能形态。
func (s *Service) SkillStore() *skill.Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.skills
}

// HandleMessage 处理一条用户消息：加载/创建会话 → 落库用户消息 →
// 构建（或取缓存）运行器 → 创建 run session → 执行并消费事件流
// （delta 实时下行）→ 落库助手消息与 run 终态 → 下行 message.done。
// 返回本次运行 ID；run 同步执行完成才返回（同会话天然排队）。
func (s *Service) HandleMessage(ctx context.Context, userID, sessionID, text string) (string, error) {
	return s.HandleMessageWithOptions(ctx, userID, sessionID, text, nil, "", "")
}

// HandleMessageWithAttachments 处理带图片引用的用户消息。
func (s *Service) HandleMessageWithAttachments(ctx context.Context, userID, sessionID, text string,
	attachmentIDs []string) (string, error) {
	return s.HandleMessageWithOptions(ctx, userID, sessionID, text, attachmentIDs, "", "")
}

// HandleMessageWithOptions 处理带附件及单轮推理覆盖的消息。覆盖只作用于本轮；
// Agent profile 仍是持久默认值，避免用户为一次深度任务永久拉高后续成本。
func (s *Service) HandleMessageWithOptions(ctx context.Context, userID, sessionID, text string,
	attachmentIDs []string, reasoningEffort, reasoningVisibility string) (string, error) {
	return s.HandleMessageWithMode(ctx, userID, sessionID, text, attachmentIDs,
		reasoningEffort, reasoningVisibility, interactionModeCollaboration)
}

// HandleMessageWithMode 在同一 Conversation 中按本轮模式装配 Leader。
// Plan 只是只读权限与提示词的收窄，不创建隐藏 Conversation 或并行 Runtime。
func (s *Service) HandleMessageWithMode(ctx context.Context, userID, sessionID, text string,
	attachmentIDs []string, reasoningEffort, reasoningVisibility, interactionMode string) (string, error) {
	return s.handleMessageWithMode(ctx, userID, sessionID, text, attachmentIDs,
		reasoningEffort, reasoningVisibility, interactionMode, "")
}

// HandleMessageToAgent 在共享会话中创建指定 Agent 的独立 Run。
func (s *Service) HandleMessageToAgent(ctx context.Context, userID, sessionID, text string,
	attachmentIDs []string, reasoningEffort, reasoningVisibility, interactionMode, agentID string) (string, error) {
	return s.handleMessageWithMode(ctx, userID, sessionID, text, attachmentIDs,
		reasoningEffort, reasoningVisibility, interactionMode, "", agentID)
}

// handleMessageWithMode 允许 AnswerRouter 把来源 Interaction 绑定到新 Run。
// 普通用户消息不携带该字段；结构化回答使用它保证 Server 重启恢复时不会
// 为同一回答重复创建 Leader Run。
func (s *Service) handleMessageWithMode(ctx context.Context, userID, sessionID, text string,
	attachmentIDs []string, reasoningEffort, reasoningVisibility, interactionMode, sourceInteractionID string, recipientIDs ...string) (string, error) {
	if err := validateRunReasoningOptions(reasoningEffort, reasoningVisibility); err != nil {
		return "", err
	}
	if interactionMode != interactionModeCollaboration && interactionMode != interactionModePlan {
		return "", fmt.Errorf("interaction_mode 仅支持 collaboration/plan")
	}
	originalText := strings.TrimSpace(text)
	text = originalText
	if text == "" && len(attachmentIDs) == 0 {
		return "", ErrEmptyMessage
	}
	if text == "" {
		text = "请分析这些图片。"
	}
	latestMessage, attachmentViews, err := s.resolveAttachments(userID, text, attachmentIDs)
	if err != nil {
		return "", err
	}

	initialTitle := conversationTitle(originalText, attachmentViews)
	if initialTitle == "" {
		initialTitle = "新会话"
	}
	sess, err := s.loadOrCreateSession(userID, sessionID, initialTitle)
	if err != nil {
		return "", err
	}

	// 先做统一门禁，再等待会话运行锁。等待可能持续到上一轮完成，不能在
	// 此处占用 Project 写门闩，否则归档/切换请求会被一条排队消息长期阻塞。
	// 真正落消息和 Run 前还会在同一个门闩内复查并连续写入。
	sess, project, err := s.requireWritableSession(userID, sess.ID)
	if err != nil {
		return "", err
	}
	recipientID := ""
	if len(recipientIDs) > 0 {
		recipientID = strings.TrimSpace(recipientIDs[0])
	}
	recipient, err := s.conversationRecipient(userID, sess.ID, recipientID)
	if err != nil {
		return "", err
	}
	gate := s.sessionLock(sess.ID)
	gate.Lock()
	defer gate.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var rt *sessionRuntime
	if recipient.ID != s.leaderPlanningAgentID() {
		var prof *profile.Profile
		prof, err = s.profileForAgent(recipient.ID)
		if err == nil {
			copy := *prof
			if reasoningVisibility != "" && reasoningVisibility != "inherit" {
				copy.ReasoningVisibility = reasoningVisibility
			}
			rt, err = s.buildConversationAgentRuntime(ctx, sess.ID, recipient.ID, &copy, reasoningEffort)
		}
	} else {
		var baseRT *sessionRuntime
		baseRT, err = s.runtimeForLocked(ctx, sess.ID)
		if err == nil {
			rt, err = s.runtimeForRunOptions(ctx, sess.ID, baseRT, reasoningEffort, reasoningVisibility)
		}
	}
	if err == nil && interactionMode == interactionModePlan && recipient.ID == s.leaderPlanningAgentID() {
		rt, err = s.buildPurposeRuntime(ctx, sess.ID, s.leaderPlanningAgentID(),
			store.RunKindConversation, interactionModePlan)
	}
	if err != nil {
		return "", err
	}
	if len(attachmentViews) > 0 && !rt.supportsVision {
		// 非视觉 Leader 仍可根据 ArtifactRef 把图片委派给视觉 SubAgent。
		// 图片本体不能发送给当前模型，否则兼容端点会直接返回能力错误。
		latestMessage = attachmentReferenceMessage(text, attachmentViews)
	}
	executionPolicy, err := s.st.GetSessionExecutionPolicy(sess.ID)
	if err != nil {
		return "", fmt.Errorf("加载会话执行策略失败: %w", err)
	}

	// roster 状态迁移：leader 进入/退出在办（未组建 Team 时 leaderID 为空，
	// setStatus 静默无操作）。
	if id := recipient.ID; id != "" {
		s.roster.setStatus(id, AgentStatusRunning, "处理用户消息")
		defer s.roster.setStatus(id, AgentStatusIdle, "待命")
	}

	// 历史重放（S7）：在写入新消息之前读取，新消息（S8）由 run 输入追加。
	contextInput, err := s.loadContextHistory(sess.ID, rt.supportsVision, recipient.ID)
	if err != nil {
		return "", err
	}
	history := contextInput.Messages

	// 先落库，再执行：用户消息是 run 的输入，落库失败不应启动 run。
	now := time.Now().UTC()
	userMeta, _ := json.Marshal(map[string]any{"attachments": attachmentViews, "target_agent_id": recipient.ID})
	artifactRefs := make([]string, 0, len(attachmentViews))
	for _, attachment := range attachmentViews {
		artifactRefs = append(artifactRefs, attachment.ID)
	}
	runID := store.NewRunSessionID()
	traceID := kernel.NewTraceID()
	run := store.RunSession{
		ID: runID, ProjectID: project.ID, ChatSessionID: sess.ID,
		SourceInteractionID: sourceInteractionID,
		ContextID:           "leader:" + sess.ID, AgentID: recipient.ID,
		AgentName: rt.profile.Name, Provider: rt.modelResolution.Provider,
		Endpoint: rt.modelResolution.ResolvedEndpoint,
		Model:    rt.modelResolution.ResolvedModel, TraceID: traceID,
		Status: store.RunStatusRunning, StartedAt: now, UpdatedAt: now,
	}
	userMessageID := store.NewChatMessageID()
	err = s.st.WithWritableConversation(userID, sess.ID,
		func(current store.ChatSession, writableProject store.Project) error {
			// Project 可能在模型装配期间被用户切换，因此真正落消息前再校验
			// 一次，并把当前 Project 写入 Run。门闩一直持有到 Run 创建完成，
			// 归档检查不可能从“尚未建 Run”的窗口穿过。
			sess = current
			project = writableProject
			run.ProjectID = project.ID
			if writeErr := s.st.AppendChatMessage(store.ChatMessage{
				ID: userMessageID, SessionID: sess.ID,
				Message: schema.UserMessage(text), ArtifactRefs: artifactRefs,
				Metadata: string(userMeta), CreatedAt: now,
			}); writeErr != nil {
				return writeErr
			}
			if writeErr := s.st.TouchChatSession(sess.ID, now); writeErr != nil {
				return writeErr
			}
			s.maybeAutoTitleSession(&sess, originalText, attachmentViews)
			return s.st.CreateRunSession(run)
		})
	if err != nil {
		return "", mapWritableSessionError(err)
	}
	run, err = s.st.GetRunSession(runID)
	if err != nil {
		return "", err
	}
	s.logger.Info("run 开始",
		"run_id", run.ID, "session_id", sess.ID, "user_id", userID,
		"agent", rt.profile.Name, "trace_id", run.TraceID, "history_len", len(history))

	// run 与连接解耦，但可由显式 chat.cancel / interrupt_current 中断。
	runCtx, cancel := context.WithCancel(context.Background())
	runCtx = kernel.WithRunArtifactRefs(runCtx, artifactRefs)
	runCtx = kernel.WithSummaryPersistence(runCtx, kernel.SummaryPersistence{
		ContextID: run.ContextID, ProjectID: run.ProjectID, SessionID: sess.ID,
		CoveredThroughMessageID: userMessageID,
		ExpectedRevision:        contextInput.Summary.Revision,
	})
	// 执行类工具通过单次 Run context 获取 Project 工作区，避免把会话目录
	// 写入全局工具实例后造成并发串用。Project 归属已在加载会话时校验。
	runCtx = tool.WithExecutionScope(runCtx, tool.ExecutionScope{
		RunKind: run.Kind, RunID: run.ID, AgentID: run.AgentID,
		RobotID:         recipient.RobotID,
		InteractionMode: interactionMode,
		SessionID:       sess.ID, ProjectID: project.ID, OwnerID: userID,
		WorkspaceRoot: project.WorkspaceRoot, SkillsRoot: s.skillRoot(),
		ExecutionMode:        executionPolicy.Mode,
		HostExecutionEnabled: executionPolicy.HostExecutionEnabled,
		HostExecutionAllowed: s.hostExecutionAllowed(),
	})
	runDone := make(chan struct{})
	s.mu.Lock()
	s.activeRuns[run.ContextID] = activeRun{
		userID: userID, projectID: project.ID,
		sessionID: sess.ID, contextID: run.ContextID,
		runID: run.ID, runtime: rt,
		cancel: cancel, done: runDone,
	}
	s.mu.Unlock()
	// started 必须在活动 Run 已登记后发布，前端收到事件后立即点击停止也能
	// 精确命中同一 run_id。
	s.publishRunState(run, rt, EventTypeRunStarted)
	defer func() {
		cancel()
		close(runDone)
		s.mu.Lock()
		if active, ok := s.activeRuns[run.ContextID]; ok && active.runID == runID {
			delete(s.activeRuns, run.ContextID)
		}
		if _, stale := s.staleSessions[sess.ID]; stale {
			delete(s.sessions, sess.ID)
			delete(s.staleSessions, sess.ID)
		}
		s.mu.Unlock()
	}()
	runErr := s.run(runCtx, rt, run, history, latestMessage)
	return run.ID, runErr
}

// sessionLock 的身份在 Service 生命周期中保持稳定。不能随缓存删除该锁，
// 否则持有旧指针的排队消息会与新锁上的模型切换或消息并发执行。
func (s *Service) sessionLock(sessionID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	gate := s.sessionLocks[sessionID]
	if gate == nil {
		gate = &sync.Mutex{}
		s.sessionLocks[sessionID] = gate
	}
	return gate
}

func validateRunReasoningOptions(effort, visibility string) error {
	validEffort := effort == "" || effort == "inherit" || effort == "auto" ||
		effort == "low" || effort == "medium" || effort == "high"
	if !validEffort {
		return fmt.Errorf("reasoning_effort 仅支持 inherit/auto/low/medium/high")
	}
	validVisibility := visibility == "" || visibility == "inherit" || visibility == "auto" ||
		visibility == "show" || visibility == "hide"
	if !validVisibility {
		return fmt.Errorf("reasoning_visibility 仅支持 inherit/auto/show/hide")
	}
	return nil
}

// runtimeForRunOptions 在会话缓存的基础运行态之外按需构建一次性运行器。它复用
// 调用方已持有独立会话锁；本轮覆盖不写入缓存，下一轮恢复会话模型快照。
func (s *Service) runtimeForRunOptions(ctx context.Context, sessionID string, baseRT *sessionRuntime,
	effort, visibility string) (*sessionRuntime, error) {
	if (effort == "" || effort == "inherit") &&
		(visibility == "" || visibility == "inherit") {
		return baseRT, nil
	}
	effectiveEffort := baseRT.profile.ReasoningEffort
	if effort != "" && effort != "inherit" {
		effectiveEffort = effort
	}
	effectiveVisibility := baseRT.profile.ReasoningVisibility
	if visibility != "" && visibility != "inherit" {
		effectiveVisibility = visibility
	}
	if effectiveEffort == baseRT.profile.ReasoningEffort &&
		effectiveVisibility == baseRT.profile.ReasoningVisibility {
		return baseRT, nil
	}
	prof := *baseRT.profile
	prof.ReasoningEffort = effectiveEffort
	prof.ReasoningVisibility = effectiveVisibility
	return s.buildSessionRuntime(ctx, sessionID, &prof, effectiveEffort)
}

// maybeAutoTitleSession 把 REST 预创建的默认标题替换为首轮语义标题。Web 会先
// 创建“新会话”再发消息，所以标题策略必须在运行时入口兜底，而不能只放在
// sessionID 为空的隐式创建路径中。
func (s *Service) maybeAutoTitleSession(sess *store.ChatSession, text string,
	attachments []MessageAttachment) {
	if sess == nil || !isDefaultSessionTitle(sess.Title) {
		return
	}
	title := conversationTitle(text, attachments)
	if title == "" {
		return
	}
	if err := s.st.UpdateChatSessionTitle(sess.ID, title); err != nil {
		s.logger.WithError(err).Warn("会话自动命名失败", "session_id", sess.ID)
		return
	}
	sess.Title = title
	s.logger.Info("会话已自动命名", "session_id", sess.ID, "title", title)
}

func isDefaultSessionTitle(title string) bool {
	switch strings.TrimSpace(title) {
	case "", "新会话", "新对话":
		return true
	default:
		return false
	}
}

func conversationTitle(text string, attachments []MessageAttachment) string {
	normalized := strings.Join(strings.Fields(text), " ")
	if normalized != "" && !isGenericConversationOpening(normalized) {
		return titleFrom(normalized)
	}
	if len(attachments) > 0 {
		name := strings.TrimSpace(attachments[0].Name)
		if dot := strings.LastIndex(name, "."); dot > 0 {
			name = name[:dot]
		}
		if name != "" && name != "blob" {
			return titleFrom(name + " · 图片分析")
		}
		return "图片分析"
	}
	return ""
}

func isGenericConversationOpening(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "你好", "你好！", "你好。", "hi", "hello", "在吗", "开始":
		return true
	default:
		return false
	}
}

// CancelRun 是旧会话入口的兼容层。它先解析活动 run_id，再进入精确取消；
// 没有活动 Run 时保持幂等成功，供旧 CLI 的停止按钮使用。
func (s *Service) CancelRun(ctx context.Context, userID, sessionID string) error {
	sess, err := s.st.GetChatSession(sessionID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && sess.UserID != userID) {
		return ErrSessionNotFound
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	active, ok := s.activeRuns["leader:"+sessionID]
	s.mu.Unlock()
	if !ok {
		return nil
	}
	return s.CancelRunByID(ctx, userID, active.projectID, active.runID)
}

// CancelRunByID 只取消调用方明确指定且仍在本进程执行的 Run。先把 Store
// 状态迁移为 cancelling 并发布事件，再发送取消信号；自然结束与停止请求
// 竞速时，条件迁移会阻止旧请求误伤后续 Run。
func (s *Service) CancelRunByID(_ context.Context, userID, projectID, runID string) error {
	run, err := s.st.GetRunSession(runID)
	if err != nil {
		return err
	}
	if run.ProjectID != projectID || s.st.ProjectOwnedByUser(userID, projectID) != nil {
		return store.ErrNotFound
	}
	s.mu.Lock()
	active, ok := s.activeRuns[run.ContextID]
	s.mu.Unlock()
	if !ok || active.runID != runID || active.userID != userID ||
		active.projectID != projectID {
		return fmt.Errorf("Run 当前不在本进程执行: %w", store.ErrInvalidState)
	}
	transitioned, err := s.st.TransitionRunStatus(runID,
		[]string{store.RunStatusQueued, store.RunStatusRunning,
			store.RunStatusWaitingInput}, store.RunStatusCancelling, time.Now().UTC())
	if err != nil {
		return err
	}
	s.publishRunState(transitioned, active.runtime, EventTypeRunCancelling)
	active.cancel()
	if s.directRobot != nil && run.Kind == store.RunKindConversation {
		return s.directRobot.StopDirectRun(context.Background(), runID)
	}
	return nil
}

// cancelActiveRun 用于 Server 关闭、会话归档和执行硬开关关闭。内部停止也尽量
// 先留下 cancelling 状态；若 Run 已经自然结束，仍发送 cancel 让进程资源收尾。
func (s *Service) cancelActiveRun(active activeRun) {
	transitioned, err := s.st.TransitionRunStatus(active.runID,
		[]string{store.RunStatusQueued, store.RunStatusRunning,
			store.RunStatusWaitingInput}, store.RunStatusCancelling, time.Now().UTC())
	if err == nil {
		s.publishRunState(transitioned, active.runtime, EventTypeRunCancelling)
	} else if !errors.Is(err, store.ErrInvalidState) && !errors.Is(err, store.ErrNotFound) {
		s.logger.WithError(err).Warn("Run 进入 cancelling 失败", "run_id", active.runID)
	}
	active.cancel()
	if s.directRobot != nil {
		if err := s.directRobot.StopDirectRun(context.Background(), active.runID); err != nil {
			s.logger.WithError(err).Warn("停止对话 Robot 请求失败", "run_id", active.runID)
		}
	}
}

// EvictSession 取消活动 run 并等待其完成落库，再剔除进程内运行态。
// 删除处理器必须在本方法成功后删除数据库会话，避免取消收尾与级联删除竞态。
func (s *Service) EvictSession(ctx context.Context, sessionID string) error {
	s.mu.Lock()
	s.runtimeGeneration++
	active, running := s.activeRuns["leader:"+sessionID]
	delete(s.sessions, sessionID)
	delete(s.staleSessions, sessionID)
	s.mu.Unlock()
	if running {
		s.cancelActiveRun(active)
	}
	if s.interaction != nil {
		if err := s.interaction.CancelSession(context.Background(), sessionID); err != nil {
			s.logger.WithError(err).Warn("取消会话待应答交互失败", "session_id", sessionID)
		}
	}
	if running {
		select {
		case <-active.done:
		case <-ctx.Done():
			return fmt.Errorf("等待会话运行结束: %w", ctx.Err())
		}
	}
	return nil
}

// leaderMemberID 返回 roster 中 leader 成员的登记 ID（空 = 未组建 Team）。
func (s *Service) leaderMemberID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leaderID
}

// loadOrCreateSession 加载会话并校验归属；sessionID 为空时创建新会话
// （标题取首条消息前 20 字）。会话不存在或归属不符统一返回 ErrSessionNotFound。
func (s *Service) loadOrCreateSession(userID, sessionID, text string) (store.ChatSession, error) {
	if sessionID == "" {
		now := time.Now().UTC()
		sess := store.ChatSession{
			ID: store.NewChatSessionID(), UserID: userID,
			Title: titleFrom(text), CreatedAt: now, UpdatedAt: now,
		}
		if err := s.st.CreateChatSession(sess); err != nil {
			return store.ChatSession{}, err
		}
		if err := s.st.WithWritableConversation(userID, sess.ID,
			func(current store.ChatSession, _ store.Project) error {
				sess = current
				return s.InitializeSessionModels(sess.ID)
			}); err != nil {
			// 模型快照是会话创建的一部分；失败时回滚空会话，避免留下下一轮
			// 行为不确定的半初始化数据。
			_ = s.st.DeleteChatSession(sess.ID)
			return store.ChatSession{}, fmt.Errorf("初始化会话 Agent 模型失败: %w",
				mapWritableSessionError(err))
		}
		s.logger.Info("会话已创建", "session_id", sess.ID, "user_id", userID, "title", sess.Title)
		return sess, nil
	}

	sess, _, err := s.requireWritableSession(userID, sessionID)
	return sess, err
}

// requireWritableSession 是 Runtime 所有 Conversation 写入口的统一门禁。
// 归属失败继续隐藏为 ErrSessionNotFound；归档和非活动状态原样保留，供
// HTTP/WS 返回明确原因。只读模型、工具、消息接口仍使用 requireOwnedSession。
func (s *Service) requireWritableSession(userID, sessionID string) (store.ChatSession, store.Project, error) {
	session, project, err := s.st.RequireWritableConversation(userID, sessionID)
	if err != nil {
		return session, project, mapWritableSessionError(err)
	}
	return session, project, nil
}

func mapWritableSessionError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return ErrSessionNotFound
	}
	return err
}

// runtimeFor 返回会话的运行态：缓存命中直接返回，否则按 leader profile
// 构建运行器并缓存（进程重启后的首条消息走这里的重建路径，doc.go 第 1 点）。
func (s *Service) runtimeFor(ctx context.Context, sessionID string) (*sessionRuntime, error) {
	gate := s.sessionLock(sessionID)
	gate.Lock()
	defer gate.Unlock()
	return s.runtimeForLocked(ctx, sessionID)
}

// runtimeForLocked 只装配实际由 Leader 接收的会话轮次；调用方必须持有
// sessionLock。设置切换与首次装配因此也是原子的，不依赖缓存是否已存在。
func (s *Service) runtimeForLocked(ctx context.Context, sessionID string) (*sessionRuntime, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		if rt, ok := s.sessions[sessionID]; ok {
			s.mu.Unlock()
			return rt, nil
		}
		generation, loader := s.runtimeGeneration, s.profiles
		s.mu.Unlock()
		prof, err := loader.Load(agentRoleLeader)
		if err != nil {
			return nil, fmt.Errorf("加载角色 %q 失败: %w", agentRoleLeader, err)
		}
		rt, err := s.buildSessionRuntime(ctx, sessionID, prof)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		if generation != s.runtimeGeneration {
			s.mu.Unlock()
			continue
		}
		s.sessions[sessionID] = rt
		s.mu.Unlock()
		return rt, nil
	}
}

func (s *Service) buildSessionRuntime(ctx context.Context, sessionID string,
	prof *profile.Profile, effortOverride ...string) (*sessionRuntime, error) {
	agentID := s.leaderMemberID()
	if agentID == "" {
		agentID = agentRoleLeader
	}
	return s.buildConversationAgentRuntime(ctx, sessionID, agentID, prof, effortOverride...)
}

func (s *Service) buildConversationAgentRuntime(ctx context.Context, sessionID, agentID string,
	prof *profile.Profile, effortOverride ...string) (*sessionRuntime, error) {
	entry, snapshot, err := s.resolveSessionModelEntry(sessionID, agentID, prof)
	if err != nil {
		return nil, err
	}
	if len(effortOverride) > 0 && effortOverride[0] != "" && effortOverride[0] != "inherit" {
		entry = withReasoningEffort(entry, effectiveReasoningEffort(effortOverride[0]))
	}
	// Profile defaults may differ from the persisted session override. Archive
	// the effort actually applied to this model, without mutating the shared
	// Profile or the cached baseline when the caller requests a one-run override.
	actualProfile := *prof
	actualProfile.ReasoningEffort = snapshot.ReasoningEffort
	if len(effortOverride) > 0 && effortOverride[0] != "" && effortOverride[0] != "inherit" {
		actualProfile.ReasoningEffort = effortOverride[0]
	}
	if actualProfile.ReasoningEffort == "" {
		actualProfile.ReasoningEffort = "auto"
	}
	prof = &actualProfile
	chatModel, err := s.buildModel(ctx, entry, s.llmReg.APIKey(snapshot.EndpointID))
	if err != nil {
		return nil, err
	}
	// Leader 与 SubAgent 共用同一保守重试策略：只在同端点且尚无输出时
	// 重试一次，不允许模型故障触发跨端点回退。
	chatModel = kernel.WithSafeModelRetry(chatModel)
	modelResolution := ModelResolution{
		RequestedEndpoint: prof.Model,
		ResolvedEndpoint:  snapshot.EndpointID,
		ResolvedModel:     entry.Model,
		Provider:          entry.Service,
		Source:            snapshot.Source,
		DefaultInherited:  snapshot.Source == store.ModelSourceSystemDefault,
		Fallback:          false,
	}
	// 委派工具先于工具链装配：其安全契约要并入 leader 的门禁清单
	// （门禁对未登记工具 fail-closed，见 buildToolchain）。
	var subTools []kernel.Tool
	var subDefs []tool.Definition
	if agentID == s.leaderPlanningAgentID() {
		subTools, subDefs, err = s.buildSubAgentTools(ctx, sessionID)
		if err != nil {
			return nil, err
		}
	}
	tools, dynamicTools, safety, err := s.buildSessionToolchain(
		sessionID, prof, agentID, subDefs...)
	if err != nil {
		return nil, err
	}
	tools = append(tools, subTools...)
	effectiveSkills, err := s.sessionSkillReader(sessionID, prof)
	if err != nil {
		return nil, err
	}
	workspaceRoot, err := s.sessionWorkspaceRoot(sessionID)
	if err != nil {
		return nil, err
	}
	runner, err := kernel.BuildAgent(ctx, kernel.AgentConfig{
		Name:          prof.Name,
		Role:          string(prof.Mode),
		Description:   prof.Description,
		Instruction:   prof.Instruction,
		SafetyDoc:     prof.SafetyDoc,
		Model:         chatModel,
		ModelName:     entry.Model,
		Tools:         tools,
		ToolSearch:    prof.Tools.ToolSearch,
		DynamicTools:  dynamicTools,
		Safety:        safety,
		MaxTurns:      prof.Limits.MaxTurns,
		ContextTokens: prof.Limits.ContextTokens,
		Store:         s.st,
		SkillStore:    effectiveSkills,
		WorkspaceRoot: workspaceRoot,
		Price:         entry.Price,
		Purpose:       "chat",
		// 直达 Robot 的对话在启动技能后交回执行面板；技能终态由 Pilot
		// 独立上报，避免模型重复轮询或因对话结束而撤销已接受的动作。
		ReturnDirectly: func() map[string]bool {
			if strings.HasPrefix(agentID, "robot:") {
				return map[string]bool{"robot.run": true}
			}
			return nil
		}(),
		Logger: s.logger,
	})
	if err != nil {
		return nil, err
	}

	rt := &sessionRuntime{
		runner:          runner,
		profile:         prof,
		supportsVision:  hasCapability(entry.Capabilities, "image"),
		modelResolution: modelResolution,
	}
	s.logger.Info("运行器已构建", "session_id", sessionID,
		"agent", prof.Name, "model", entry.Model)
	return rt, nil
}

// run 执行一轮对话并消费事件流：delta 实时下行；危险工具中断时
// 经交互服务等待审批、恢复执行（Run 状态 running→waiting_input→running）；
// 结束时先落库助手消息与 run 终态，再下行 message.done（一致性顺序见 doc.go 第 2 点）。
// 断点 ID = run ID（断点随 run_sessions 存）。
func (s *Service) run(ctx context.Context, rt *sessionRuntime, run store.RunSession,
	history []*schema.Message, latestMessage *schema.Message) error {
	sessionID, runID, traceID := run.ChatSessionID, run.ID, run.TraceID
	ref := eventContextFromRun(run)
	effort := rt.profile.ReasoningEffort
	if effort == "" {
		effort = "auto"
	}
	activity := RunMetadata{Status: store.RunStatusRunning, ReasoningEffort: effort, StartedAt: run.StartedAt.Format(time.RFC3339Nano),
		ReasoningVisibility: rt.profile.ReasoningVisibility,
		Model:               &rt.modelResolution}

	messages := append([]*schema.Message(nil), history...)
	messages = append(messages, latestMessage)
	stream, err := rt.runner.RunMessages(ctx, messages,
		kernel.RunOptions{CheckpointID: runID, TraceID: traceID})
	if err != nil {
		activity.Status = store.RunStatusFailed
		activity.Error = err.Error()
		finished, finishErr := s.finishRun(rt, run, store.RunStatusFailed, "", activity)
		if finishErr != nil {
			return errors.Join(err, finishErr)
		}
		s.publishRunState(finished, rt, EventTypeRunFailed)
		return err
	}

	var fullText strings.Builder
	var turns int
	var usage kernel.Usage
	var runErr error
	var done bool
	delegationByAgent := make(map[string]int)

	// 消费-恢复循环：中断事件关闭当前事件流后，处理中断原因（审批）
	// 并用断点恢复出新事件流，直到运行正常结束。
	for stream != nil && !done && runErr == nil {
		ev, ok := stream.Next()
		if !ok {
			break
		}
		switch ev.Kind {
		case kernel.EventReasoningDelta:
			if ev.AgentID != "" {
				if info, ok := s.roster.get(ev.AgentID); ok && info.ReasoningVisibility == "hide" {
					continue
				}
				idx, exists := delegationByAgent[ev.AgentID]
				if !exists {
					activity.Delegations = append(activity.Delegations, DelegationActivity{
						AgentID: ev.AgentID, Status: "running",
					})
					idx = len(activity.Delegations) - 1
					delegationByAgent[ev.AgentID] = idx
				}
				activity.Delegations[idx].Reasoning += ev.Text
				s.publishSubAgent(ref, ev.AgentID, EventTypeReasoningDelta,
					ReasoningDeltaPayload{RunID: runID, Text: ev.Text})
			} else {
				if rt.profile.ReasoningVisibility == "hide" {
					continue
				}
				activity.Reasoning += ev.Text
				if len(activity.ReasoningRounds) == 0 || activity.ReasoningRounds[len(activity.ReasoningRounds)-1].Turn != ev.Turns {
					activity.ReasoningRounds = append(activity.ReasoningRounds, ReasoningRound{Turn: ev.Turns})
				}
				activity.ReasoningRounds[len(activity.ReasoningRounds)-1].Text += ev.Text
				s.publish(ref, rt, EventTypeReasoningDelta,
					ReasoningDeltaPayload{RunID: runID, Text: ev.Text, Turn: ev.Turns})
			}
		case kernel.EventTextDelta:
			fullText.WriteString(ev.Text)
			s.publish(ref, rt, EventTypeMessageDelta, MessageDeltaPayload{RunID: runID, Text: ev.Text})
		case kernel.EventSubAgentDelta:
			idx, exists := delegationByAgent[ev.AgentID]
			if !exists {
				activity.Delegations = append(activity.Delegations, DelegationActivity{
					AgentID: ev.AgentID, Status: "running",
				})
				idx = len(activity.Delegations) - 1
				delegationByAgent[ev.AgentID] = idx
			}
			activity.Delegations[idx].Text += ev.Text
			s.publishSubAgent(ref, ev.AgentID, EventTypeSubAgentDelta,
				SubAgentDeltaPayload{RunID: runID, Text: ev.Text})
		case kernel.EventSubAgentResult:
			idx, exists := delegationByAgent[ev.AgentID]
			if !exists {
				activity.Delegations = append(activity.Delegations, DelegationActivity{AgentID: ev.AgentID})
				idx = len(activity.Delegations) - 1
			}
			activity.Delegations[idx].Task = ev.Task
			activity.Delegations[idx].Text = ev.Text
			activity.Delegations[idx].Status = "done"
			delete(delegationByAgent, ev.AgentID)
			s.logger.Info("委派完成", "run_id", runID, "session_id", sessionID,
				"subagent", ev.AgentID, "task", truncateRunes(ev.Task, runSummaryRuneLimit))
			s.publishSubAgent(ref, ev.AgentID, EventTypeSubAgentResult,
				SubAgentResultPayload{RunID: runID, Task: ev.Task, Text: ev.Text})
		case kernel.EventToolCall:
			activity.Tools = append(activity.Tools, ToolActivity{
				CallID: ev.CallID, AgentID: ev.AgentID, Name: ev.ToolName,
				Arguments: ev.Arguments, Status: "running",
			})
			payload := ToolCallPayload{RunID: runID, CallID: ev.CallID,
				Name: ev.ToolName, Arguments: ev.Arguments}
			if ev.AgentID != "" {
				s.publishSubAgent(ref, ev.AgentID, EventTypeToolCall, payload)
			} else {
				s.publish(ref, rt, EventTypeToolCall, payload)
			}
		case kernel.EventToolResult:
			s.logger.Debug("工具执行完成", "run_id", runID, "session_id", sessionID, "tool", ev.ToolName)
			if (ev.ToolName == "plan.suggest" || ev.ToolName == "plan_suggest") && planSuggestionAccepted(ev.Text) {
				// The successful Proposal submission is the answer. Do not ask the
				// model for a cosmetic closing sentence after a durable side effect.
				const reply = "Plan Proposal 已生成，请审阅后再批准执行。"
				fullText.WriteString(reply)
				s.publish(ref, rt, EventTypeMessageDelta, MessageDeltaPayload{RunID: runID, Text: reply})
			}
			result, truncated := boundedToolResult(ev.Text)
			matched := false
			for i := len(activity.Tools) - 1; i >= 0; i-- {
				tool := &activity.Tools[i]
				if (ev.CallID != "" && tool.CallID == ev.CallID) ||
					(ev.CallID == "" && tool.Name == ev.ToolName && tool.AgentID == ev.AgentID && tool.Status == "running") {
					tool.Status, tool.Result, tool.Truncated = "done", result, truncated
					matched = true
					break
				}
			}
			if !matched {
				activity.Tools = append(activity.Tools, ToolActivity{CallID: ev.CallID,
					AgentID: ev.AgentID, Name: ev.ToolName, Status: "done", Result: result, Truncated: truncated})
			}
			payload := ToolResultPayload{RunID: runID, CallID: ev.CallID,
				Name: ev.ToolName, Result: result, Truncated: truncated}
			if ev.AgentID != "" {
				s.publishSubAgent(ref, ev.AgentID, EventTypeToolResult, payload)
			} else {
				s.publish(ref, rt, EventTypeToolResult, payload)
			}
		case kernel.EventInterrupted:
			turns += ev.Turns
			usage.PromptTokens += ev.Usage.PromptTokens
			usage.CompletionTokens += ev.Usage.CompletionTokens
			usage.TotalTokens += ev.Usage.TotalTokens
			stream, run, err = s.awaitApproval(ctx, rt, run, ev.Interrupts)
			ref = eventContextFromRun(run)
			if err != nil {
				runErr = err
			}
		case kernel.EventError:
			if errors.Is(ev.Err, errPlanSubmissionComplete) {
				turns += ev.Turns
				usage.PromptTokens += ev.Usage.PromptTokens
				usage.CompletionTokens += ev.Usage.CompletionTokens
				usage.TotalTokens += ev.Usage.TotalTokens
				done = true
				break
			}
			if runErr == nil {
				runErr = ev.Err
			}
		case kernel.EventDone:
			turns += ev.Turns
			usage.PromptTokens += ev.Usage.PromptTokens
			usage.CompletionTokens += ev.Usage.CompletionTokens
			usage.TotalTokens += ev.Usage.TotalTokens
			done = true
		}
	}

	if !done && runErr == nil {
		runErr = errors.New("Agent 事件流在返回终态前关闭")
	}
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			activity.Status = store.RunStatusCancelled
			activity.Turns = turns
			activity.Usage = usagePayload(usage)
			finished, finishErr := s.finishRun(rt, run, store.RunStatusCancelled,
				fullText.String(), activity)
			if finishErr != nil {
				return errors.Join(runErr, finishErr)
			}
			run, ref = finished, eventContextFromRun(finished)
			s.publishRunState(run, rt, EventTypeRunCancelled)
			s.publish(ref, rt, EventTypeMessageDone, MessageDonePayload{
				RunID: runID, TraceID: traceID, Text: fullText.String(), Turns: turns,
				Usage: activity.Usage, Cancelled: true, Model: activity.Model,
			})
			s.logger.Info("run 已由用户中断", "run_id", runID, "session_id", sessionID)
			return nil
		}
		activity.Status = store.RunStatusFailed
		activity.Turns = turns
		activity.Usage = usagePayload(usage)
		activity.Error = runErr.Error()
		finished, finishErr := s.finishRun(rt, run, store.RunStatusFailed,
			fullText.String(), activity)
		if finishErr != nil {
			return errors.Join(runErr, finishErr)
		}
		run, ref = finished, eventContextFromRun(finished)
		s.publishRunState(run, rt, EventTypeRunFailed)
		s.publish(ref, rt, EventTypeMessageDone, MessageDonePayload{
			RunID: runID, TraceID: traceID, Turns: turns,
			Error: runErr.Error(), Model: activity.Model,
		})
		s.logger.Warn("run 失败", "run_id", runID, "session_id", sessionID,
			"turns", turns, "error", runErr.Error())
		return runErr
	}

	activity.Status = store.RunStatusCompleted
	activity.Turns = turns
	activity.Usage = usagePayload(usage)
	finished, err := s.finishCompletedRun(rt, run, fullText.String(), activity)
	if err != nil {
		return err
	}
	run, ref = finished, eventContextFromRun(finished)
	if finished.Status == store.RunStatusCancelled {
		s.publishRunState(run, rt, EventTypeRunCancelled)
		s.publish(ref, rt, EventTypeMessageDone, MessageDonePayload{
			RunID: runID, TraceID: traceID, Text: fullText.String(), Turns: turns,
			Usage: activity.Usage, Cancelled: true, Model: activity.Model,
		})
		s.logger.Info("run 完成与取消竞速，按已保存的取消请求收尾",
			"run_id", runID, "session_id", sessionID)
		return nil
	}
	s.publishRunState(run, rt, EventTypeRunCompleted)
	s.publish(ref, rt, EventTypeMessageDone, MessageDonePayload{
		RunID: runID, TraceID: traceID, Text: fullText.String(), Turns: turns,
		Model: activity.Model,
		Usage: &UsagePayload{
			PromptTokens:     usage.PromptTokens,
			CompletionTokens: usage.CompletionTokens,
			TotalTokens:      usage.TotalTokens,
		},
	})
	s.logger.Info("run 结束",
		"run_id", runID, "session_id", sessionID, "turns", turns,
		"prompt_tokens", usage.PromptTokens,
		"completion_tokens", usage.CompletionTokens,
		"total_tokens", usage.TotalTokens)
	return nil
}

func planSuggestionAccepted(result string) bool {
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			ProposalID string `json:"plan_proposal_id"`
		} `json:"data"`
	}
	return json.Unmarshal([]byte(result), &envelope) == nil && envelope.OK && envelope.Data.ProposalID != ""
}

// awaitApproval 处理运行中断（危险工具审批）：状态迁移 waiting_input →
// 逐个中断点发起交互请求等待应答 → 状态恢复 running → 断点恢复执行。
// 一致性顺序：断点在交互请求之前已持久化（内核保证，见 kernel run.go）；
// 应答先落库再恢复执行（interaction.Reply 保证）。
func (s *Service) awaitApproval(ctx context.Context, rt *sessionRuntime, run store.RunSession,
	interrupts []kernel.Interrupt) (*kernel.EventStream, store.RunSession, error) {
	sessionID, runID := run.ChatSessionID, run.ID

	if s.interaction == nil {
		return nil, run, errors.New("运行需要人工审批，但交互服务未配置")
	}
	transitioned, err := s.st.TransitionRunStatus(runID,
		[]string{store.RunStatusRunning}, store.RunStatusWaitingInput, time.Now().UTC())
	if err != nil {
		return nil, run, err
	}
	run = transitioned
	s.publishRunState(run, rt, EventTypeRunWaitingInput)
	s.logger.Info("run 等待人工审批", "run_id", runID, "session_id", sessionID,
		"interrupts", len(interrupts))

	targets := make(map[string]any, len(interrupts))
	for _, intr := range interrupts {
		info, ok := intr.Info.(security.ApprovalInfo)
		if !ok {
			// 未知中断负载：安全兜底按拒绝处理（恢复为"不批准"），不放行。
			s.logger.Warn("未知中断负载，按拒绝处理",
				"run_id", runID, "interrupt_id", intr.ID, "info_type", fmt.Sprintf("%T", intr.Info))
			targets[intr.ID] = false
			continue
		}
		// 审批归因到中断发起方：SubAgent 内的审批中断经 CompositeInterrupt
		// 冒泡到本 run，发起方身份随负载自包含（ApprovalInfo.Agent，如
		// query-1）；空值是防御性回退（负载来自未携身份的旧装配路径）。
		agent := info.Agent
		if agent == "" {
			agent = rt.profile.Name
		}
		approved, err := s.interaction.Request(ctx, interaction.Request{
			SessionID: sessionID, Agent: agent,
			Question: info.Question, Risk: info.Risk,
			RunID: runID, CheckpointID: runID,
		})
		if err != nil {
			return nil, run, fmt.Errorf("等待审批应答失败: %w", err)
		}
		targets[intr.ID] = approved
		s.logger.Info("审批结论", "run_id", runID, "session_id", sessionID,
			"agent", agent, "tool", info.Tool, "approved", approved)
	}

	transitioned, err = s.st.TransitionRunStatus(runID,
		[]string{store.RunStatusWaitingInput}, store.RunStatusRunning, time.Now().UTC())
	if err != nil {
		return nil, run, err
	}
	run = transitioned
	s.publishRunState(run, rt, EventTypeRunRunning)
	s.logger.Info("run 恢复执行", "run_id", runID, "session_id", sessionID)
	stream, err := rt.runner.Resume(ctx, runID, targets)
	return stream, run, err
}
