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

package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"insightos.cn/semantic-framework/pkg/log"
)

// 默认并发与短路参数。为什么取这两个并发值：与 Claude Code 的连接
// batch size 同源——本地建连要 fork/exec 子进程，资源尖峰压到 3；
// 远程建连是网络 IO，并发价值高，放宽到 20。
const (
	// defaultLocalConcurrency 是 stdio（本地 sidecar）建连并发上限。
	defaultLocalConcurrency = 3

	// defaultRemoteConcurrency 是 http（远程/卫星）建连并发上限。
	defaultRemoteConcurrency = 20

	// defaultCircuitOpenDuration 是短路时长：与 Claude Code 认证失败短路
	// TTL（15min）同源——足够长，避免对故障 server 的重试风暴放大故障；
	// 足够短，运维修复后能较快自愈（到点半开一次试连确认）。
	defaultCircuitOpenDuration = 15 * time.Minute
)

// PoolOptions 是 Pool 的可调参数（零值取默认；测试经 Now 注入时钟）。
type PoolOptions struct {
	// LocalConcurrency stdio 建连并发上限；<=0 取 defaultLocalConcurrency。
	LocalConcurrency int

	// RemoteConcurrency http 建连并发上限；<=0 取 defaultRemoteConcurrency。
	RemoteConcurrency int

	// CircuitOpenDuration 短路时长；<=0 取 defaultCircuitOpenDuration。
	CircuitOpenDuration time.Duration

	// Now 时钟函数，测试注入以快进退短路窗口；nil 用 time.Now。
	Now func() time.Time
}

// poolEntry 是 memoize 表条目。
type poolEntry struct {
	// client 缓存的连接；短路期间为 nil。
	client *Client

	// openUntil 短路截止时间；零值表示未短路。
	openUntil time.Time
}

// Pool 是 MCP 连接池：以配置规范化 hash 为键 memoize 连接
// （同一配置全进程只建一次连接，对齐 Claude Code connectToServer 的
// memoize 语义），并负责失效剔除重建与短路熔断。
//
// Get 的健康语义：返回前对缓存连接做一次 ping——Get 是低频路径
// （R16 目录对账/调用前取连），一次 ping 换"返回即可用"的确定性，
// 不把失效连接发给调用方。
type Pool struct {
	// logger 日志输出。
	logger *log.Logger

	// opts 可调参数（NewPool 时定型，不再变化）。
	opts PoolOptions

	// mu 保护 entries。
	mu sync.Mutex

	// entries memoize 表：配置 hash → 连接条目。
	entries map[string]*poolEntry

	// localSem stdio 建连并发信号量（只限建连尖峰，已建连接不占额度）。
	localSem chan struct{}

	// remoteSem http 建连并发信号量。
	remoteSem chan struct{}
}

// NewPool 创建连接池。opts 传 nil 全取默认值。
func NewPool(logger *log.Logger, opts *PoolOptions) *Pool {
	o := PoolOptions{}
	if opts != nil {
		o = *opts
	}
	if o.LocalConcurrency <= 0 {
		o.LocalConcurrency = defaultLocalConcurrency
	}
	if o.RemoteConcurrency <= 0 {
		o.RemoteConcurrency = defaultRemoteConcurrency
	}
	if o.CircuitOpenDuration <= 0 {
		o.CircuitOpenDuration = defaultCircuitOpenDuration
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if logger == nil {
		logger = log.New(log.Options{})
	}
	return &Pool{
		logger:    logger,
		opts:      o,
		entries:   make(map[string]*poolEntry),
		localSem:  make(chan struct{}, o.LocalConcurrency),
		remoteSem: make(chan struct{}, o.RemoteConcurrency),
	}
}

// Get 复用或新建到 cfg 所描述 server 的连接：
//   - 命中健康缓存：ping 验证通过后返回同一实例；
//   - 缓存失效（调用侧报连接类错误置 broken，或 ping 失败）：剔除重建一次；
//   - 重建失败（二次失败）：进入短路——CircuitOpenDuration 内 Get 直接返回
//     ErrServerUnavailable，不再拨号；
//   - 短路到点：半开放行一次试连，成功即恢复，失败重新短路整周期；
//   - 无缓存（首次连接）：失败只返回拨号错误，不短路——没有"一次失败"
//     在先，每次 Get 都允许重试。
func (p *Pool) Get(ctx context.Context, cfg ServerConfig) (*Client, error) {
	key := configKey(cfg)

	p.mu.Lock()
	entry := p.entries[key]
	p.mu.Unlock()

	if entry == nil || entry.client == nil && entry.openUntil.IsZero() {
		// 无缓存（后者是防御分支：按不变量 client 为 nil 时必然处于短路，
		// 这里兜底按首次连接处理，避免未来重构引入空指针）。
		client, err := p.connect(ctx, cfg)
		if err != nil {
			return nil, err
		}
		p.store(key, client)
		return client, nil
	}

	if !entry.openUntil.IsZero() {
		if p.opts.Now().Before(entry.openUntil) {
			return nil, fmt.Errorf("MCP server %q 短路中（%s 后半开试连）: %w",
				cfg.Name, entry.openUntil.Format(time.RFC3339), ErrServerUnavailable)
		}
		// 半开：放行一次试连。
		return p.reconnect(ctx, key, cfg, entry)
	}

	if !entry.client.Broken() {
		if err := entry.client.ping(ctx); err == nil {
			return entry.client, nil
		}
	}
	// broken 或 ping 失败：剔除重建一次，失败即短路（二次失败）。
	return p.reconnect(ctx, key, cfg, entry)
}

// Close 关闭池内全部缓存连接并清空 memoize 表。多次调用安全。
func (p *Pool) Close() {
	p.mu.Lock()
	entries := p.entries
	p.entries = make(map[string]*poolEntry)
	p.mu.Unlock()
	for _, entry := range entries {
		if entry.client != nil {
			entry.client.Close()
		}
	}
}

// reconnect 剔除旧连接并重建一次：成功则替换缓存并返回新连接；
// 失败进入短路并返回 ErrServerUnavailable。
func (p *Pool) reconnect(ctx context.Context, key string, cfg ServerConfig, old *poolEntry) (*Client, error) {
	if old.client != nil {
		old.client.Close()
	}
	client, err := p.connect(ctx, cfg)
	if err != nil {
		p.openCircuit(key)
		p.logger.Warn("MCP server 重建失败，进入短路",
			"server", cfg.Name, "circuit_open", p.opts.CircuitOpenDuration.String(), "error", err.Error())
		return nil, fmt.Errorf("MCP server %q 连接重建失败: %w: %v", cfg.Name, ErrServerUnavailable, err)
	}
	p.store(key, client)
	p.logger.Info("MCP server 连接已重建", "server", cfg.Name)
	return client, nil
}

// connect 在对应传输的信号量内建连：只限制建连尖峰（进程拉起/握手），
// 已建立的连接不持有额度——与 Claude Code 连接 batch size 语义一致。
func (p *Pool) connect(ctx context.Context, cfg ServerConfig) (*Client, error) {
	sem := p.remoteSem
	if cfg.Transport == TransportStdio {
		sem = p.localSem
	}
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return nil, fmt.Errorf("等待建连额度时被取消: %w", ctx.Err())
	}
	return Connect(ctx, cfg)
}

// store 写入缓存：并发重建下后写者胜出，被替换的连接立即关闭，
// 保证 memoize 表任意时刻只有一个活连接。
func (p *Pool) store(key string, client *Client) {
	p.mu.Lock()
	old := p.entries[key]
	p.entries[key] = &poolEntry{client: client}
	p.mu.Unlock()
	if old != nil && old.client != nil && old.client != client {
		old.client.Close()
	}
}

// openCircuit 将某 server 置为短路态：条目保留（client 为 nil），
// 短路期内 Get 快速失败。
func (p *Pool) openCircuit(key string) {
	p.mu.Lock()
	p.entries[key] = &poolEntry{openUntil: p.opts.Now().Add(p.opts.CircuitOpenDuration)}
	p.mu.Unlock()
}

// configKey 计算配置的 memoize 键：name/transport/endpoint/command/args/env/
// timeout 全量参与（env 排序后与书写顺序无关；args 顺序敏感保持原序），
// 规范化 JSON 取 SHA-256。同配置必同键，配置任何字段变化即视为新 server。
func configKey(cfg ServerConfig) string {
	env := append([]string(nil), cfg.Env...)
	sort.Strings(env)
	// struct 字段顺序固定 + map 不参与，encoding/json 输出确定。
	data, _ := json.Marshal(struct {
		Name      string        `json:"name"`
		Transport string        `json:"transport"`
		Endpoint  string        `json:"endpoint"`
		Command   string        `json:"command"`
		Args      []string      `json:"args"`
		Env       []string      `json:"env"`
		Timeout   time.Duration `json:"timeout"`
	}{
		Name:      cfg.Name,
		Transport: cfg.Transport,
		Endpoint:  cfg.Endpoint,
		Command:   cfg.Command,
		Args:      cfg.Args,
		Env:       env,
		Timeout:   cfg.Timeout,
	})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
