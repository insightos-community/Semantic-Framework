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
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"insightos.cn/semantic-framework/pkg/log"
)

// syncBuffer 是并发安全的日志输出缓冲（事件循环 goroutine 与断言并发读写）。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write 实现 io.Writer，追加一行日志。
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String 返回当前累积的全部日志文本。
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// hookRecorder 记录白名单钩子的调用次数与末次参数（并发安全）。
type hookRecorder struct {
	mu          sync.Mutex
	llmCalls    int
	lastLLM     LLMConfig
	levelCalls  int
	lastLevel   string
	profileDirs []string
	mcpCalls    int
	lastMCP     []MCPServerConfig
}

// Hooks 返回绑定到该记录器的 Hooks 实例。
func (h *hookRecorder) Hooks() Hooks {
	return Hooks{
		OnLLMChanged: func(cfg LLMConfig) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.llmCalls++
			h.lastLLM = cfg
			return nil
		},
		OnLogLevelChanged: func(level string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.levelCalls++
			h.lastLevel = level
		},
		OnProfilesDirChanged: func(dir string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.profileDirs = append(h.profileDirs, dir)
			return nil
		},
		OnMCPServersChanged: func(servers []MCPServerConfig) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.mcpCalls++
			h.lastMCP = servers
			return nil
		},
	}
}

// snapshot 返回当前计数快照，供断言轮询。
func (h *hookRecorder) snapshot() (llm, level int, dirs []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.llmCalls, h.levelCalls, append([]string(nil), h.profileDirs...)
}

// mcpSnapshot 返回 mcp_servers 钩子的调用次数与末次参数，供断言轮询。
func (h *hookRecorder) mcpSnapshot() (int, []MCPServerConfig) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.mcpCalls, append([]MCPServerConfig(nil), h.lastMCP...)
}

// waitFor 轮询 cond 直到成立或超时（3s），超时报测试失败。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// newTestReloader 在临时目录写入初始配置并创建已 Start 的 Reloader。
func newTestReloader(t *testing.T, initialYAML string) (*Reloader, *hookRecorder, *syncBuffer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "semantic-server.yaml")
	if err := os.WriteFile(path, []byte(initialYAML), 0o644); err != nil {
		t.Fatalf("写入初始配置失败: %v", err)
	}
	initial, err := Load(path)
	if err != nil {
		t.Fatalf("加载初始配置失败: %v", err)
	}
	rec := &hookRecorder{}
	logBuf := &syncBuffer{}
	logger := log.New(log.Options{Level: log.LevelTrace, Writer: logBuf})
	reloader, err := NewReloader(path, initial, nil, rec.Hooks(), logger)
	if err != nil {
		t.Fatalf("NewReloader 失败: %v", err)
	}
	reloader.Start()
	t.Cleanup(reloader.Stop)
	return reloader, rec, logBuf, path
}

// rewriteConfig 以替换式写入更新配置文件（模拟编辑器保存）。
func rewriteConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入配置失败: %v", err)
	}
}

// TestReloadDebounceMerges 验证去抖窗口内的连续写入只触发一次热应用，
// 且应用的是最后一次写入的值。
func TestReloadDebounceMerges(t *testing.T) {
	_, rec, _, path := newTestReloader(t, "log:\n  level: info\n")

	rewriteConfig(t, path, "log:\n  level: debug\n")
	time.Sleep(100 * time.Millisecond) // 窗口内第二次写入
	rewriteConfig(t, path, "log:\n  level: warn\n")

	waitFor(t, "log.level 热应用一次", func() bool {
		_, levelCalls, _ := rec.snapshot()
		return levelCalls == 1
	})
	rec.mu.Lock()
	if rec.lastLevel != "warn" {
		t.Errorf("热应用的应是末次写入值 warn，实际: %q", rec.lastLevel)
	}
	rec.mu.Unlock()

	// 窗口过后不应再有第二次触发。
	time.Sleep(reloadDebounce + 300*time.Millisecond)
	if _, levelCalls, _ := rec.snapshot(); levelCalls != 1 {
		t.Errorf("去抖后应只触发 1 次，实际: %d", levelCalls)
	}
}

// TestReloadWhitelistLLM 验证 llm.* 变更命中白名单并热应用。
func TestReloadWhitelistLLM(t *testing.T) {
	reloader, rec, _, path := newTestReloader(t, "")

	rewriteConfig(t, path, "llm:\n  providers:\n    mock:\n      component: mock\n      model: mock\n      capabilities: [text]\n")
	waitFor(t, "llm.* 热应用", func() bool {
		llmCalls, _, _ := rec.snapshot()
		return llmCalls == 1
	})

	rec.mu.Lock()
	if got := rec.lastLLM.Providers["mock"].Capabilities; len(got) != 1 || got[0] != "text" {
		t.Errorf("热应用后的 mock capabilities 应为 [text]，实际: %v", got)
	}
	rec.mu.Unlock()
	if got := reloader.Current().LLM.Providers["mock"].Capabilities; len(got) != 1 || got[0] != "text" {
		t.Errorf("Current 快照应持有新的 mock capabilities，实际: %v", got)
	}
}

// TestReloadRestartOnlyWarns 验证非白名单变更只 WARN 不热应用，
// 且当前快照不被替换。
func TestReloadRestartOnlyWarns(t *testing.T) {
	reloader, rec, logBuf, path := newTestReloader(t, "")

	rewriteConfig(t, path, "server:\n  http_addr: \":19999\"\n")
	waitFor(t, "需重启 WARN 出现", func() bool {
		return strings.Contains(logBuf.String(), "需重启生效") &&
			strings.Contains(logBuf.String(), "server.http_addr")
	})

	if llmCalls, levelCalls, _ := rec.snapshot(); llmCalls+levelCalls != 0 {
		t.Errorf("非白名单变更不应触发热应用钩子，实际 llm=%d level=%d", llmCalls, levelCalls)
	}
	if got := reloader.Current().Server.HTTPAddr; got != ":8080" {
		t.Errorf("非白名单变更不应替换快照，实际 http_addr: %q", got)
	}
}

// TestReloadWhitelistMCPServers 验证 mcp_servers 段变更命中白名单：
// 触发 OnMCPServersChanged 钩子并替换当前快照。
func TestReloadWhitelistMCPServers(t *testing.T) {
	reloader, rec, _, path := newTestReloader(t, "")

	rewriteConfig(t, path, "mcp_servers:\n  - name: map\n    transport: http\n    endpoint: \"http://127.0.0.1:8082/mcp\"\n    enabled: true\n    namespace: map\n")
	waitFor(t, "mcp_servers 热应用", func() bool {
		calls, _ := rec.mcpSnapshot()
		return calls == 1
	})

	_, servers := rec.mcpSnapshot()
	if len(servers) != 1 || servers[0].Name != "map" || !servers[0].Enabled || servers[0].Namespace != "map" {
		t.Errorf("热应用的 mcp_servers 内容不符: %+v", servers)
	}
	if got := reloader.Current().MCPServers; len(got) != 1 || got[0].Endpoint != "http://127.0.0.1:8082/mcp" {
		t.Errorf("Current 快照应已替换, 实际: %+v", got)
	}
}

// TestReloadInvalidConfigKept 验证坏配置（未知键）不进入运行时：
// 记 ERROR、保持旧快照、不触发任何钩子。
func TestReloadInvalidConfigKept(t *testing.T) {
	reloader, rec, logBuf, path := newTestReloader(t, "")

	rewriteConfig(t, path, "server:\n  http-addr: \":1\"\n")
	waitFor(t, "热重载失败 ERROR 出现", func() bool {
		return strings.Contains(logBuf.String(), "配置热重载失败")
	})

	if llmCalls, levelCalls, dirs := rec.snapshot(); llmCalls+levelCalls+len(dirs) != 0 {
		t.Errorf("坏配置不应触发任何钩子，实际 llm=%d level=%d dirs=%v", llmCalls, levelCalls, dirs)
	}
	if got := reloader.Current().Log.Level; got != "info" {
		t.Errorf("坏配置不应替换快照，实际 log.level: %q", got)
	}
}

// TestSyncDotEnvOwnedKeys 验证 .env 热同步只更新"自有键"：
// 值变化覆盖、键消失移除、外部进程 env 不被触碰。
func TestSyncDotEnvOwnedKeys(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte("SEMANTIC_TEST_SYNC_OWNED=old\nSEMANTIC_TEST_SYNC_GONE=bye\n"), 0o644); err != nil {
		t.Fatalf("写入 .env 失败: %v", err)
	}
	t.Setenv("SEMANTIC_TEST_SYNC_OWNED", "old")
	t.Setenv("SEMANTIC_TEST_SYNC_GONE", "bye")
	t.Setenv("SEMANTIC_TEST_SYNC_EXTERNAL", "keep")

	r := &Reloader{
		dotEnvKeys: map[string]struct{}{
			"SEMANTIC_TEST_SYNC_OWNED": {},
			"SEMANTIC_TEST_SYNC_GONE":  {},
		},
		logger: log.New(log.Options{Level: log.LevelTrace, Writer: &syncBuffer{}}),
	}

	// 自有键改值 + 另一自有键消失；外部键出现在文件中但未被跟踪。
	if err := os.WriteFile(envPath, []byte("SEMANTIC_TEST_SYNC_OWNED=new\nSEMANTIC_TEST_SYNC_EXTERNAL=hacked\n"), 0o644); err != nil {
		t.Fatalf("重写 .env 失败: %v", err)
	}
	r.syncDotEnv(envPath)

	if got := os.Getenv("SEMANTIC_TEST_SYNC_OWNED"); got != "new" {
		t.Errorf("自有键应被更新为 new，实际: %q", got)
	}
	if _, ok := os.LookupEnv("SEMANTIC_TEST_SYNC_GONE"); ok {
		t.Error("消失的自有键应被移除")
	}
	if got := os.Getenv("SEMANTIC_TEST_SYNC_EXTERNAL"); got != "keep" {
		t.Errorf("未被跟踪的外部键不应被 .env 改写，实际: %q", got)
	}
}

// TestReloadFileHookFailure 验证文件重载失败会恢复真实日志器的级别。
func TestReloadFileHookFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "semantic-server.yaml")
	rewriteConfig(t, path, "log:\n  level: info\n")
	initial, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	logger := log.New(log.Options{Level: log.LevelInfo, Writer: io.Discard})
	r := &Reloader{path: path, cfg: initial, logger: logger, hooks: Hooks{
		OnLogLevelChanged: func(v string) { logger.SetLevel(log.ParseLevel(v)) },
		OnProfilesDirChanged: func(dir string) error {
			_, err := os.Stat(dir)
			return err
		},
	}}
	missing := filepath.Join(t.TempDir(), "missing")
	rewriteConfig(t, path, "log:\n  level: debug\nagents:\n  profiles_dir: "+missing+"\n")
	r.reload()
	if logger.Level() != log.LevelInfo || !reflect.DeepEqual(r.Current(), initial) {
		t.Fatalf("failed file reload changed logger or snapshot: level=%s", logger.Level())
	}
}

// TestReloadHookFailureRestoresRuntime 验证拒绝更新后运行态与快照一致。
func TestReloadHookFailureRestoresRuntime(t *testing.T) {
	for _, name := range []string{"rejected hook", "missing hook"} {
		t.Run(name, func(t *testing.T) {
			initial := &Config{}
			initial.Log.Level = "info"
			initial.Agents.ProfilesDir = "old"
			level := initial.Log.Level
			hooks := Hooks{OnLogLevelChanged: func(v string) { level = v }}
			if name == "rejected hook" {
				hooks.OnProfilesDirChanged = func(string) error { return errors.New("invalid profile") }
			}
			r := &Reloader{cfg: initial, hooks: hooks, logger: log.New(log.Options{Writer: io.Discard})}
			next := *initial
			next.Log.Level = "debug"
			next.Agents.ProfilesDir = "new"
			if err := r.ApplyExternal(&next); err == nil {
				t.Fatal("expected reload failure")
			}
			if level != "info" || !reflect.DeepEqual(r.Current(), initial) {
				t.Fatalf("failed reload changed runtime or snapshot: level=%s snapshot=%+v", level, r.Current())
			}
			if err := r.ApplyExternal(initial); err != nil || level != "info" {
				t.Fatalf("restoring the original config left a stale level: %s, %v", level, err)
			}
		})
	}
}

// TestReloadRollbackOrder 验证 LLM 在依赖它的角色加载器之前恢复。
func TestReloadRollbackOrder(t *testing.T) {
	initial := &Config{}
	initial.LLM.Default = "old"
	initial.Log.Level = "info"
	initial.Agents.ProfilesDir = "old"
	initial.Skills.Dir = "old"
	actual := *initial
	rejection := errors.New("invalid MCP config")
	r := &Reloader{cfg: initial, logger: log.New(log.Options{Writer: io.Discard}), hooks: Hooks{
		OnLLMChanged:      func(v LLMConfig) error { actual.LLM = v; return nil },
		OnLogLevelChanged: func(v string) { actual.Log.Level = v },
		OnProfilesDirChanged: func(v string) error {
			if actual.LLM.Default != v {
				return errors.New("profile requires matching LLM")
			}
			actual.Agents.ProfilesDir = v
			return nil
		},
		OnSkillsDirChanged:  func(v string) error { actual.Skills.Dir = v; return nil },
		OnMCPServersChanged: func([]MCPServerConfig) error { return rejection },
		OnExecutionChanged:  func(v ExecutionConfig) { actual.Execution = v },
	}}
	next := *initial
	next.LLM.Default = "new"
	next.Log.Level = "debug"
	next.Agents.ProfilesDir = "new"
	next.Skills.Dir = "new"
	next.MCPServers = []MCPServerConfig{{Name: "new"}}
	next.Execution.AllowHost = true
	if err := r.ApplyExternal(&next); err == nil {
		t.Fatal("expected reload failure")
	}
	if !reflect.DeepEqual(actual, *initial) || !reflect.DeepEqual(r.Current(), initial) {
		t.Fatalf("rollback did not restore runtime and snapshot: actual=%+v snapshot=%+v", actual, r.Current())
	}
}

// TestReloadRollbackFailureTracksRuntime 验证回滚失败可见且下次仍可重试恢复。
func TestReloadRollbackFailureTracksRuntime(t *testing.T) {
	initial := &Config{}
	initial.LLM.Default = "old"
	initial.Log.Level = "info"
	initial.Agents.ProfilesDir = "old"
	actual := *initial
	rollbackErr := errors.New("old LLM unavailable")
	rejectRollback := true
	r := &Reloader{cfg: initial, logger: log.New(log.Options{Writer: io.Discard}), hooks: Hooks{
		OnLLMChanged: func(v LLMConfig) error {
			if rejectRollback && v.Default == "old" {
				return rollbackErr
			}
			actual.LLM = v
			return nil
		},
		OnLogLevelChanged:    func(v string) { actual.Log.Level = v },
		OnProfilesDirChanged: func(string) error { return errors.New("invalid profile") },
	}}
	next := *initial
	next.LLM.Default = "new"
	next.Log.Level = "debug"
	next.Agents.ProfilesDir = "new"
	if err := r.ApplyExternal(&next); !errors.Is(err, rollbackErr) {
		t.Fatalf("expected rollback error, got %v", err)
	}
	if actual.LLM.Default != "new" || actual.Log.Level != "info" || !reflect.DeepEqual(*r.Current(), actual) {
		t.Fatalf("rollback failure hidden from snapshot: actual=%+v snapshot=%+v", actual, r.Current())
	}
	rejectRollback = false
	if err := r.ApplyExternal(initial); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, *initial) || !reflect.DeepEqual(r.Current(), initial) {
		t.Fatal("retry did not restore the original runtime and snapshot")
	}
}

// TestReloadMCPRollbackAndRetry 验证最后一段缺少 hook 时恢复 MCP，修正后可重试。
func TestReloadMCPRollbackAndRetry(t *testing.T) {
	initial := &Config{MCPServers: []MCPServerConfig{{Name: "old"}}}
	actual := *initial
	r := &Reloader{cfg: initial, logger: log.New(log.Options{Writer: io.Discard}), hooks: Hooks{
		OnMCPServersChanged: func(v []MCPServerConfig) error { actual.MCPServers = v; return nil },
	}}
	next := *initial
	next.MCPServers = []MCPServerConfig{{Name: "new"}}
	next.Execution.AllowHost = true
	if err := r.ApplyExternal(&next); err == nil {
		t.Fatal("expected missing execution hook error")
	}
	if !reflect.DeepEqual(actual, *initial) || !reflect.DeepEqual(r.Current(), initial) {
		t.Fatal("failed update did not restore MCP servers")
	}
	r.hooks.OnExecutionChanged = func(v ExecutionConfig) { actual.Execution = v }
	if err := r.ApplyExternal(&next); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, next) || !reflect.DeepEqual(*r.Current(), next) {
		t.Fatal("retry did not apply the complete configuration")
	}
}

// TestReloadSerializesUpdates 验证后续更新必须以先前操作完成后的快照为基线。
func TestReloadSerializesUpdates(t *testing.T) {
	initial := &Config{}
	initial.Log.Level = "info"
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	level := "info"
	r := &Reloader{cfg: initial, logger: log.New(log.Options{Writer: io.Discard}), hooks: Hooks{
		OnLogLevelChanged: func(v string) {
			if v == "debug" {
				close(entered)
				<-release
			}
			level = v
		},
	}}
	next := *initial
	next.Log.Level = "debug"
	first := make(chan error, 1)
	go func() { first <- r.ApplyExternal(&next) }()
	<-entered
	second := make(chan error, 1)
	go func() { second <- r.ApplyExternal(initial) }()
	select {
	case err := <-second:
		t.Fatalf("second update finished before the first: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	for _, done := range []chan error{first, second} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("reload did not complete")
		}
	}
	if level != "info" || r.Current().Log.Level != level {
		t.Fatalf("concurrent updates left stale runtime or snapshot: level=%s snapshot=%s", level, r.Current().Log.Level)
	}
}
