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

package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"insightos.cn/semantic-framework/pkg/log"
)

// watchDebounce 是文件变更事件的去抖窗口：编辑器一次保存常触发多个
// fsnotify 事件（写/改权限/替换），合并为一次 Reload，避免半写状态被加载
// （与 pkg/config 热重载同一策略）。
const watchDebounce = 500 * time.Millisecond

// Store 是技能注册中心（skill store，架构文档 06 §2）：技能目录的快照 +
// 热更新监听。
//
// 快照语义：Reload 全量重建后原子替换（RWMutex 保护），读路径
// （List/Get/Summary）永远看到一致的一份快照；热更新下一轮 run 生效
// （内核 skill middleware 每次 run 重新读快照渲染清单与加载正文）。
type Store struct {
	// dir 当前技能目录（Reload 可换目录，监听目录集随快照同步切换）。
	dir string

	// logger 结构化日志器（加载告警与热更审计）。
	logger *log.Logger

	// mu 保护 dir/skills/watcher/watched。
	mu sync.RWMutex

	// skills 当前快照（name → Skill，整体原子替换，读者不改写）。
	skills map[string]Skill

	// watcher 技能目录监听器（Start 后非 nil）。
	watcher *fsnotify.Watcher

	// watched 当前挂载监听的目录集（Reload 后增量同步）。
	watched map[string]struct{}

	// wg 等待监听 goroutine 退出（Stop 后无残留）。
	wg sync.WaitGroup
}

// NewStore 创建技能存储并立即加载一次：dir 不存在/不是目录返回错误
// （调用方决定降级策略）；单个技能文件失败只记 WARN（软错误，见 LoadDir）。
func NewStore(dir string, logger *log.Logger) (*Store, error) {
	s := &Store{dir: dir, logger: logger, skills: map[string]Skill{}}
	if err := s.Reload(dir); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload 从 dir 全量重载技能并原子替换快照：dir 不可读返回错误且保留
// 旧快照（一次坏变更不打掉在用的技能集）；单个技能失败只记 WARN 继续
// （软错误）。加载成功后同步监听目录集（Start 后），技能热更与
// skills.dir 配置热重载都走本路径。
func (s *Store) Reload(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("技能目录 %s 不可读: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("技能路径 %s 不是目录", dir)
	}

	skills, errs := LoadDir(dir)
	for _, loadErr := range errs {
		s.logger.WithError(loadErr).Warn("技能加载失败，已跳过")
	}

	snap := make(map[string]Skill, len(skills))
	for _, sk := range skills {
		if prev, ok := snap[sk.Name]; ok {
			// 同名冲突后加载覆盖先加载（WalkDir 字典序，结果确定）并 WARN：
			// 技能名是全局标识，冲突多半是新版本覆盖旧文件的部署笔误。
			s.logger.Warn("技能重名，后加载的覆盖先加载的",
				"name", sk.Name, "kept_dir", sk.Dir, "dropped_dir", prev.Dir)
		}
		snap[sk.Name] = sk
	}

	s.mu.Lock()
	s.dir = dir
	s.skills = snap
	s.mu.Unlock()

	if err := s.syncWatch(); err != nil {
		// 监听同步失败不影响快照生效：降级为不热更，只记 WARN。
		s.logger.WithError(err).Warn("技能目录监听同步失败，热更不生效", "dir", dir)
	}
	s.logger.Info("技能快照已替换", "dir", dir, "skills", len(snap), "load_errors", len(errs))
	return nil
}

// List 返回当前快照的全部技能（按名升序，渲染稳定）。
func (s *Store) List() []Skill {
	s.mu.RLock()
	defer s.mu.RUnlock()
	skills := make([]Skill, 0, len(s.skills))
	for _, sk := range s.skills {
		skills = append(skills, sk)
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills
}

// Get 按名取技能；未命中返回 false。
func (s *Store) Get(name string) (Skill, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sk, ok := s.skills[name]
	return sk, ok
}

// Summary 渲染技能清单摘要文本（渐进披露的常驻段）：按 category 分组，
// 每技能一行 "- name: description (category)"；分组与组内均按名升序
// （渲染稳定，工具描述逐 run 重建时内容不因 map 序抖动）。无技能返回空串。
func (s *Store) Summary() string {
	return renderSummary(s.List())
}

// View 是全局 Skill Store 的只读过滤视图。它不复制 Skill 正文，因此源 Store
// 热更新后下一次 List/Get 会立即读取新快照，同时始终受 Agent 与 Project
// 两层白名单约束。
type View struct {
	// source 是全局 Skill 快照来源。
	source *Store

	// allowed 是 Agent allowlist 与 Project 绑定计算后的有效名称集合。
	allowed map[string]struct{}
}

// NewView 创建有效 Skill 视图。Agent allowlist 是硬边界，空列表表示该 Agent
// 不使用 Skill；Project 绑定为空表示不增加 Project 级限制，非空时继续取交集。
func NewView(source *Store, agentAllowlist, projectBindings []string) *View {
	view := &View{source: source, allowed: make(map[string]struct{})}
	if source == nil || len(agentAllowlist) == 0 {
		return view
	}
	projectAllowed := make(map[string]struct{}, len(projectBindings))
	for _, name := range projectBindings {
		name = strings.TrimSpace(name)
		if name != "" {
			projectAllowed[name] = struct{}{}
		}
	}
	for _, name := range agentAllowlist {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if len(projectAllowed) > 0 {
			if _, ok := projectAllowed[name]; !ok {
				continue
			}
		}
		view.allowed[name] = struct{}{}
	}
	return view
}

// List 返回源快照中当前仍存在且已授权的 Skill，顺序继承 Store 的按名排序。
func (v *View) List() []Skill {
	if v == nil || v.source == nil || len(v.allowed) == 0 {
		return []Skill{}
	}
	result := make([]Skill, 0, len(v.allowed))
	for _, item := range v.source.List() {
		if _, ok := v.allowed[item.Name]; ok {
			result = append(result, item)
		}
	}
	return result
}

// Get 只返回有效集内的 Skill；即使源 Store 存在同名 Skill，未授权时也按未命中处理。
func (v *View) Get(name string) (Skill, bool) {
	if v == nil || v.source == nil {
		return Skill{}, false
	}
	if _, ok := v.allowed[name]; !ok {
		return Skill{}, false
	}
	return v.source.Get(name)
}

// Summary 渲染过滤后的渐进披露清单。
func (v *View) Summary() string {
	if v == nil {
		return ""
	}
	return renderSummary(v.List())
}

// renderSummary 为 Store 与过滤视图复用同一份稳定摘要格式。
func renderSummary(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}

	groups := make(map[string][]Skill)
	var order []string
	for _, sk := range skills {
		if _, ok := groups[sk.Category]; !ok {
			order = append(order, sk.Category)
		}
		groups[sk.Category] = append(groups[sk.Category], sk)
	}
	sort.Strings(order)

	var b strings.Builder
	for i, cat := range order {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("### " + cat + "\n")
		for _, sk := range groups[cat] {
			b.WriteString("- " + sk.Name + ": " + sk.Description + " (" + sk.Category + ")\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// Dir 返回当前技能目录（热更循环与热重载钩子的 Reload 入参）。
func (s *Store) Dir() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dir
}

// Start 启动技能目录监听：fsnotify 挂载 dir 下全部子目录（技能以目录为
// 单位，新增技能 = 新目录 + SKILL.md，父目录事件可见），500ms 去抖后自动
// Reload。重复调用返回错误。
// 已知限制：技能目录被整体删除后，需经 Reload（配置热重载钩子）或重启恢复，
// 目录重建本身不再触发（根目录的 watch 随删除失效）。
func (s *Store) Start() error {
	s.mu.Lock()
	if s.watcher != nil {
		s.mu.Unlock()
		return fmt.Errorf("技能目录监听已启动")
	}
	s.mu.Unlock()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("创建技能目录监听器失败: %w", err)
	}
	s.mu.Lock()
	s.watcher = watcher
	s.watched = map[string]struct{}{}
	s.mu.Unlock()

	if err := s.syncWatch(); err != nil {
		_ = watcher.Close()
		s.mu.Lock()
		s.watcher = nil
		s.watched = nil
		s.mu.Unlock()
		return err
	}

	s.wg.Add(1)
	go s.watchLoop()
	s.logger.Info("技能目录热更监听已启动", "dir", s.Dir(), "debounce", watchDebounce.String())
	return nil
}

// Stop 停止监听并等待 goroutine 退出；未 Start 时为 no-op。
// 快照在 Stop 后仍可正常读取（只停热更，不清数据）。
func (s *Store) Stop() {
	s.mu.RLock()
	watcher := s.watcher
	s.mu.RUnlock()
	if watcher == nil {
		return
	}
	_ = watcher.Close() // 关闭后 Events 通道随之关闭，watchLoop 自然退出
	s.wg.Wait()
}

// syncWatch 把监听目录集同步为当前技能目录的全部子目录（含根）：
// Reload 换目录/增删技能目录后调用，保证新技能目录的文件事件可被捕获。
// 未 Start 时为 no-op。
func (s *Store) syncWatch() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watcher == nil {
		return nil
	}

	want := map[string]struct{}{}
	_ = filepath.WalkDir(s.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if path != s.dir && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		want[path] = struct{}{}
		return nil
	})

	for dir := range want {
		if _, ok := s.watched[dir]; ok {
			continue
		}
		if err := s.watcher.Add(dir); err != nil {
			return fmt.Errorf("监听技能目录 %s 失败: %w", dir, err)
		}
		s.watched[dir] = struct{}{}
	}
	for dir := range s.watched {
		if _, ok := want[dir]; !ok {
			_ = s.watcher.Remove(dir)
			delete(s.watched, dir)
		}
	}
	return nil
}

// watchLoop 事件循环：收集技能目录的文件事件，去抖后触发一次 Reload。
// 只过滤 Chmod 噪声：技能变更包括 SKILL.md 写入与目录增删，统一在去抖后
// 全量重扫最可靠——按文件名过滤反而漏掉"新增技能目录"这类结构性变更。
func (s *Store) watchLoop() {
	defer s.wg.Done()
	s.mu.RLock()
	watcher := s.watcher
	s.mu.RUnlock()

	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case ev, ok := <-watcher.Events:
			if !ok {
				return // watcher 已关闭（Stop）
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			// 去抖：窗口内的新事件不断推迟触发，最终只 Reload 一次。
			if timer == nil {
				timer = time.NewTimer(watchDebounce)
				timerC = timer.C
			} else {
				timer.Reset(watchDebounce)
			}
		case err, ok := <-watcher.Errors:
			if ok {
				s.logger.WithError(err).Warn("技能目录监听异常")
			}
		case <-timerC:
			timer = nil
			timerC = nil
			if err := s.Reload(s.Dir()); err != nil {
				s.logger.WithError(err).Error("技能热重载失败，保留旧快照", "dir", s.Dir())
			}
		}
	}
}
