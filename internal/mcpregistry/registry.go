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

package mcpregistry

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/log"
	"insightos.cn/semantic-framework/pkg/mcp"
)

// 目录层结构化错误码（工具结果的 code 字段，线上协议的一部分）。
const (
	// CodeServerUnavailable server 失联或连接池短路，工具暂不可用
	// （retryable=true：恢复后可直接重试，架构 16 §7 降级语义）。
	CodeServerUnavailable = "MCP_SERVER_UNAVAILABLE"

	// CodeServerUnknown 工具所属 server 未登记在目录（防御性分支：
	// 注入与调用共享同一目录，正常链路不可达）。
	CodeServerUnknown = "MCP_SERVER_UNKNOWN"
)

// ServerSpec 是一个 enabled server 的目录同步输入：连接参数
// （pool memoize 键的计算输入）+ 目录元数据（per-server 风险覆盖）。
// 由 bootstrap 从 config.MCPServerConfig 转换而来，本包不依赖 pkg/config。
type ServerSpec struct {
	// Config 连接参数（Name 即 server 标识/命名空间）。
	Config mcp.ServerConfig

	// Risk 该 server 全部工具的风险等级覆盖；空取 defaultRisk。
	Risk string
}

// SyncReport 是一次 SyncServer 成功对账的三态结果（reconcile 输出），
// 供日志审计与测试断言；清单均为工具全名，按字典序排序（输出稳定）。
type SyncReport struct {
	// Added 本次新发现的工具。
	Added []string

	// Updated 描述/schema/风险发生变化的工具。
	Updated []string

	// Removed server 侧已下线的工具。
	Removed []string
}

// empty 报告对账是否无任何变化。
func (r SyncReport) empty() bool {
	return len(r.Added) == 0 && len(r.Updated) == 0 && len(r.Removed) == 0
}

// Registry 是 MCP 工具目录：各 server 工具条目的进程内事实源，
// 并发安全。为什么内存实现而不落库：见 doc.go 第 2 点。
//
// 健康语义：健康状态按 server 维护（一个 server 的全部条目同生共死），
// 来自两条路径——对账/调用的连接类错误标 unavailable、成功标 healthy；
// 标记只做状态迁移，从不删除条目（warning 不清仓，doc.go 第 4 点）。
type Registry struct {
	// pool 连接池（对账与调用共用；memoize/短路语义见 pkg/mcp）。
	pool *mcp.Pool

	// logger 结构化日志器。
	logger *log.Logger

	// mu 保护 entries/specs/health/onToolsChanged。
	mu sync.RWMutex

	// entries 目录条目（FullName → Entry）。
	entries map[string]Entry

	// specs server → 同步输入（CallTool 取连接参数用；
	// 与 entries 分离：连接参数不属于目录契约）。
	specs map[string]ServerSpec

	// health server → 健康状态（无记录视为 healthy——尚未对账过的
	// server 没有条目，健康值不影响任何视图）。
	health map[string]string

	// onToolsChanged tools/list_changed 消费钩子（syncer 注入）；
	// nil 时不桥接（单测直接驱动 SyncServer）。
	onToolsChanged func(server string)
}

// NewRegistry 创建目录。pool 是对账与调用的唯一连接来源（不外发原始连接）。
func NewRegistry(pool *mcp.Pool, logger *log.Logger) *Registry {
	if logger == nil {
		logger = log.New(log.Options{})
	}
	return &Registry{
		pool:    pool,
		logger:  logger,
		entries: make(map[string]Entry),
		specs:   make(map[string]ServerSpec),
		health:  make(map[string]string),
	}
}

// SetToolsChangedHook 注册 tools/list_changed 的消费钩子（syncer 注入）：
// SyncServer 成功取连后把钩子桥接到连接上（client.OnToolsChanged），
// server 推送工具集变化时 fn(server) 被调用。fn 在 SDK 接收 goroutine
// 中执行，必须快速返回（syncer 侧为非阻塞投递）。
// 为什么桥接放在 SyncServer 而不是注入连接时：连接由 pool memoize，
// 重建（失效剔除/半开试连）产生的新连接在下一次成功对账时重新桥接，
// 目录层不需要感知连接生命周期。
func (r *Registry) SetToolsChangedHook(fn func(server string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onToolsChanged = fn
}

// SyncServer 对单个 server 做一次目录对账：pool 取连 →（桥接
// list-changed 钩子）→ ListTools → reconcile 三态 → 快照替换该
// server 的条目集并标 healthy。
//
// warning 不清仓：取连或 ListTools 失败时保留全部旧条目，仅标
// unavailable（防抖动误删，doc.go 第 4 点）。调用方 ctx 取消/超时
// 不改健康状态（进程关停等运行级取消不应误标 server 失联）。
func (r *Registry) SyncServer(ctx context.Context, spec ServerSpec) (SyncReport, error) {
	name := spec.Config.Name

	r.mu.Lock()
	r.specs[name] = spec
	hook := r.onToolsChanged
	r.mu.Unlock()

	client, err := r.pool.Get(ctx, spec.Config)
	if err != nil {
		if ctx.Err() != nil {
			return SyncReport{}, ctx.Err()
		}
		r.markHealth(name, HealthUnavailable, "连接失败: "+err.Error())
		return SyncReport{}, fmt.Errorf("对账 MCP server %q 失败（保留旧条目）: %w", name, err)
	}
	if hook != nil {
		client.OnToolsChanged(func() { hook(name) })
	}

	tools, err := client.ListTools(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return SyncReport{}, ctx.Err()
		}
		r.markHealth(name, HealthUnavailable, "ListTools 失败: "+err.Error())
		return SyncReport{}, fmt.Errorf("对账 MCP server %q 失败（保留旧条目）: %w", name, err)
	}

	r.mu.Lock()
	report := r.reconcileLocked(name, spec.Risk, tools)
	r.mu.Unlock()
	r.markHealth(name, HealthHealthy, "对账成功")

	if !report.empty() {
		r.logger.Info("MCP server 目录对账有变化",
			"server", name, "added", report.Added, "updated", report.Updated, "removed", report.Removed)
	}
	return report, nil
}

// reconcileLocked 全量比对 server 侧工具清单与目录旧条目，三态分流后
// 快照替换该 server 的条目集（调用方持锁）。内容未变化的条目保留原
// UpdatedAt（对账幂等：无变化即无写）。
func (r *Registry) reconcileLocked(server, riskOverride string, tools []mcp.ToolInfo) SyncReport {
	risk := riskOverride
	if risk == "" {
		risk = defaultRisk
	}
	now := time.Now().UTC()

	// 旧条目按工具全名索引（本 server 的子集）。
	old := make(map[string]Entry)
	for fullName, e := range r.entries {
		if e.Server == server {
			old[fullName] = e
		}
	}

	var report SyncReport
	fresh := make(map[string]Entry, len(tools))
	for _, t := range tools {
		fullName := server + "." + t.Name
		desc := t.Description
		if desc == "" {
			desc = fullName // 模型侧必须有可读描述，回退为全名
		}
		schema := t.InputSchemaJSON
		if schema == "" {
			schema = defaultSchemaJSON
		}
		candidate := Entry{
			FullName: fullName, Server: server, Tool: t.Name,
			Description: desc, SchemaJSON: schema, Risk: risk,
			SourceKind: SourceKindMCP, Health: HealthHealthy, UpdatedAt: now,
		}
		prev, exists := old[fullName]
		switch {
		case !exists:
			report.Added = append(report.Added, fullName)
		case prev.Description == candidate.Description && prev.SchemaJSON == candidate.SchemaJSON &&
			prev.Risk == candidate.Risk:
			// 内容未变：保留旧条目（含原 UpdatedAt），不算 updated。
			candidate.UpdatedAt = prev.UpdatedAt
		default:
			report.Updated = append(report.Updated, fullName)
		}
		fresh[fullName] = candidate
		delete(old, fullName)
	}
	for fullName := range old {
		report.Removed = append(report.Removed, fullName)
	}

	// 快照替换：先删该 server 全部旧条目，再写入新集合。
	for fullName, e := range r.entries {
		if e.Server == server {
			delete(r.entries, fullName)
		}
	}
	for fullName, e := range fresh {
		r.entries[fullName] = e
	}

	sort.Strings(report.Added)
	sort.Strings(report.Updated)
	sort.Strings(report.Removed)
	return report
}

// List 返回目录全部条目（含 unavailable），按 FullName 排序（输出稳定）。
// 是 REST 目录视图的数据源；Agent 注入走 MatchNamespaces（只含 healthy）。
func (r *Registry) List() []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entries := make([]Entry, 0, len(r.entries))
	for _, e := range r.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].FullName < entries[j].FullName })
	return entries
}

// Get 按工具全名查询条目；第二个返回值表示是否存在。
func (r *Registry) Get(fullName string) (Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[fullName]
	return e, ok
}

// MatchNamespaces 返回命中命名空间模式且 healthy 的条目（按 FullName
// 排序），Agent 工具链注入用：unavailable 条目不进入模型视图
// （架构 16 §4.3：失联来源的工具 ToolSearch 不再返回）。
// 模式语义与 tool.Registry.MatchNamespaces 一致（"test.*" 前缀 / 精确全名）。
func (r *Registry) MatchNamespaces(patterns []string) []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var entries []Entry
	for _, e := range r.entries {
		if e.Health != HealthHealthy {
			continue
		}
		if tool.NamespaceMatch(patterns, e.FullName) {
			entries = append(entries, e)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].FullName < entries[j].FullName })
	return entries
}

// MarkUnavailable 把 server 标记为不可用（全部条目标 unavailable，不删除）。
// 健康状态的两个来源：pool 对账/调用的连接类错误、以及（未来的 provider
// 心跳）外部标记——本方法即外部标记入口（架构 16 §7 降级）。
func (r *Registry) MarkUnavailable(server string) {
	r.markHealth(server, HealthUnavailable, "外部标记")
}

// MarkHealthy 把 server 标记为健康（全部条目恢复 healthy）。
func (r *Registry) MarkHealthy(server string) {
	r.markHealth(server, HealthHealthy, "外部标记")
}

// RemoveServer 从目录移除 server：删除其全部条目与同步输入。
// 与 MarkUnavailable 的边界：配置段删除了该 server（不再是系统的一部分）
// 才移除条目；server 失联但配置仍在时一律只标 unavailable。
func (r *Registry) RemoveServer(server string) {
	r.mu.Lock()
	var removed int
	for fullName, e := range r.entries {
		if e.Server == server {
			delete(r.entries, fullName)
			removed++
		}
	}
	delete(r.specs, server)
	delete(r.health, server)
	r.mu.Unlock()
	r.logger.Info("MCP server 已从目录移除", "server", server, "removed_tools", removed)
}

// CallTool 经连接池执行一次 MCP 工具调用（kernel 适配层的执行后端）：
// spec 定位 → pool 取连 → CallTool → 结果归一为结构化文本。
// 与内置工具执行器同一出口约定：失败一律返回结构化错误结果
// （{"ok":false,...}，nil error），Go error 仅表示父 ctx 取消/超时
// （运行级取消，向上传播）。连接类失败顺带标 unavailable，
// 调用成功顺带标 healthy（自愈，不等下一轮对账）。
func (r *Registry) CallTool(ctx context.Context, server, toolName, argsJSON string) (string, error) {
	r.mu.RLock()
	spec, ok := r.specs[server]
	r.mu.RUnlock()
	if !ok {
		return tool.ErrorResult(CodeServerUnknown,
			fmt.Sprintf("MCP server %q 未登记在工具目录", server), false), nil
	}

	client, err := r.pool.Get(ctx, spec.Config)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		r.markHealth(server, HealthUnavailable, "调用前取连失败: "+err.Error())
		return tool.ErrorResult(CodeServerUnavailable,
			fmt.Sprintf("MCP server %q 暂不可用: %v", server, err), true), nil
	}

	start := time.Now()
	out, err := client.CallTool(ctx, toolName, argsJSON)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if client.Broken() || errors.Is(err, mcp.ErrServerUnavailable) {
			// 连接类错误：标记不可用并给出可重试语义（pool 会剔除重建）。
			r.markHealth(server, HealthUnavailable, "调用连接失败: "+err.Error())
			return tool.ErrorResult(CodeServerUnavailable,
				fmt.Sprintf("MCP server %q 连接中断: %v", server, err), true), nil
		}
		// 协议/工具错误：server 本身健康，错误交回 Agent 决策（不重试）。
		r.logger.Warn("MCP 工具调用失败", "server", server, "tool", toolName,
			"duration_ms", time.Since(start).Milliseconds(), "error", err.Error())
		return tool.ErrorResult(tool.CodeToolError, err.Error(), false), nil
	}

	r.markHealth(server, HealthHealthy, "调用成功")
	r.logger.Info("MCP 工具调用完成", "server", server, "tool", toolName,
		"duration_ms", time.Since(start).Milliseconds())
	return tool.OKResult(out)
}

// markHealth 迁移 server 健康状态并同步其全部条目的 Health 字段。
// 只有真实迁移才记日志（unavailable 记 WARN、恢复记 INFO）——
// 对账/调用的高频路径不产生重复日志。
func (r *Registry) markHealth(server, health, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.health[server]
	if prev == health {
		return
	}
	r.health[server] = health
	for fullName, e := range r.entries {
		if e.Server == server {
			e.Health = health
			r.entries[fullName] = e
		}
	}
	switch {
	case health == HealthUnavailable:
		r.logger.Warn("MCP server 标记为不可用（条目保留）",
			"server", server, "reason", reason)
	case prev == HealthUnavailable:
		r.logger.Info("MCP server 恢复健康", "server", server, "reason", reason)
	}
}
