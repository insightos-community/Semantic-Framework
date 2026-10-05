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

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"insightos.cn/semantic-framework/pkg/config"
)

// TestInstallRuntime 验证首次安装会生成可加载的独立配置和资源，并验证
// 重复执行 init 具有幂等性、不会覆盖用户已修改的主配置。
func TestInstallRuntime(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "configs", "semantic-server.yaml")
	created, skipped, err := installRuntime(target, false)
	if err != nil {
		t.Fatalf("首次安装失败: %v", err)
	}
	if !created || skipped != 0 {
		t.Fatalf("首次安装状态不符: created=%v skipped=%d", created, skipped)
	}
	cfg, err := config.Load(target)
	if err != nil {
		t.Fatalf("安装配置不可加载: %v", err)
	}
	if cfg.Store.SQLitePath != filepath.Join(root, "data", "semantic.db") {
		t.Errorf("sqlite_path 未指向安装目录: %q", cfg.Store.SQLitePath)
	}
	if cfg.Agents.ProfilesDir != filepath.Join(root, "configs", "agents") ||
		cfg.Skills.Dir != filepath.Join(root, "configs", "skills") {
		t.Errorf("Agent/Skill 路径未指向安装副本: agents=%q skills=%q",
			cfg.Agents.ProfilesDir, cfg.Skills.Dir)
	}
	if cfg.LLM.Default != "mock" || len(cfg.LLM.Providers) != 1 ||
		cfg.LLM.Providers["mock"].Component != "mock" {
		t.Errorf("新安装必须只预置 mock 模型，实际: default=%q providers=%+v",
			cfg.LLM.Default, cfg.LLM.Providers)
	}
	for _, path := range []string{
		filepath.Join(root, "configs", "agents", "leader", "role.yaml"),
		filepath.Join(root, "configs", "agents", "robot", "role.yaml"),
		filepath.Join(root, "configs", "skills", "general", "echo-guide", "SKILL.md"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("安装资源缺失 %s: %v", path, err)
		}
	}
	// 三个内置 Agent 不固定端点，统一继承安装配置中的系统 Default。安装
	// 模板的 Default 是 mock，用户后续修改 Default 后新会话才能自然生效。
	for _, role := range []string{"leader", "query", "robot"} {
		path := filepath.Join(root, "configs", "agents", role, "role.yaml")
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取 %s Agent 配置失败: %v", role, err)
		}
		if strings.Contains(string(content), "model: mock") ||
			strings.Contains(string(content), "deepseek") {
			t.Errorf("%s Agent 安装配置不应固定模型端点，实际:\n%s", role, content)
		}
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	marker := append(data, []byte("\n# user-marker\n")...)
	if err := os.WriteFile(target, marker, 0o600); err != nil {
		t.Fatal(err)
	}
	created, skipped, err = installRuntime(target, false)
	if err != nil || created || skipped == 0 {
		t.Fatalf("重复 init 应保留现有文件: created=%v skipped=%d err=%v", created, skipped, err)
	}
	after, _ := os.ReadFile(target)
	if !strings.Contains(string(after), "user-marker") {
		t.Fatal("重复 init 不应覆盖现有配置")
	}
}

// TestInstallRuntimeMigratesMissingSimulationPaths 验证旧配置会补上安装目录，
// 同时保留未知字段、用户注释和已经显式设置的仿真路径。
func TestInstallRuntimeMigratesMissingSimulationPaths(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "configs", "semantic-server.yaml")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `# keep-comment
server:
  http_addr: :8080
  read_timeout: 17s
`
	if err := os.WriteFile(target, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	created, _, err := installRuntime(target, false)
	if err != nil || !created {
		t.Fatalf("旧配置迁移失败: created=%v err=%v", created, err)
	}
	cfg, err := config.Load(target)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Simulation.RuntimesDir != filepath.Join(root, "runtimes.d") ||
		cfg.Simulation.CatalogDir != filepath.Join(root, "content", "scene-catalogs") {
		t.Fatalf("仿真目录未迁移到安装根: %+v", cfg.Simulation)
	}
	body, _ := os.ReadFile(target)
	if !strings.Contains(string(body), "keep-comment") ||
		!strings.Contains(string(body), "read_timeout: 17s") {
		t.Fatalf("迁移覆盖了用户配置:\n%s", body)
	}

	custom := `simulation:
  runtimes_dir: /custom/runtimes
  catalog_dir: /custom/scenes
`
	if err := os.WriteFile(target, []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}
	created, _, err = installRuntime(target, false)
	if err != nil || created {
		t.Fatalf("已有仿真路径不应重写: created=%v err=%v", created, err)
	}
	after, _ := os.ReadFile(target)
	if string(after) != custom {
		t.Fatalf("用户仿真路径被覆盖:\n%s", after)
	}
}

// TestResetRuntimeData 验证 reset-data 整体备份数据库、WAL、Artifact，且
// 配置与 Agent/Skill 文件不受影响。
func TestResetRuntimeData(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "configs", "semantic-server.yaml")
	if _, _, err := installRuntime(target, false); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(root, "data")
	for _, name := range []string{"semantic.db", "semantic.db-wal", filepath.Join("artifacts", "art-1")} {
		path := filepath.Join(dataDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	when := time.Date(2026, 8, 5, 1, 2, 3, 4, time.UTC)
	backup, err := resetRuntimeData(target, when)
	if err != nil {
		t.Fatalf("resetRuntimeData 失败: %v", err)
	}
	if backup == "" || !strings.Contains(backup, "data-20260805T010203.000000004Z") {
		t.Fatalf("备份目录名称不稳定: %q", backup)
	}
	for _, name := range []string{"semantic.db", "semantic.db-wal", filepath.Join("artifacts", "art-1")} {
		if _, err := os.Stat(filepath.Join(backup, name)); err != nil {
			t.Errorf("备份缺少 %s: %v", name, err)
		}
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("重建后的 data 应为空: entries=%v err=%v", entries, err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("reset-data 不应删除安装配置: %v", err)
	}
}

// TestInstallRuntimeForceOnlyReplacesConfig 验证 --force 只重建主配置，
// 已有 Agent profile 仍属于用户数据，必须完整保留。
func TestInstallRuntimeForceOnlyReplacesConfig(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "configs", "semantic-server.yaml")
	if _, _, err := installRuntime(target, false); err != nil {
		t.Fatal(err)
	}
	rolePath := filepath.Join(root, "configs", "agents", "leader", "role.yaml")
	if err := os.WriteFile(rolePath, []byte("custom-agent-profile"), 0o600); err != nil {
		t.Fatal(err)
	}
	if created, _, err := installRuntime(target, true); err != nil || !created {
		t.Fatalf("force 安装失败: created=%v err=%v", created, err)
	}
	role, _ := os.ReadFile(rolePath)
	if string(role) != "custom-agent-profile" {
		t.Fatal("--force 不应覆盖用户 Agent profile")
	}
}

// TestInstallRuntimeMigratesLegacyDefaultTeam 验证旧内置 Team 会自动升级，
// 但未被引用的旧 Monitor profile 作为安装资源保留。
func TestInstallRuntimeMigratesLegacyDefaultTeam(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "configs", "semantic-server.yaml")
	if _, _, err := installRuntime(target, false); err != nil {
		t.Fatal(err)
	}
	teamPath := filepath.Join(root, "configs", "agents", "teams", "default.yaml")
	want, err := os.ReadFile(teamPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(teamPath, []byte(legacyDefaultTeamV014), 0o600); err != nil {
		t.Fatal(err)
	}
	monitorDir := filepath.Join(root, "configs", "agents", "monitor")
	if err := os.MkdirAll(monitorDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rolePath := filepath.Join(monitorDir, "role.yaml")
	if err := os.WriteFile(rolePath, []byte("name: monitor\nmode: observer\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := installRuntime(target, false); err != nil {
		t.Fatalf("升级旧默认 Team 失败: %v", err)
	}
	got, err := os.ReadFile(teamPath)
	if err != nil {
		t.Fatal(err)
	}
	if !yamlEquivalent(got, want) {
		t.Fatalf("默认 Team 未升级为 v0.3 模板:\n%s", got)
	}
	if _, err := os.Stat(rolePath); err != nil {
		t.Fatalf("未被引用的旧 profile 不应被自动删除: %v", err)
	}
}

func TestInstallRuntimeMigratesExactV020WorkflowDefaults(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "configs", "semantic-server.yaml")
	if _, _, err := installRuntime(target, false); err != nil {
		t.Fatal(err)
	}
	teamPath := filepath.Join(root, "configs", "agents", "teams", "default.yaml")
	leaderPath := filepath.Join(root, "configs", "agents", "leader", "role.yaml")
	if err := os.WriteFile(teamPath, []byte(legacyDefaultTeamV020), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leaderPath, []byte(legacyLeaderRoleV020), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := installRuntime(target, false); err != nil {
		t.Fatalf("升级 v0.2 默认 Agent 配置失败: %v", err)
	}
	team, err := os.ReadFile(teamPath)
	if err != nil {
		t.Fatal(err)
	}
	leader, err := os.ReadFile(leaderPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(team), "id: developer-1") ||
		!strings.Contains(string(team), "role: developer") {
		t.Fatalf("v0.2 默认 Team 未补 Developer Worker:\n%s", team)
	}
	if !strings.Contains(string(leader), "plan.suggest") {
		t.Fatalf("v0.2 默认 Leader 未补 Plan 建议工具:\n%s", leader)
	}
}

func TestInstallRuntimeMigratesExactV030DefaultTeam(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "configs", "semantic-server.yaml")
	if _, _, err := installRuntime(target, false); err != nil {
		t.Fatal(err)
	}
	teamPath := filepath.Join(root, "configs", "agents", "teams", "default.yaml")
	if err := os.WriteFile(teamPath, []byte(legacyDefaultTeamV030), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := installRuntime(target, false); err != nil {
		t.Fatalf("升级 v0.3 默认 Team 失败: %v", err)
	}
	team, err := os.ReadFile(teamPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(team), "id: robot-1") ||
		!strings.Contains(string(team), "id: map-1") ||
		!strings.Contains(string(team), "id: monitor-1") {
		t.Fatalf("v0.3 默认 Team 应补持久 Worker 且不预置 Robot 实例:\n%s", team)
	}
}

func TestInstallRuntimePreservesCustomizedV030DefaultTeam(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "configs", "semantic-server.yaml")
	if _, _, err := installRuntime(target, false); err != nil {
		t.Fatal(err)
	}
	teamPath := filepath.Join(root, "configs", "agents", "teams", "default.yaml")
	custom := legacyDefaultTeamV030 + "custom_marker: keep-team\n"
	if err := os.WriteFile(teamPath, []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := installRuntime(target, false); err != nil {
		t.Fatalf("自定义 v0.3-like Team 应保持不变: %v", err)
	}
	team, err := os.ReadFile(teamPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(team) != custom {
		t.Fatalf("semantic init 不得覆盖自定义 v0.3 Team:\n%s", team)
	}
}

func TestInstallRuntimePreservesCustomizedV020LikeWorkflowDefaults(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "configs", "semantic-server.yaml")
	if _, _, err := installRuntime(target, false); err != nil {
		t.Fatal(err)
	}
	teamPath := filepath.Join(root, "configs", "agents", "teams", "default.yaml")
	leaderPath := filepath.Join(root, "configs", "agents", "leader", "role.yaml")
	customTeam := legacyDefaultTeamV020 + "custom_marker: keep-team\n"
	customLeader := legacyLeaderRoleV020 + "custom_marker: keep-leader\n"
	if err := os.WriteFile(teamPath, []byte(customTeam), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leaderPath, []byte(customLeader), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := installRuntime(target, false); err != nil {
		t.Fatalf("自定义 v0.2-like 配置应保留并由运行期给出能力缺口: %v", err)
	}
	team, _ := os.ReadFile(teamPath)
	leader, _ := os.ReadFile(leaderPath)
	if string(team) != customTeam || string(leader) != customLeader {
		t.Fatalf("semantic init 不得覆盖自定义 Agent 配置:\nteam=%s\nleader=%s", team, leader)
	}
}

// TestInstallRuntimeRejectsCustomObserverTeam 验证用户修改过的 Team 不会被
// 静默覆盖，错误同时指出 Team、Role 文件和正确处理方法。
func TestInstallRuntimeRejectsCustomObserverTeam(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "configs", "semantic-server.yaml")
	if _, _, err := installRuntime(target, false); err != nil {
		t.Fatal(err)
	}
	teamPath := filepath.Join(root, "configs", "agents", "teams", "default.yaml")
	custom := legacyDefaultTeamV014 + "custom_marker: true\n"
	if err := os.WriteFile(teamPath, []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}
	monitorDir := filepath.Join(root, "configs", "agents", "monitor")
	if err := os.MkdirAll(monitorDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rolePath := filepath.Join(monitorDir, "role.yaml")
	if err := os.WriteFile(rolePath, []byte("name: monitor\nmode: observer\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := installRuntime(target, false)
	if err == nil {
		t.Fatal("自定义 observer Team 应要求用户显式迁移")
	}
	message := err.Error()
	for _, want := range []string{teamPath, rolePath, "members", "不要把 mode 改成其他值"} {
		if !strings.Contains(message, want) {
			t.Fatalf("迁移错误缺少 %q: %v", want, err)
		}
	}
}
