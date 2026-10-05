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

package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/install"
	"insightos.cn/semantic-framework/internal/mcpregistry"
	robotdomain "insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/server/aggregate"
	"insightos.cn/semantic-framework/internal/server/auth"
	serverhttp "insightos.cn/semantic-framework/internal/server/http"
	"insightos.cn/semantic-framework/internal/server/http/handlers"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool/builtin"
	"insightos.cn/semantic-framework/internal/workflow"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/llm"
	"insightos.cn/semantic-framework/pkg/log"
	"insightos.cn/semantic-framework/pkg/mcp"
)

// wsReadHeaderTimeout 限制 WS 握手请求头的读取时间，防慢速攻击；
// 注意 WS 服务不能设置 ReadTimeout/WriteTimeout——升级后的长连接
// 会被整连接超时误杀。
const wsReadHeaderTimeout = 10 * time.Second

// Options 是 Wire 的可选注入（集成测试用：MCP 连接池短路窗口、
// 目录对账节奏——用短窗口/关周期消除后台时序对断言的干扰）。
// 生产路径一律用零值（Wire 入口）。
type Options struct {
	// MCPPool 连接池参数；nil 取默认（短路 15min 等）。
	MCPPool *mcp.PoolOptions

	// MCPSyncer 目录对账节奏；nil 取默认（快频 2s×3 → 稳态 30s）。
	MCPSyncer *mcpregistry.SyncerOptions
}

// Wire 按启动序列装配接入层应用（架构文档 §4），等价于
// WireWithOptions(cfg, logger, Options{})。
func Wire(cfg *config.Config, logger *log.Logger) (*App, error) {
	return WireWithOptions(cfg, logger, Options{})
}

// WireWithOptions 按启动序列装配接入层应用（架构文档 §4）：
// 打开 store → 执行迁移 → auth 种子用户 → LLM 注册表（缺 key 降级 WARN）→
// 事件总线/WS hub → 工具体系/MCP 目录 → 装配 HTTP 网关（中间件链 +
// 公开/受保护路由）与 WS 网关。
// 任一步骤失败即返回错误，已打开的 store 随之关闭，不留半启动状态。
func WireWithOptions(cfg *config.Config, logger *log.Logger, opts Options) (*App, error) {
	// ① 元数据存储：打开并迁移到最新 schema。
	st, err := store.Open(cfg.Store, logger)
	if err != nil {
		return nil, err
	}
	if err := st.Migrate(); err != nil {
		_ = st.Close()
		return nil, err
	}

	// ② 认证服务与种子用户（保证零配置启动后存在可登录账号）。
	authSvc := auth.NewService(st, logger)
	if cfg.Server.AccessTokenTTL != 0 {
		if err := authSvc.SetTokenTTL(time.Duration(cfg.Server.AccessTokenTTL)); err != nil {
			_ = st.Close()
			return nil, fmt.Errorf("用户令牌有效期配置失败: %w", err)
		}
	}
	if err := authSvc.SeedAdmin(); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("种子用户初始化失败: %w", err)
	}

	// ③ LLM 注册表：配置错误（如 default 不在清单）直接失败；
	// 缺 key 的端点只 WARN 降级（该模型暂不可用，仅 mock 可用），不阻塞启动。
	llmReg, err := llm.Load(cfg.LLM)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("LLM 注册表加载失败: %w", err)
	}
	// 注入服务端托管密钥库：密钥解析链变为 env（含 .env）> settings_keys 表。
	llmReg.SetKeyStore(keyStoreAdapter{st})
	// 未知驱动 fail-closed：启动时聚合报出，而不是等首次调用才失败。
	if err := checkLLMComponents(cfg.LLM); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("LLM 注册表加载失败: %w", err)
	}
	for _, name := range llmReg.Names() {
		p, err := llmReg.Get(name)
		if err != nil {
			continue // Names 返回的名字必然存在，防御性跳过
		}
		if kernel.RequiresAPIKey(p.Component) && llmReg.APIKey(name) == "" {
			logger.Warn("模型端点缺少 API key，该模型暂不可用",
				"provider", name,
				"component", p.Component,
				"env", llm.APIKeyEnv(name),
			)
		}
	}
	logger.Info("LLM 注册表已加载",
		"default", llmReg.Default().Name,
		"providers", llmReg.Names(),
	)

	// ④ 事件总线、WS 连接中枢与聚合器：bus 承载域模块事件，hub 负责
	// 下行投递，聚合器（bus → 归一化/分级/落库 → hub）是事件面的唯一
	// 正规通道（替代 B1 的朴素桥接，断连续传的补发能力也由它提供）。
	bus := event.NewBus(logger)
	hub := ws.NewHub(logger)
	agg := aggregate.NewAggregator(bus, hub, st, logger)
	gateway := ws.NewGateway(hub, authSvc, agg, logger)

	// ⑤ 技能存储：skills.dir 快照 + 热更监听；目录不可用降级为无技能
	// 形态（nil），skills.dir 配置热重载时可补建（见 wire_reload）。
	// Eino Skill Middleware 在 Agent 运行器装配时读取该快照。
	skillStore := newSkillStore(cfg, logger)

	// ⑤.5 工具体系与交互服务：注册表/执行器是 agent 工具链与安全管线的
	// 输入；交互服务承载审批请求（下行 interaction.request / 上行 reply）。
	interactionSvc := newInteraction(st, bus, logger)
	planEntry := newPlanEntry(interactionSvc, st)
	toolRegistry, toolExecutor, err := newToolchain(st, logger, planEntry)
	if err != nil {
		stopSkillStore(skillStore)
		_ = st.Close()
		return nil, err
	}

	// ⑤.5 MCP 工具目录（架构 16 §4）：mcp_servers 结构校验 fail-closed
	// （重名/非法 risk 是配置错误，启动期暴露）；连接随 App.Run 的首轮
	robotService := robotdomain.NewService(st, robotEventPublisher{bus: bus})
	if err := builtin.RegisterRobotTools(toolRegistry, robotService); err != nil {
		stopSkillStore(skillStore)
		_ = st.Close()
		return nil, fmt.Errorf("注册 Robot 工具失败: %w", err)
	}
	// 对账建立，server 不可达只降级不阻塞启动。
	if err := validateMCPServers(cfg.MCPServers); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("mcp_servers 配置校验失败: %w", err)
	}
	mcpPool, mcpReg, mcpSyncer := newMCPStack(logger, opts.MCPPool, opts.MCPSyncer)

	// ⑥ Agent 运行时：角色 profile + 对话闭环服务（REST handler 与 WS 对话网关的共用后端）。
	agentRT, err := newAgentRuntime(cfg, logger, st, llmReg, bus, toolRegistry, toolExecutor, mcpReg, interactionSvc, skillStore)
	if err != nil {
		stopSkillStore(skillStore)
		_ = st.Close()
		return nil, err
	}
	agentRT.SetDirectRobotLifecycle(robotService)
	agentRT.SetRobotSkillContracts(robotService)
	if err := agentRT.RecoverInterruptedRuns(time.Now().UTC()); err != nil {
		stopSkillStore(skillStore)
		_ = st.Close()
		return nil, fmt.Errorf("收敛 Server 重启前的 Run 失败: %w", err)
	}
	workflowSvc, err := workflow.NewService(workflow.Deps{
		Store: st, Bus: bus, Planner: agentRT, Executor: agentRT,
		RobotReply: robotService, RobotStop: robotService, ConversationResumer: agentRT,
	})
	if err != nil {
		stopSkillStore(skillStore)
		_ = st.Close()
		return nil, fmt.Errorf("装配 Workflow 服务失败: %w", err)
	}
	robotService.SetExecutionObserver(workflowSvc)
	robotService.SetAvailabilityObserver(workflowSvc)
	planEntry.SetWorkflow(workflowSvc)
	interactionSvc.SetAnswerRouter(workflowSvc)
	if err := workflowSvc.RecoverInterruptedWorkflows(time.Now().UTC()); err != nil {
		stopSkillStore(skillStore)
		_ = st.Close()
		return nil, fmt.Errorf("收敛 Server 重启前的 Workflow 失败: %w", err)
	}
	// 已经 answered 但尚未完成业务路由的 Interaction 在状态收敛之后继续。
	// 已存在失败/取消的 Task Run 会保持未处理，等待用户显式 Resume，不重放。
	if err := interactionSvc.RecoverAnsweredInteractions(context.Background()); err != nil {
		logger.WithError(err).Warn("部分 Interaction 等待用户显式恢复")
	}
	chatGateway := ws.NewChatGateway(hub, authSvc, agentRT, interactionSvc, agg, logger)
	studioGateway := ws.NewStudioGateway(hub, authSvc, agentRT,
		interactionSvc, agg, st, logger)
	chatHandler := handlers.NewChatHandler(st, agentRT, logger, bus)
	simulationSvc, sceneAuthoringSvc, err := newConfiguredSimulationServices(st, cfg, bus)
	if err != nil {
		stopSkillStore(skillStore)
		_ = st.Close()
		return nil, fmt.Errorf("装配仿真 Runtime 与场景目录失败: %w", err)
	}
	robotLifecycle, err := newManagedSceneRobotLifecycle(cfg, st, robotService, logger)
	if err != nil {
		stopSkillStore(skillStore)
		_ = st.Close()
		return nil, fmt.Errorf("装配受管 Robot Runtime 失败: %w", err)
	}
	// 仅安装 Runtime/场景时可关闭 Robot 调度。typed nil 转为接口后并不等于 nil，
	// 因而必须在装配点决定是否注册，避免场景加载成功后异步调用空生命周期。
	if robotLifecycle != nil {
		simulationSvc.ConfigureRobotLifecycle(robotLifecycle)
	}
	robotService.SetRobotStateReader("simulation.robot_state", func(ctx context.Context,
		projectID, sceneInstanceID, robotID string) (robotdomain.StateObservation, error) {
		state, err := simulationSvc.RobotState(ctx, projectID, sceneInstanceID, robotID)
		if err != nil {
			return robotdomain.StateObservation{}, err
		}
		return robotdomain.StateObservation{RobotID: state.RobotID, ObservedAt: state.ObservedAt,
			Generation: state.Generation, HoldingObject: state.HoldingObject, InHold: state.InHold}, nil
	})
	robotService.SetExecutionObserver(executionObserverChain{
		robotMapCheckpointObserver{store: st, simulation: simulationSvc},
		workflowSvc,
	})
	// Scene 与受管 Runtime 属于 Project 的显式运行资源，不能由浏览器 WS
	// 连接代持。长时间 Robot Execution 期间页面休眠或短暂断网都可能让 Studio
	// WS 超时；若把断连解释为“退出 Project”，一分钟后会在物理动作中途停止
	// Pilot 和 Runtime。场景只由显式 stop、Project 切换/归档或 Server shutdown
	// 收敛，Web 连接恢复后通过 Project Snapshot 继续观察。

	simulationHandler := handlers.NewSimulationHandler(
		simulationSvc, sceneAuthoringSvc, st, bus,
	)
	simulationStreamGateway := ws.NewSimulationStreamGateway(
		authSvc, st, simulationSvc, logger,
	)

	// ⑦ 装配两个 server：REST 面走统一网关路由；WS 面独立 listener，
	// 单独的超时策略（长连接不能用整连接超时）。
	// 设置控制器先挂到 App（PATCH 写回路径由 EnableConfigReload 随后装配），
	// settings handler 与 chat handler 一并进入受保护路由组。
	wsMux := http.NewServeMux()
	wsMux.Handle("/ws/agent-events", gateway)
	wsMux.Handle("/ws/chat", chatGateway)
	wsMux.Handle("/ws/studio", studioGateway)
	wsMux.Handle("/ws/devices", ws.NewDeviceGateway(robotService, authSvc, logger))
	wsMux.Handle("/ws/pilot", robotdomain.NewPilotGateway(robotService, logger))
	wsMux.Handle("/ws/simulation-stream", simulationStreamGateway)

	app := &App{
		cfg:           cfg,
		logger:        logger,
		store:         st,
		llmReg:        llmReg,
		bus:           bus,
		hub:           hub,
		aggregator:    agg,
		runtime:       agentRT,
		workflow:      workflowSvc,
		robots:        robotService,
		simulation:    simulationSvc,
		managedRobots: robotLifecycle,
		toolRegistry:  toolRegistry,
		skills:        skillStore,
		mcpPool:       mcpPool,
		mcpReg:        mcpReg,
		mcpSyncer:     mcpSyncer,
	}
	app.settingsCtl = newSettingsController(app, logger)
	simulationHandler.SetResourceReload(func(ctx context.Context) error {
		return simulationSvc.ReloadResources(ctx, cfg.Simulation.RuntimesDir, cfg.Simulation.CatalogDir)
	}, func(ctx context.Context, id string) error {
		item, err := simulationSvc.RuntimeInstallation(id)
		if err != nil {
			return err
		}
		if err := simulationSvc.CheckRuntimeInstallIdle(ctx, item.Profile.RuntimeProfileID); err != nil {
			return err
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		if err := install.RunCommand(ctx, func(string) {}, filepath.Join(filepath.Dir(executable), "semantic"), "runtime", "uninstall", "--id", id, "-c", app.settingsCtl.ConfigPath()); err != nil {
			return err
		}
		return simulationSvc.ReloadResources(ctx, cfg.Simulation.RuntimesDir, cfg.Simulation.CatalogDir)
	})
	settingsHandler := handlers.NewSettingsHandler(st, llmReg, app.settingsCtl, logger)
	agentsHandler := handlers.NewAgentsHandler(agentRT)
	// 技能库经 provider 读 runtime 持有的当前 store（带锁）：skills.dir
	// 热重载补建路径替换实例后，端点无需重装配即读到新快照。
	skillsHandler := handlers.NewSkillsHandler(agentRT.SkillStore)
	toolsHandler := handlers.NewToolsHandler(toolRegistry, mcpReg)
	projectsHandler := handlers.NewProjectsHandler(st, agentRT, logger, bus)
	projectsHandler.SetWorkflowApplication(workflowSvc)
	projectsHandler.SetSimulationLifecycle(simulationSvc)
	app.imports = install.NewInbox(st, func(ctx context.Context, projectID string, pkg install.Package, data []byte) ([]string, error) {
		project, err := st.GetProject(projectID)
		if err != nil {
			return nil, err
		}
		if project.ArchivedAt != nil || project.Mode != store.ProjectModeDevelopment {
			return nil, fmt.Errorf("项目当前不在开发模式")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch pkg.Kind {
		case "robot_skill":
			item, err := robotService.PublishSkillArchive(bytes.NewReader(data))
			if err != nil {
				return nil, err
			}
			return []string{item.Name + "@" + item.Version}, nil
		case "scene":
			profile, err := simulationSvc.ProjectRuntimeProfile(projectID)
			if err != nil {
				return nil, err
			}
			documents, err := sceneAuthoringSvc.ImportScenePackage(projectID, profile.RuntimeProfileID, data)
			if err != nil {
				return nil, err
			}
			ids := make([]string, 0, len(documents))
			for _, document := range documents {
				ids = append(ids, document.ID)
			}
			return ids, nil
		default:
			return nil, fmt.Errorf("尚未支持的包类型: %s", pkg.Kind)
		}
	})
	projectsHandler.SetImports(app.imports)
	app.components = &install.ComponentStore{Root: filepath.Join(cfg.RobotRuntime.DataRoot, "installations")}
	projectsHandler.SetComponents(app.components, componentRemovalCheck(app, cfg, st, simulationSvc))
	projectsHandler.SetComponentApply(func(ctx context.Context, projectID, robotID string) error {
		if app.managedRobots == nil {
			return fmt.Errorf("当前未启用受管 Robot")
		}
		return app.managedRobots.applyInstalledComponents(ctx, projectID, robotID, simulationSvc)
	})
	app.imports.SetInstaller(componentInstaller(app, cfg, st, robotService, simulationSvc))
	robotsHandler := handlers.NewRobotsHandler(robotService, st)

	app.httpServer = &http.Server{
		Addr: cfg.Server.HTTPAddr,
		Handler: serverhttp.NewRouter(logger, authSvc, chatHandler, projectsHandler,
			settingsHandler, agentsHandler, skillsHandler, toolsHandler,
			handlers.NewTracesHandler(st, logger),
			handlers.NewMeteringHandler(st, logger),
			handlers.NewInteractionsHandler(st, logger),
			robotsHandler,
			simulationHandler),
		ReadTimeout:  time.Duration(cfg.Server.ReadTimeout),
		WriteTimeout: time.Duration(cfg.Server.WriteTimeout),
	}
	app.wsServer = &http.Server{
		Addr:              cfg.Server.WSAddr,
		Handler:           wsMux,
		ReadHeaderTimeout: wsReadHeaderTimeout,
	}
	return app, nil
}
