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

package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShippedPlanningAgentsPinReadOnlyMapQuery(t *testing.T) {
	loader := NewLoader(filepath.Join("..", "..", "..", "configs", "agents"))
	for _, role := range []string{"leader", "robot"} {
		p, err := loader.Load(role)
		if err != nil {
			t.Fatal(err)
		}
		for _, list := range [][]string{p.Tools.Namespaces, p.Tools.Pinned} {
			found := false
			for _, name := range list {
				if name == "map.query" {
					found = true
				}
				if name == "map.*" {
					t.Fatalf("%s must not gain map writes", role)
				}
			}
			if !found {
				t.Fatalf("%s lacks permanently visible read-only map.query: %v", role, list)
			}
		}
	}
}

// writeRoleFixture 在临时目录创建角色 profile 文件，返回角色根目录。
func writeRoleFixture(t *testing.T, role string, roleYAML, agentMD, safetyMD string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, role)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("创建角色目录失败: %v", err)
	}
	if roleYAML != "" {
		if err := os.WriteFile(filepath.Join(dir, "role.yaml"), []byte(roleYAML), 0o644); err != nil {
			t.Fatalf("写入 role.yaml 失败: %v", err)
		}
	}
	if agentMD != "" {
		if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte(agentMD), 0o644); err != nil {
			t.Fatalf("写入 AGENT.md 失败: %v", err)
		}
	}
	if safetyMD != "" {
		if err := os.WriteFile(filepath.Join(dir, "SAFETY.md"), []byte(safetyMD), 0o644); err != nil {
			t.Fatalf("写入 SAFETY.md 失败: %v", err)
		}
	}
	return root
}

// TestLoadFull 验证完整 role.yaml 的全部字段被正确加载。
func TestLoadFull(t *testing.T) {
	root := writeRoleFixture(t, "leader", `
name: leader
mode: coordinator
description: 团队指挥官
instruction_file: AGENT.md
model: deepseek-chat
reasoning_effort: medium
reasoning_visibility: show
tools:
  namespaces: [system.*, artifact.*]
  pinned: [system.time]
  tool_search: true
limits:
  max_turns: 30
  context_tokens: 90000
interrupt:
  approval_required: [artifact.*]
memory:
  long_term: true
skills:
  allowlist: [data-profile, semantic-diagnostics, data-profile, ""]
`, "# Role\n你是 Leader。", "# 安全总则\n危险操作必须审批。")

	p, err := NewLoader(root).Load("leader")
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if p.Name != "leader" || p.Mode != ModeCoordinator || p.Description != "团队指挥官" {
		t.Errorf("基础字段不符: %+v", p)
	}
	if p.Model != "deepseek-chat" {
		t.Errorf("模型字段不符: %+v", p)
	}
	if p.ReasoningEffort != "medium" || p.ReasoningVisibility != "show" {
		t.Errorf("思考配置不符: effort=%q visibility=%q", p.ReasoningEffort, p.ReasoningVisibility)
	}
	if len(p.Tools.Namespaces) != 2 || !p.Tools.ToolSearch {
		t.Errorf("工具字段不符: %+v", p.Tools)
	}
	if len(p.Tools.Pinned) != 1 || p.Tools.Pinned[0] != "system.time" {
		t.Errorf("常驻工具字段不符: %+v", p.Tools.Pinned)
	}
	if p.Limits.MaxTurns != 30 || p.Limits.ContextTokens != 90000 {
		t.Errorf("限额字段不符: %+v", p.Limits)
	}
	if len(p.Interrupt.ApprovalRequired) != 1 || p.Interrupt.ApprovalRequired[0] != "artifact.*" {
		t.Errorf("审批字段不符: %+v", p.Interrupt)
	}
	if !p.Memory.LongTerm {
		t.Error("memory.long_term 应为 true")
	}
	if len(p.Skills.Allowlist) != 2 || p.Skills.Allowlist[0] != "data-profile" ||
		p.Skills.Allowlist[1] != "semantic-diagnostics" {
		t.Errorf("Skill 白名单应去空去重并保留配置顺序: %+v", p.Skills.Allowlist)
	}
	if p.Instruction != "# Role\n你是 Leader。" {
		t.Errorf("Instruction 不符: %q", p.Instruction)
	}
	if p.SafetyDoc != "# 安全总则\n危险操作必须审批。" {
		t.Errorf("SafetyDoc 不符: %q", p.SafetyDoc)
	}
	if p.SummarizeThreshold() != 54000 {
		t.Errorf("摘要阈值应为预算 60%%（54000），实际: %d", p.SummarizeThreshold())
	}
}

// TestLoadDefaults 验证缺失字段回填默认值；model 保持为空，由运行时继承
// 系统 Default，而不是在 Profile 中复制一份端点配置。
func TestLoadDefaults(t *testing.T) {
	root := writeRoleFixture(t, "leader", "name: leader\nmode: coordinator\n", "# Role\n你是 Leader。", "")

	p, err := NewLoader(root).Load("leader")
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if p.Mode != ModeCoordinator {
		t.Errorf("mode 加载不符，实际: %q", p.Mode)
	}
	if p.Model != "" {
		t.Errorf("未指定 model 时应保留为空并继承系统 Default，实际: %q", p.Model)
	}
	if p.InstructionFile != "AGENT.md" {
		t.Errorf("instruction_file 默认应为 AGENT.md，实际: %q", p.InstructionFile)
	}
	if p.Limits.MaxTurns != 10 || p.Limits.ContextTokens != 120000 {
		t.Errorf("limits 默认值不符: %+v", p.Limits)
	}
	if p.SafetyDoc != "" {
		t.Errorf("无 SAFETY.md 时 SafetyDoc 应为空，实际: %q", p.SafetyDoc)
	}
	if p.ReasoningVisibility != "auto" {
		t.Errorf("reasoning_visibility 默认应为 auto，实际: %q", p.ReasoningVisibility)
	}
	if p.ReasoningEffort != "auto" {
		t.Errorf("reasoning_effort 默认应为 auto，实际: %q", p.ReasoningEffort)
	}
	if p.SummarizeThreshold() != 72000 {
		t.Errorf("默认摘要阈值应为 72000，实际: %d", p.SummarizeThreshold())
	}
}

func TestUpdateModels(t *testing.T) {
	root := writeRoleFixture(t, "leader", "name: leader\nmode: coordinator\nmodel: mock\n", "# Role", "")
	loader := NewLoader(root)
	if _, err := loader.Load("leader"); err != nil {
		t.Fatalf("初次 Load 失败: %v", err)
	}
	if err := loader.UpdateModels("leader", "deepseek-v4-flash", "auto", "hide"); err != nil {
		t.Fatalf("UpdateModels 失败: %v", err)
	}
	p, err := loader.Load("leader")
	if err != nil {
		t.Fatalf("更新后 Load 失败: %v", err)
	}
	if p.Model != "deepseek-v4-flash" || p.ReasoningEffort != "auto" ||
		p.ReasoningVisibility != "hide" {
		t.Fatalf("更新后的模型策略不符: %+v", p)
	}
	if err := loader.UpdateModels("leader", "", "auto", "auto"); err != nil {
		t.Fatalf("清除固定模型失败: %v", err)
	}
	p, err = loader.Load("leader")
	if err != nil || p.Model != "" {
		t.Fatalf("清除后应恢复系统 Default 继承: profile=%+v err=%v", p, err)
	}
}

// TestLoadMissingFiles 验证缺文件报错：role.yaml 缺失 / AGENT.md 缺失 / 角色不存在。
func TestLoadMissingFiles(t *testing.T) {
	// 角色目录不存在。
	if _, err := NewLoader(t.TempDir()).Load("ghost"); err == nil {
		t.Error("角色目录不存在应报错")
	}
	// role.yaml 缺失。
	root := writeRoleFixture(t, "leader", "", "# Role\n你是 Leader。", "")
	if _, err := NewLoader(root).Load("leader"); err == nil {
		t.Error("role.yaml 缺失应报错")
	}
	// AGENT.md 缺失。
	root = writeRoleFixture(t, "leader", "name: leader\nmode: coordinator\nmodel: deepseek-chat\n", "", "")
	if _, err := NewLoader(root).Load("leader"); err == nil {
		t.Error("AGENT.md 缺失应报错")
	}
}

// TestLoadInvalid 验证非法配置的校验：缺 name、name 与目录不一致、缺 mode
// 和 mode 非法。model 允许省略，表示继承系统 Default。
func TestLoadInvalid(t *testing.T) {
	cases := map[string]string{
		"缺 name":   "model: deepseek-chat\nmode: coordinator\n",
		"name 不一致": "name: robot\nmode: coordinator\nmodel: deepseek-chat\n",
		"缺 mode":   "name: leader\nmodel: deepseek-chat\n",
		"mode 非法":  "name: leader\nmodel: deepseek-chat\nmode: king\n",
	}
	for name, yamlBody := range cases {
		root := writeRoleFixture(t, "leader", yamlBody, "# Role\n你是 Leader。", "")
		if _, err := NewLoader(root).Load("leader"); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
}

// TestLoadObserverShowsMigrationPath 验证直接加载旧安装副本时，错误能指出
// 具体文件和正确迁移方式，而不是只报告一个无法定位的非法枚举值。
func TestLoadObserverShowsMigrationPath(t *testing.T) {
	root := writeRoleFixture(t, "monitor", "name: monitor\nmode: observer\n", "# Role", "")
	_, err := NewLoader(root).Load("monitor")
	if err == nil {
		t.Fatal("observer 已从 v0.2 移除，应返回迁移错误")
	}
	for _, want := range []string{"observer", filepath.Join(root, "monitor", "role.yaml"), "Team members"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("迁移错误缺少 %q: %v", want, err)
		}
	}
}

// TestLoadSubAgent 验证 subagent 段的加载与校验：service 模式启用成功
// （字段透传）；非 service 启用、缺 tool_name/tool_description、tool_name
// 含非法字符均报错（委派契约不完整不应带病上线）。
func TestLoadSubAgent(t *testing.T) {
	root := writeRoleFixture(t, "query", `
name: query
mode: service
description: 查询助手
model: deepseek-chat
limits:
  max_turns: 6
subagent:
  enabled: true
  tool_name: ask_query
  tool_description: 系统状态与产物查询助手
  max_turns: 4
`, "# Role\n你是查询助手。", "")

	p, err := NewLoader(root).Load("query")
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if !p.SubAgent.Enabled || p.SubAgent.ToolName != "ask_query" ||
		p.SubAgent.ToolDescription != "系统状态与产物查询助手" || p.SubAgent.MaxTurns != 4 {
		t.Errorf("subagent 段加载不符: %+v", p.SubAgent)
	}

	invalid := map[string]string{
		"非 service 启用":       "name: leader\nmode: coordinator\nmodel: mock\nsubagent: {enabled: true, tool_name: ask_x, tool_description: d}\n",
		"缺 tool_name":        "name: query\nmode: service\nmodel: mock\nsubagent: {enabled: true, tool_description: d}\n",
		"缺 tool_description": "name: query\nmode: service\nmodel: mock\nsubagent: {enabled: true, tool_name: ask_x}\n",
		"tool_name 非法字符":     "name: query\nmode: service\nmodel: mock\nsubagent: {enabled: true, tool_name: ask.query, tool_description: d}\n",
	}
	for name, yamlBody := range invalid {
		role := "query"
		if name == "非 service 启用" {
			role = "leader"
		}
		root := writeRoleFixture(t, role, yamlBody, "# Role\nx。", "")
		if _, err := NewLoader(root).Load(role); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
}

// TestLoadCache 验证加载结果按角色名缓存（两次加载返回同一实例）。
func TestLoadCache(t *testing.T) {
	root := writeRoleFixture(t, "leader", "name: leader\nmode: coordinator\nmodel: deepseek-chat\n", "# Role\n你是 Leader。", "")
	loader := NewLoader(root)
	p1, err := loader.Load("leader")
	if err != nil {
		t.Fatalf("首次 Load 失败: %v", err)
	}
	p2, err := loader.Load("leader")
	if err != nil {
		t.Fatalf("二次 Load 失败: %v", err)
	}
	if p1 != p2 {
		t.Error("二次加载应命中缓存返回同一实例")
	}
}
