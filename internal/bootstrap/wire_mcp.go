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
	"fmt"

	"insightos.cn/semantic-framework/internal/mcpregistry"
	"insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/log"
	"insightos.cn/semantic-framework/pkg/mcp"
)

// newMCPStack 装配 MCP 工具目录体系（架构 16 §4）：连接池 → 目录 → 同步器。
// 连接不在装配期建立——首轮对账随 App.Run 的 Syncer.Start 开始，
// MCP server 不可达只降级（条目标 unavailable），不阻塞启动。
func newMCPStack(logger *log.Logger, poolOpts *mcp.PoolOptions,
	syncerOpts *mcpregistry.SyncerOptions) (*mcp.Pool, *mcpregistry.Registry, *mcpregistry.Syncer) {
	pool := mcp.NewPool(logger, poolOpts)
	reg := mcpregistry.NewRegistry(pool, logger)
	syncer := mcpregistry.NewSyncer(reg, logger, syncerOptsValue(syncerOpts))
	return pool, reg, syncer
}

// syncerOptsValue 解引用可选的 SyncerOptions（nil 取默认节奏）。
func syncerOptsValue(opts *mcpregistry.SyncerOptions) mcpregistry.SyncerOptions {
	if opts == nil {
		return mcpregistry.SyncerOptions{}
	}
	return *opts
}

// mcpSpecs 把配置清单转换为目录同步输入（只含 enabled 条目；
// disabled 条目保留配置但不连接，对账语义上等价于"不在清单"）。
func mcpSpecs(servers []config.MCPServerConfig) []mcpregistry.ServerSpec {
	specs := make([]mcpregistry.ServerSpec, 0, len(servers))
	for _, srv := range servers {
		if !srv.Enabled {
			continue
		}
		specs = append(specs, mcpregistry.ServerSpec{
			Config: mcp.ServerConfig{
				Name: srv.Name, Transport: srv.Transport,
				Endpoint: srv.Endpoint, Command: srv.Command,
				Args: srv.Args, Env: srv.Env,
			},
			Risk: srv.Risk,
		})
	}
	return specs
}

// validateMCPServers 是 mcp_servers 段的结构性校验（启动装配与热重载
// 共用，fail-closed）：重名与非法 risk 是配置错误，拒绝整段。
// 传输/endpoint/command 等连接参数问题不在此拦截——卫星服务不可达
// 属于运行态降级（对账标 unavailable 并重试），不应回滚整份配置。
func validateMCPServers(servers []config.MCPServerConfig) error {
	seen := make(map[string]bool, len(servers))
	for i, srv := range servers {
		if srv.Name == "" {
			return fmt.Errorf("mcp_servers[%d] 缺少 name", i)
		}
		if seen[srv.Name] {
			return fmt.Errorf("mcp_servers 存在重名 server %q", srv.Name)
		}
		seen[srv.Name] = true
		switch srv.Risk {
		case "", tool.RiskLow, tool.RiskMedium, tool.RiskHigh, tool.RiskCritical:
		default:
			return fmt.Errorf("mcp_servers[%s].risk %q 非法（low|medium|high|critical）", srv.Name, srv.Risk)
		}
	}
	return nil
}

// applyMCPServers 热应用 mcp_servers 段（OnMCPServersChanged 钩子实现）：
// 结构校验后交给 Syncer.Reconcile 做新旧清单对账——新增 server 启动同步项
// （立即首轮对账 + 双频对账 + list-changed 即时同步）；删除的停同步项并移除
// 目录条目；spec 变更的重建同步项（pool 按配置 hash memoize，连接参数变化
// 自动新建连接）。
//
// 目录变化不需要额外广播：Agent 工具链在会话运行器构建时（runtimeFor →
// buildToolchain）重新查询目录，与 profile 热重载同一语义——已缓存的会话
// 运行器持旧工具快照继续运行，新会话/运行器重建即见新目录
// （"下一轮 PrepareAgent 生效"，架构 16 §4.3）。
func (a *App) applyMCPServers(servers []config.MCPServerConfig) error {
	if err := validateMCPServers(servers); err != nil {
		return err
	}
	a.mcpSyncer.Reconcile(mcpSpecs(servers))
	a.logger.Info("mcp_servers 已热应用", "servers", len(servers))
	return nil
}
