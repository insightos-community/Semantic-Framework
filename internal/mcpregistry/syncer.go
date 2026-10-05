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
	"reflect"
	"sync"
	"time"

	"insightos.cn/semantic-framework/pkg/log"
)

// 双频对账默认参数（取值的"为什么"见 doc.go 第 5 点）。
const (
	// defaultFastInterval 是启动期快频对账间隔。
	defaultFastInterval = 2 * time.Second

	// defaultFastRounds 是启动期快频轮数（随后进入稳态）。
	defaultFastRounds = 3

	// defaultSteadyInterval 是稳态对账周期。
	defaultSteadyInterval = 30 * time.Second
)

// SyncerOptions 是对账节奏的可调参数（零值取默认；集成测试用
// FastRounds=0 + 长 SteadyInterval 关闭周期对账，改由显式 SyncServer
// 驱动，消除后台时序对断言的干扰）。
type SyncerOptions struct {
	// FastInterval 启动期快频间隔；<=0 取 defaultFastInterval。
	FastInterval time.Duration

	// FastRounds 启动期快频轮数：<0 取 defaultFastRounds，0 关闭快频。
	FastRounds int

	// SteadyInterval 稳态对账周期；<=0 取 defaultSteadyInterval。
	SteadyInterval time.Duration
}

// syncItem 是一个 server 的同步项：独立 goroutine 跑双频对账循环，
// 并接收该 server 的 list-changed 即时同步触发。
type syncItem struct {
	// spec 同步输入快照（变更检测的比照基准）。
	spec ServerSpec

	// cancel 停止该项（Reconcile 删除/变更、Stop 全停）。
	cancel context.CancelFunc

	// notify list-changed 即时同步触发（缓冲 1：通知合并，
	// SDK 接收 goroutine 非阻塞投递，多次变化合并为一次对账）。
	notify chan struct{}
}

// Syncer 管理全部 enabled server 的目录同步生命周期：启动期对每个
// server 立即首轮对账，随后快频（2s×3）→ 稳态（30s）双频对账，
// 并桥接 tools/list_changed 通知做即时单 server 对账。
// 随 App 生命周期启停（Start/Stop）；mcp_servers 热重载经 Reconcile 对账。
type Syncer struct {
	// reg 目录（对账与条目操作的唯一出口）。
	reg *Registry

	// logger 结构化日志器。
	logger *log.Logger

	// opts 对账节奏（NewSyncer 时定型）。
	opts SyncerOptions

	// mu 保护 ctx/started/items。
	mu sync.Mutex

	// ctx Start 传入的生命周期 ctx（各 item ctx 的父）。
	ctx context.Context

	// started Start 已调用标记（Reconcile 的前置条件）。
	started bool

	// items server 名 → 同步项。
	items map[string]*syncItem

	// wg 等待全部 item goroutine 退出（Stop 用）。
	wg sync.WaitGroup
}

// NewSyncer 创建同步器（尚未启动；reg 的 list-changed 钩子在 Start 时注入）。
func NewSyncer(reg *Registry, logger *log.Logger, opts SyncerOptions) *Syncer {
	if opts.FastInterval <= 0 {
		opts.FastInterval = defaultFastInterval
	}
	if opts.FastRounds < 0 {
		opts.FastRounds = defaultFastRounds
	}
	if opts.SteadyInterval <= 0 {
		opts.SteadyInterval = defaultSteadyInterval
	}
	if logger == nil {
		logger = log.New(log.Options{})
	}
	return &Syncer{
		reg:    reg,
		logger: logger,
		opts:   opts,
		items:  make(map[string]*syncItem),
	}
}

// Start 启动同步生命周期：注入 list-changed 钩子，并按初始清单
// 对账同步项（每个 server 的首轮对账立即开始）。随 App.Run 调用。
func (s *Syncer) Start(ctx context.Context, initial []ServerSpec) {
	s.mu.Lock()
	s.ctx = ctx
	s.started = true
	s.mu.Unlock()
	// 钩子注入在 Start 而非 NewSyncer：未启动的 syncer 不消费通知
	// （如 Wire 后未 Run 的短生命周期装配）。
	s.reg.SetToolsChangedHook(s.notifyServer)
	s.Reconcile(initial)
}

// Reconcile 按目标清单对账同步项（mcp_servers 热重载的唯一入口）：
//   - 新增 server → 启动同步项（立即首轮对账）；
//   - 删除 server → 停同步项并移除目录条目（RemoveServer）；
//   - spec 变更（DeepEqual）→ 停旧项起新项——pool 以配置规范化 hash
//     memoize，连接参数变化自动新建连接，未变化则复用旧连接；
//   - 未变 → 不动（避免无意义的首轮对账冲击）。
func (s *Syncer) Reconcile(specs []ServerSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		s.logger.Warn("Syncer 未启动，忽略对账请求", "servers", len(specs))
		return
	}

	want := make(map[string]ServerSpec, len(specs))
	for _, spec := range specs {
		want[spec.Config.Name] = spec
	}

	for name, item := range s.items {
		spec, ok := want[name]
		switch {
		case !ok:
			// 删除：停同步 + 移除目录条目（配置删除才清条目，
			// 与失联标 unavailable 严格区分，doc.go 第 4 点）。
			item.cancel()
			delete(s.items, name)
			s.reg.RemoveServer(name)
			s.logger.Info("MCP server 同步项已停止（配置删除）", "server", name)
		case reflect.DeepEqual(item.spec, spec):
			delete(want, name)
		default:
			// 变更：重建同步项（首轮对账会拿新 spec 取连，
			// 连接参数变化时 pool memoize 键变化即新连接）。
			item.cancel()
			delete(s.items, name)
			s.logger.Info("MCP server 配置变更，重建同步项", "server", name)
		}
	}
	for name, spec := range want {
		s.startItemLocked(name, spec)
	}
}

// Stop 停止全部同步项并等待 goroutine 退出（App 优雅关闭路径）。
// item ctx 取消后，进行中的对账随 ctx 取消快速退出（不改健康状态）。
func (s *Syncer) Stop() {
	s.mu.Lock()
	items := s.items
	s.items = make(map[string]*syncItem)
	s.started = false
	s.mu.Unlock()
	for _, item := range items {
		item.cancel()
	}
	s.wg.Wait()
}

// startItemLocked 启动一个 server 的同步项（调用方持锁）。
func (s *Syncer) startItemLocked(name string, spec ServerSpec) {
	ctx, cancel := context.WithCancel(s.ctx)
	item := &syncItem{spec: spec, cancel: cancel, notify: make(chan struct{}, 1)}
	s.items[name] = item
	s.wg.Add(1)
	go s.runItem(ctx, name, item)
	s.logger.Info("MCP server 同步项已启动", "server", name)
}

// notifyServer 是 list-changed 通知的消费钩子（注入 Registry）：
// 非阻塞投递到对应同步项（SDK 接收 goroutine 中执行，绝不阻塞）；
// 项不存在（已删除）或已有待处理通知时丢弃——对账是全量的，合并无害。
func (s *Syncer) notifyServer(server string) {
	s.mu.Lock()
	item, ok := s.items[server]
	s.mu.Unlock()
	if !ok {
		return
	}
	select {
	case item.notify <- struct{}{}:
	default:
	}
}

// runItem 是单 server 的对账循环：立即首轮 → 快频 N 轮 → 稳态周期，
// list-changed 通知即时触发一次对账（不改变快慢频节奏）。
func (s *Syncer) runItem(ctx context.Context, name string, item *syncItem) {
	defer s.wg.Done()
	s.syncOnce(ctx, name, item.spec)

	round := 0
	for {
		interval := s.opts.SteadyInterval
		if round < s.opts.FastRounds {
			interval = s.opts.FastInterval
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-item.notify:
			timer.Stop()
			s.syncOnce(ctx, name, item.spec)
		case <-timer.C:
			round++
			s.syncOnce(ctx, name, item.spec)
		}
	}
}

// syncOnce 执行一次对账并记审计日志：失败（含保留旧条目的降级）
// 记 WARN，成功且有目录变化时由 Registry 记 INFO，此处只记失败。
func (s *Syncer) syncOnce(ctx context.Context, name string, spec ServerSpec) {
	if _, err := s.reg.SyncServer(ctx, spec); err != nil && ctx.Err() == nil {
		s.logger.Warn("MCP server 目录对账失败（旧条目保留，下轮重试）",
			"server", name, "error", err.Error())
	}
}
