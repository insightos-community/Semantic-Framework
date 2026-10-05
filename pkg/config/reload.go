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

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"insightos.cn/semantic-framework/pkg/log"
)

// reloadDebounce 是文件变更事件的去抖窗口：编辑器一次保存常触发多个
// fsnotify 事件（写、改权限、替换），合并为一个热应用周期，避免半写状态被加载。
const reloadDebounce = 500 * time.Millisecond

// Hooks 是白名单配置段的热应用回调，由 bootstrap 注入
// （pkg/config 不反向依赖 pkg/llm / internal，保持 pkg 层纯净）。
// 白名单表见本包 doc.go；未列入白名单的变更只 WARN，需重启生效。
// hook 返回错误时须保留原状态，且不得修改传入配置；成功后可能再次以旧值
// 调用以回滚本轮变更。不返回错误的 hook 也必须支持恢复旧值。
type Hooks struct {
	// OnLLMChanged llm.* 整段变更时调用（替换注册表快照 + 清空密钥缓存）。
	OnLLMChanged func(LLMConfig) error

	// OnLogLevelChanged log.level 变更时调用（logger.SetLevel 即时生效）。
	OnLogLevelChanged func(level string)

	// OnProfilesDirChanged agents.profiles_dir 变更时调用（重建 profile 加载器）。
	OnProfilesDirChanged func(dir string) error

	// OnSkillsDirChanged skills.dir 变更时调用（走 store.Reload 路径：
	// 原子替换技能快照 + 监听目录集同步切换）。
	OnSkillsDirChanged func(dir string) error

	// OnMCPServersChanged mcp_servers 段变更时调用（bootstrap 实现：
	// 结构校验后按新旧清单对账 mcpregistry 同步项——新增启动同步、
	// 删除停同步并移除目录条目、变更重建同步项）。
	OnMCPServersChanged func([]MCPServerConfig) error

	// OnExecutionChanged 热应用命令执行的服务端硬开关。
	OnExecutionChanged func(ExecutionConfig)
}

// Reloader 监听配置文件与 ./.env 的变更，经 500ms 去抖后重新加载配置，
// 按白名单热应用。校验失败时保留旧配置；钩子拒绝时回滚已应用段，
// 回滚失败时记录实际生效快照并报 ERROR。
type Reloader struct {
	// path 配置文件路径（与启动 -c 一致）。
	path string

	// dotEnvKeys 初次启动时由 ./.env 写入进程 env 的键：
	// 热重载只更新这些"自有键"，绝不触碰外部设置的进程 env。
	dotEnvKeys map[string]struct{}

	// hooks 白名单热应用回调。
	hooks Hooks

	// logger 审计日志输出（热应用 INFO、需重启 WARN、失败 ERROR）。
	logger *log.Logger

	// watcher 底层 fsnotify 监听器（挂在目标文件所在目录，
	// 兼容编辑器"先写临时文件再 rename"的替换式保存）。
	watcher *fsnotify.Watcher

	// mu 保护 cfg。
	mu sync.Mutex

	// applyMu 串行化 watcher 与 ApplyExternal 的应用、回滚和快照发布。
	applyMu sync.Mutex

	// cfg 当前生效的配置快照（热应用成功后替换）。
	cfg *Config

	// wg 等待事件循环退出，保证 Stop 后无残留 goroutine。
	wg sync.WaitGroup
}

// NewReloader 创建配置热重载器并开始监听（事件循环在 Start 后运行）。
// dotEnvKeys 为启动时 LoadDotEnv 结果中 ./.env 写入的键（DotEnvResult.LocalKeys）。
func NewReloader(path string, initial *Config, dotEnvKeys []string, hooks Hooks, logger *log.Logger) (*Reloader, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("创建配置文件监听器失败: %w", err)
	}

	// 监听目标文件所在目录而非文件本身：替换式保存（rename）会摧毁
	// 文件级 watch，目录级 watch 不受影响，按文件名过滤事件即可。
	dirs := map[string]struct{}{
		filepath.Dir(path):            {},
		filepath.Dir(localDotEnvPath): {},
	}
	for dir := range dirs {
		if err := watcher.Add(dir); err != nil {
			_ = watcher.Close()
			return nil, fmt.Errorf("监听配置目录 %s 失败: %w", dir, err)
		}
	}

	owned := make(map[string]struct{}, len(dotEnvKeys))
	for _, k := range dotEnvKeys {
		owned[k] = struct{}{}
	}
	return &Reloader{
		path:       filepath.Clean(path),
		dotEnvKeys: owned,
		hooks:      hooks,
		logger:     logger,
		watcher:    watcher,
		cfg:        initial,
	}, nil
}

// Start 启动事件循环 goroutine；可取消性由 Stop 保证（spec §3 并发纪律）。
func (r *Reloader) Start() {
	r.wg.Add(1)
	go r.loop()
	r.logger.Info("配置热重载已启动",
		"config", r.path, "dotenv", localDotEnvPath, "debounce", reloadDebounce.String())
}

// Stop 停止监听并等待事件循环退出。
func (r *Reloader) Stop() {
	_ = r.watcher.Close() // 关闭后 Events 通道随之关闭，loop 自然退出
	r.wg.Wait()
}

// Current 返回当前生效的配置快照（热应用成功后为新配置）。
func (r *Reloader) Current() *Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg
}

// ApplyExternal 将外部（settings REST PATCH）写入并校验通过的新配置经同一
// 白名单路径热应用：全部钩子成功后替换当前快照；任一钩子失败则回滚并
// 返回错误。回滚失败时快照反映实际保留的配置。调用方须已完成校验与写回。
// 文件写回触发的 watcher 事件随后到达时，diff 基于已替换的快照即为空，
// 不会重复热应用（钩子幂等，双保险）。
func (r *Reloader) ApplyExternal(next *Config) error {
	return r.applyDiff(next)
}

// loop 事件循环：收集目标文件事件，去抖后触发一次热应用周期。
func (r *Reloader) loop() {
	defer r.wg.Done()
	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case ev, ok := <-r.watcher.Events:
			if !ok {
				return // watcher 已关闭（Stop）
			}
			if !r.isTargetEvent(ev) {
				continue
			}
			// 去抖：窗口内的新事件不断推迟触发，最终只热应用一次。
			if timer == nil {
				timer = time.NewTimer(reloadDebounce)
				timerC = timer.C
			} else {
				timer.Reset(reloadDebounce)
			}
		case err, ok := <-r.watcher.Errors:
			if ok {
				r.logger.WithError(err).Warn("配置文件监听异常")
			}
		case <-timerC:
			timer = nil
			timerC = nil
			r.reload()
		}
	}
}

// isTargetEvent 过滤出配置文件与 ./.env 的实质变更事件（忽略 Chmod 等噪声）。
func (r *Reloader) isTargetEvent(ev fsnotify.Event) bool {
	if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
		return false
	}
	name := filepath.Clean(ev.Name)
	return name == r.path || name == localDotEnvPath
}

// reload 执行一次热应用周期：先同步 ./.env 自有键，再重载并校验配置，
// 最后按白名单 diff 热应用。钩子失败时尝试回滚本轮已应用的配置段。
func (r *Reloader) reload() {
	r.syncDotEnv(localDotEnvPath)

	next, err := Load(r.path)
	if err != nil {
		// 含 schema 校验失败：坏配置不进运行时。
		r.logger.WithError(err).Error("配置热重载失败，保持现有配置", "path", r.path)
		return
	}

	if err := r.applyDiff(next); err != nil {
		r.logger.WithError(err).Error("配置热重载失败", "path", r.path)
	}
}

// syncDotEnv 重新解析指定 .env 文件并只更新"自有键"：值变化则覆盖、
// 键消失则移除；外部设置的进程 env 一律不碰。
func (r *Reloader) syncDotEnv(path string) {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		data = nil // 文件被删除：下方统一按"键消失"处理
	case err != nil:
		r.logger.WithError(err).Warn(".env 热重载读取失败，跳过", "path", path)
		return
	}

	latest := make(map[string]string)
	if data != nil {
		entries, err := parseDotEnv(string(data), path)
		if err != nil {
			r.logger.WithError(err).Error(".env 热重载解析失败，保持现有环境")
			return
		}
		for _, e := range entries {
			if _, dup := latest[e.Key]; !dup {
				latest[e.Key] = e.Value // 与初次加载一致：先出现者生效
			}
		}
	}

	var updated, removed []string
	for key := range r.dotEnvKeys {
		if value, ok := latest[key]; ok {
			if os.Getenv(key) != value {
				if err := os.Setenv(key, value); err == nil {
					updated = append(updated, key)
				}
			}
		} else {
			_ = os.Unsetenv(key)
			delete(r.dotEnvKeys, key)
			removed = append(removed, key)
		}
	}
	if len(updated)+len(removed) > 0 {
		// 审计只记键名，绝不记值。
		r.logger.Info(".env 变更已热应用", "updated", updated, "removed", removed)
	}
}

// reloadStep 记录一个配置段的应用与恢复操作。
type reloadStep struct {
	name    string
	changed bool
	apply   func() error
	restore func() error
}

// newReloadStep 的 hook 必须在返回错误时保留其原状态，且不修改传入的配置。
// field 跟踪该段最后一次成功应用的值，包括回滚失败后仍实际生效的新值。
func newReloadStep[T any](name string, field *T, next T, hook func(T) error) reloadStep {
	old := *field
	set := func(value T) error {
		if hook == nil {
			return fmt.Errorf("配置段 %s 无热应用钩子，需重启生效", name)
		}
		if err := hook(value); err != nil {
			return err
		}
		*field = value
		return nil
	}
	return reloadStep{
		name: name, changed: !reflect.DeepEqual(old, next),
		apply:   func() error { return set(next) },
		restore: func() error { return set(old) },
	}
}

// infallibleReloadHook 适配不返回错误的 hook，同时保留 nil 的缺失语义。
func infallibleReloadHook[T any](hook func(T)) func(T) error {
	if hook == nil {
		return nil
	}
	return func(value T) error { hook(value); return nil }
}

// applyDiff 串行执行热应用与快照发布；失败时恢复此前已成功的配置段。
// 回滚也可能因资源不可用而失败，此时快照记录实际保留的状态并返回全部错误。
func (r *Reloader) applyDiff(next *Config) error {
	r.applyMu.Lock()
	defer r.applyMu.Unlock()

	old := r.Current()
	effective := *old
	steps := []reloadStep{
		newReloadStep("llm.*", &effective.LLM, next.LLM, r.hooks.OnLLMChanged),
		newReloadStep("log.level", &effective.Log.Level, next.Log.Level, infallibleReloadHook(r.hooks.OnLogLevelChanged)),
		newReloadStep("agents.profiles_dir", &effective.Agents.ProfilesDir, next.Agents.ProfilesDir, r.hooks.OnProfilesDirChanged),
		newReloadStep("skills.dir", &effective.Skills.Dir, next.Skills.Dir, r.hooks.OnSkillsDirChanged),
		newReloadStep("mcp_servers", &effective.MCPServers, next.MCPServers, r.hooks.OnMCPServersChanged),
		newReloadStep("execution.*", &effective.Execution, next.Execution, infallibleReloadHook(r.hooks.OnExecutionChanged)),
	}
	var applied []reloadStep
	for _, step := range steps {
		if !step.changed {
			continue
		}
		if err := step.apply(); err != nil {
			reloadErr := fmt.Errorf("配置段 %s 热应用失败: %w", step.name, err)
			// 按依赖顺序恢复：旧角色可能引用只存在于旧 LLM 注册表的模型，
			// 因此必须先恢复 LLM，再恢复 profiles，不能简单反转应用顺序。
			for _, previous := range applied {
				if err := previous.restore(); err != nil {
					rollbackErr := fmt.Errorf("配置段 %s 回滚失败: %w", previous.name, err)
					r.logger.WithError(rollbackErr).Error("配置回滚失败，快照保留该段实际生效值")
					reloadErr = errors.Join(reloadErr, rollbackErr)
				}
			}
			r.mu.Lock()
			r.cfg = &effective
			r.mu.Unlock()
			return reloadErr
		}
		applied = append(applied, step)
	}

	// 保留现有的成功快照语义：需要重启的配置仍取运行中的旧值。
	effective = *next
	effective.Server = old.Server
	effective.Store = old.Store
	effective.Agents.TeamsDir = old.Agents.TeamsDir
	r.mu.Lock()
	r.cfg = &effective
	r.mu.Unlock()

	for _, step := range applied {
		r.logger.Info("配置热应用成功", "section", step.name)
	}
	for _, key := range restartOnlyDiffs(old, next) {
		r.logger.Warn("配置项已变更，需重启生效", "key", key)
	}
	return nil
}

// restartOnlyDiffs 列出非白名单段的变更键（server.*、store.* 与
// agents.teams_dir 全部需重启：Team 组建在启动序列完成，运行中不重组）。
func restartOnlyDiffs(old, next *Config) []string {
	var keys []string
	if old.Server.HTTPAddr != next.Server.HTTPAddr {
		keys = append(keys, "server.http_addr")
	}
	if old.Server.WSAddr != next.Server.WSAddr {
		keys = append(keys, "server.ws_addr")
	}
	if old.Server.ReadTimeout != next.Server.ReadTimeout {
		keys = append(keys, "server.read_timeout")
	}
	if old.Server.WriteTimeout != next.Server.WriteTimeout {
		keys = append(keys, "server.write_timeout")
	}
	if old.Store.Driver != next.Store.Driver {
		keys = append(keys, "store.driver")
	}
	if old.Store.SQLitePath != next.Store.SQLitePath {
		keys = append(keys, "store.sqlite_path")
	}
	if old.Agents.TeamsDir != next.Agents.TeamsDir {
		keys = append(keys, "agents.teams_dir")
	}
	return keys
}
