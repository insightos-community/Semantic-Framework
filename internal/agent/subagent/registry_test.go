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

package subagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/agent/team"
	"insightos.cn/semantic-framework/internal/tool"
)

// writeProfiles 在临时根目录写入角色 profile，返回根目录。
// leader 未启用 subagent；query 启用（tool_name=ask_query）。
func writeProfiles(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	roles := map[string]map[string]string{
		"leader": {
			"role.yaml": "name: leader\nmode: coordinator\ndescription: 团队指挥官\nmodel: mock\n",
			"AGENT.md":  "# Role\n你是 Leader。",
		},
		"query": {
			"role.yaml": `name: query
mode: service
description: 系统与产物查询助手
model: mock
tools:
  namespaces: [system.*, artifact.get]
limits:
  max_turns: 6
subagent:
  enabled: true
  tool_name: ask_query
  tool_description: 系统状态与产物查询助手
`,
			"AGENT.md": "# Role\n你是查询助手。",
		},
	}
	for role, files := range roles {
		dir := filepath.Join(root, role)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("创建角色目录失败: %v", err)
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatalf("写入 %s/%s 失败: %v", role, name, err)
			}
		}
	}
	return root
}

// testTeam 是 leader + query-1 的 Team 定义。
func testTeam() *team.Def {
	return &team.Def{
		Name:    "default",
		Leader:  team.MemberDef{ID: "leader", Role: "leader"},
		Members: []team.MemberDef{{ID: "query-1", Role: "query"}},
	}
}

// TestRegistryDerivation 验证注册表派生：启用成员入册（字段从 profile 派生，
// max_turns 缺省沿用 limits），未启用成员（leader）不入册；List 按 ID 升序。
func TestRegistryDerivation(t *testing.T) {
	reg, err := NewRegistry(testTeam(), profile.NewLoader(writeProfiles(t)))
	if err != nil {
		t.Fatalf("NewRegistry 失败: %v", err)
	}

	list := reg.List()
	if len(list) != 1 {
		t.Fatalf("注册表应只有 query-1 一个启用成员，实际: %+v", list)
	}
	def := list[0]
	if def.ID != "query-1" || def.Role != "query" || def.Description != "系统与产物查询助手" {
		t.Errorf("定义身份字段不符: %+v", def)
	}
	if def.ToolName != "ask_query" || def.ToolDescription != "系统状态与产物查询助手" {
		t.Errorf("委派工具字段不符: %+v", def)
	}
	if def.MaxTurns != 6 {
		t.Errorf("max_turns 缺省应沿用 limits.max_turns=6，实际: %d", def.MaxTurns)
	}
	if len(def.Namespaces) != 2 || def.Namespaces[0] != "system.*" || def.Namespaces[1] != "artifact.get" {
		t.Errorf("namespaces 透传不符: %+v", def.Namespaces)
	}

	if _, ok := reg.Get("leader"); ok {
		t.Error("leader 未启用 subagent，不应入册")
	}
	got, ok := reg.Get("query-1")
	if !ok || got.ToolName != "ask_query" {
		t.Errorf("Get(query-1) 不符: %+v ok=%v", got, ok)
	}
}

// TestRegistryMaxTurnsOverride 验证 subagent.max_turns 显式覆盖 limits。
func TestRegistryMaxTurnsOverride(t *testing.T) {
	root := writeProfiles(t)
	path := filepath.Join(root, "query", "role.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 role.yaml 失败: %v", err)
	}
	overridden := strings.Replace(string(data), "tool_description: 系统状态与产物查询助手",
		"tool_description: 系统状态与产物查询助手\n  max_turns: 3", 1)
	if err := os.WriteFile(path, []byte(overridden), 0o644); err != nil {
		t.Fatalf("写回 role.yaml 失败: %v", err)
	}

	reg, err := NewRegistry(testTeam(), profile.NewLoader(root))
	if err != nil {
		t.Fatalf("NewRegistry 失败: %v", err)
	}
	def, ok := reg.Get("query-1")
	if !ok || def.MaxTurns != 3 {
		t.Errorf("subagent.max_turns 应覆盖为 3，实际: %+v ok=%v", def, ok)
	}
}

// TestRegistryNonServiceRejected 验证 worker 角色启用 subagent 在
// profile 加载期报错（注册表随之构建失败，不带病组建）。
func TestRegistryNonServiceRejected(t *testing.T) {
	root := writeProfiles(t)
	robotDir := filepath.Join(root, "robot")
	if err := os.MkdirAll(robotDir, 0o755); err != nil {
		t.Fatalf("创建 robot 目录失败: %v", err)
	}
	files := map[string]string{
		"role.yaml": "name: robot\nmode: worker\ndescription: 执行\nmodel: mock\n" +
			"subagent: {enabled: true, tool_name: ask_robot, tool_description: d}\n",
		"AGENT.md": "# Role\n你是 Robot。",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(robotDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("写入 robot/%s 失败: %v", name, err)
		}
	}

	def := testTeam()
	def.Members = append(def.Members, team.MemberDef{ID: "robot-1", Role: "robot"})
	if _, err := NewRegistry(def, profile.NewLoader(root)); err == nil {
		t.Error("worker 启用 subagent 应构建失败（仅 service 可启用）")
	}
}

// TestToolDefinition 验证安全门禁契约：命名空间按 tool_name 归属、risk=low、
// 参数 schema 与 {task, artifact_refs?} 契约一致、Timeout 为委派硬上限。
func TestToolDefinition(t *testing.T) {
	def := Def{ID: "query-1", ToolName: "ask_query", ToolDescription: "查询助手"}
	d := ToolDefinition(def)
	if d.Name != "ask_query" || d.Namespace != "ask_query" {
		t.Errorf("名称/命名空间应按 tool_name 归属: %+v", d)
	}
	if d.Annotations.Risk != tool.RiskLow {
		t.Errorf("委派工具应为 risk=low，实际: %s", d.Annotations.Risk)
	}
	if d.Annotations.Timeout != DelegationTimeout || DelegationTimeout != 2*time.Minute {
		t.Errorf("Timeout 应为委派硬上限 2m，实际: %s", d.Annotations.Timeout)
	}
	if !strings.Contains(d.ParametersJSON, `"task"`) || !strings.Contains(d.ParametersJSON, `"required"`) {
		t.Errorf("参数 schema 应声明 task 必填: %s", d.ParametersJSON)
	}
	if !strings.Contains(d.ParametersJSON, `"artifact_refs"`) ||
		!strings.Contains(d.ParametersJSON, `"items":{"type":"string"}`) {
		t.Errorf("参数 schema 应允许可选的 ArtifactRef 字符串数组: %s", d.ParametersJSON)
	}
}
