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
	"context"
	"errors"
	stopport "insightos.cn/semantic-framework/internal/ports/stop"
	"net/http"
	"time"

	"insightos.cn/semantic-framework/internal/agent/runtime"
	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/mcpregistry"
	"insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/server/aggregate"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/simulation"
	"insightos.cn/semantic-framework/internal/skill"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/internal/workflow"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/llm"
	"insightos.cn/semantic-framework/pkg/log"
	"insightos.cn/semantic-framework/pkg/mcp"
)

// shutdownTimeout 是优雅退出的最长等待时间，超过后强制关闭连接。
const shutdownTimeout = 5 * time.Second

// App 是装配完成的 semantic-server 应用，持有运行所需的全部组件。
type App struct {
	cfg    *config.Config
	logger *log.Logger

	// store 元数据存储，退出时负责关闭。
	store *store.Store

	// llmReg LLM 提供方注册表（缺 key 的端点已降级 WARN）。
	llmReg *llm.Registry

	// bus 进程内事件总线，域模块事件的入口。
	bus *event.Bus

	// hub WS 连接中枢，退出时负责逐条关闭连接。
	hub *ws.Hub

	// aggregator 会话聚合器（bus → 归一化/分级/落库 → hub 的唯一通道）。
	aggregator *aggregate.Aggregator

	// runtime Agent 运行时（对话闭环服务）。
	runtime *runtime.Service

	// workflow 显式 Plan Mode、Task 调度与停止恢复服务。
	workflow *workflow.Service

	// robots 管理 Robot Skill 包、多 Pilot、Execution 与 Artifact 对账。

	// simulation 管理 v0.4 场景和 Runtime 生命周期；Robot 执行链不能越过该边界
	// 直接启停场景或仿真引擎进程。
	simulation    *simulation.Service
	robots        *robot.Service
	managedRobots *managedSceneRobotLifecycle

	// toolRegistry 工具注册表（运行期只读；集成测试经 ToolRegistry 访问）。
	toolRegistry *tool.Registry

	// skills 技能存储（nil = 无技能形态）；热更监听随优雅关闭停止。
	skills *skill.Store

	// mcpPool MCP 连接池（优雅关闭时关闭全部缓存连接）。
	mcpPool *mcp.Pool

	// mcpReg MCP 工具目录（Agent 工具链注入与 REST 目录视图的数据源）。
	mcpReg *mcpregistry.Registry

	// mcpSyncer 目录同步器（随 Run 启动、随优雅关闭停止）。
	mcpSyncer *mcpregistry.Syncer

	// reloader 配置热重载器（nil 表示未开启）；随 Run 启动、随优雅关闭停止。
	reloader *config.Reloader

	// settingsCtl 设置控制器（settings REST 的配置读写能力）：
	// GET 快照取自 currentConfig；PATCH 写回路径由 EnableConfigReload 装配。
	settingsCtl *settingsController

	// httpServer 承载 /api/v1 REST 面。
	httpServer *http.Server

	// wsServer 承载 /ws/agent-events 事件通道。
	wsServer *http.Server

	// mdnsShutdown 只由 semantic-server 正式入口启用；测试装配不广播局域网服务。
	mdnsShutdown func()
}

// EventBus 返回应用的事件总线，供域模块（与集成测试）发布事件。
func (a *App) EventBus() *event.Bus {
	return a.bus
}

// LLMRegistry 返回 LLM 提供方注册表，供域模块（与集成测试）构建模型。
func (a *App) LLMRegistry() *llm.Registry {
	return a.llmReg
}

// Store 返回元数据存储，供后续 API 面（与集成测试）查询落库记录。
func (a *App) Store() *store.Store {
	return a.store
}

// Runtime 返回 Agent 运行时服务，供域模块（与集成测试）触发对话闭环。
func (a *App) Runtime() *runtime.Service {
	return a.runtime
}

// Workflow 返回 v0.3 计划与任务应用服务，供接入层与集成测试使用。
func (a *App) Workflow() *workflow.Service {
	return a.workflow
}

// Robots 返回 v0.5 Robot Server 应用服务。
func (a *App) Robots() *robot.Service { return a.robots }

// Simulation 返回仿真编排服务，供接入层和集成测试查询场景状态。
func (a *App) Simulation() *simulation.Service {
	return a.simulation
}

// SkillStore 返回技能存储，供域模块（与集成测试）断言技能装配；
// nil 表示无技能形态（启动时技能目录不可用）。
func (a *App) SkillStore() *skill.Store {
	return a.skills
}

// ToolRegistry 返回工具注册表，供域模块（与集成测试）断言工具目录；
// 注册表并发安全，运行期注册（如 MCP 来源接入）对后续构建的
// agent 工具链生效（buildToolchain 每次按 profile 重新过滤）。
func (a *App) ToolRegistry() *tool.Registry {
	return a.toolRegistry
}

// MCPRegistry 返回 MCP 工具目录，供 REST 目录视图（与集成测试）查询。
func (a *App) MCPRegistry() *mcpregistry.Registry {
	return a.mcpReg
}

// currentConfig 返回当前生效的配置快照：热重载启用时为 reloader 快照
// （只记录实际生效的配置），否则为启动加载的配置。
// 是 settings REST 与热重载共用的"生效配置"唯一事实源。
// EnableMDNSDiscovery 由 semantic-server 生产入口调用。bootstrap 测试和进程内
// 集成测试不会调用它，因此默认 CI 不发送局域网广播。
func (a *App) EnableMDNSDiscovery() error {
	advertiser, err := startMDNSAdvertiser(a.cfg)
	if err != nil {
		return err
	}
	a.mdnsShutdown = advertiser.Shutdown
	return nil
}

func (a *App) currentConfig() *config.Config {
	if a.reloader != nil {
		return a.reloader.Current()
	}
	return a.cfg
}

// Run 同时启动 HTTP 与 WS 两个监听并阻塞，直到 ctx 取消、收到
// SIGINT/SIGTERM 信号或任一服务出错。正常退出路径优雅关闭两个
// server、全部 WS 连接与 store。
func (a *App) Run(ctx context.Context) error {
	// signal.NotifyContext 将信号转换为 ctx 取消，与调用方传入的 ctx 统一处理。
	ctx, stop, err := stopport.NotifyContext(ctx)
	if err != nil {
		return err
	}
	defer stop()

	// 启动聚合器：域模块发布到 TopicAgentEvents 的事件经归一化/分级/落库
	// 后由 hub 按 session_id 投递/广播下行。聚合器随 Run 退出而停止。
	aggCtx, aggCancel := context.WithCancel(context.Background())
	defer aggCancel()
	aggDone := make(chan struct{})
	go func() {
		defer close(aggDone)
		a.aggregator.Run(aggCtx)
	}()
	// stopAggregator 在关闭 SQLite 前等待聚合器完全退出。只有取消而不等待，
	// 聚合器可能在数据库文件开始备份或清理后再次创建 journal/WAL 文件。
	stopAggregator := func() {
		aggCancel()
		select {
		case <-aggDone:
		case <-time.After(shutdownTimeout):
			a.logger.Warn("事件聚合器未在关闭时限内退出")
		}
	}

	// MCP 目录同步随 Run 启动（先于热重载 watcher：钩子触发时 syncer
	// 必须已就绪）。首轮对账在此开始，MCP server 不可达只降级不阻塞。
	mcpCtx, mcpCancel := context.WithCancel(context.Background())
	defer mcpCancel()
	a.mcpSyncer.Start(mcpCtx, mcpSpecs(a.cfg.MCPServers))

	// 配置热重载 watcher 随 Run 启动（优雅关闭时在 shutdown 中停止）。
	if a.reloader != nil {
		a.reloader.Start()
	}

	errCh := make(chan error, 2)
	go func() { errCh <- a.httpServer.ListenAndServe() }()
	go func() { errCh <- a.wsServer.ListenAndServe() }()
	a.logger.Info("HTTP 服务已启动", "addr", a.httpServer.Addr)
	a.logger.Info("WS 服务已启动", "addr", a.wsServer.Addr)

	select {
	case <-ctx.Done():
		a.shutdown(stopAggregator)
		return nil
	case err := <-errCh:
		// ListenAndServe 在服务正常关闭时返回 http.ErrServerClosed，不视为错误。
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		a.logger.WithError(err).Error("服务异常退出")
		// 一个监听失败时关闭另一个与 store，避免半启动状态残留。
		a.shutdown(stopAggregator)
		return err
	}
}

// shutdown 按序优雅关闭：先停两个 listener（拒绝新连接），再关闭全部
// WS 连接（hijack 后的连接不受 Server.Shutdown 管理，必须经 Hub 主动关），
// 最后关闭 store。单步失败只记录不中断，保证后续资源仍能释放。
func (a *App) shutdown(beforeStoreClose func()) {
	a.logger.Info("开始优雅退出", "timeout", shutdownTimeout.String())
	if a.mdnsShutdown != nil {
		a.mdnsShutdown()
		a.mdnsShutdown = nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// 先停配置热重载 watcher，避免关闭过程中仍热应用变更。
	if a.reloader != nil {
		a.reloader.Stop()
	}
	// 技能目录热更监听同样先停；Stop 只停止监听，已有快照在运行收尾期间仍可读。
	if a.skills != nil {
		a.skills.Stop()
	}

	if err := a.httpServer.Shutdown(ctx); err != nil {
		a.logger.WithError(err).Error("HTTP 服务优雅退出失败")
	}
	// HTTP 入口关闭后不会再接收新的场景或 Robot 请求，但 Pilot 的 WS 必须继续
	// 保持在线，直到受管实例按 Pilot → Ability → AbilityFramework 的顺序退出。
	// 如果先关闭 WS，Pilot 会被误判离线，supervisor 只能留下 interrupted 实例
	// 和孤立的 AbilityFramework，下一次启动也会继续读到冲突状态。
	if a.managedRobots != nil || a.simulation != nil {
		robotContext, cancelRobots := context.WithTimeout(context.Background(), managedRobotShutdownTimeout)
		if a.managedRobots != nil {
			if err := a.managedRobots.Shutdown(robotContext); err != nil {
				a.logger.WithError(err).Error("受管 Robot 实例优雅退出失败")
			}
		}
		// Runtime必须晚于Pilot、Ability和AbilityFramework退出。外部共享
		// Runtime没有受管进程句柄，Shutdown不会改变它的生命周期。
		if a.simulation != nil {
			if err := a.simulation.Shutdown(robotContext); err != nil {
				a.logger.WithError(err).Error("受管 Simulation Runtime 优雅退出失败")
			}
		}
		cancelRobots()
	}
	if err := a.wsServer.Shutdown(ctx); err != nil {
		a.logger.WithError(err).Error("WS 服务优雅退出失败")
	}
	a.hub.CloseAll()
	// 后台 Planning Run 使用 Agent Runtime。先取消并等待它们退出，再关闭
	// Runtime 和 Store，避免规划收尾写入已关闭资源。
	if a.workflow != nil {
		a.workflow.Shutdown()
	}
	// 等待当前请求级 Agent Run 停止后再关闭 Store，避免运行收尾写入已关闭的数据库。
	a.runtime.Shutdown()
	// 停 MCP 目录同步并关闭连接池：在 runtime 之后（运行中的 agent
	// 可能仍在调 MCP 工具），在 store 之前（对账日志仍需要落盘能力）。
	a.mcpSyncer.Stop()
	a.mcpPool.Close()
	// 聚合器必须在 Store 关闭前停止并完成最后一次写入；调用方负责提供
	// 关闭函数，便于 Run 持有并等待对应 goroutine 的完成信号。
	if beforeStoreClose != nil {
		beforeStoreClose()
	}
	if err := a.store.Close(); err != nil {
		a.logger.WithError(err).Error("元数据存储关闭失败")
	} else {
		a.logger.Info("元数据存储已关闭")
	}
	a.logger.Info("服务已退出")
}
