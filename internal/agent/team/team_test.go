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

package team

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTeamFile 在临时目录写入一个 Team 定义文件，返回目录路径。
func writeTeamFile(t *testing.T, dir, filename, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(body), 0o644); err != nil {
		t.Fatalf("写入 Team 定义失败: %v", err)
	}
}

// TestLoadTeams 验证完整定义的加载：字段映射、leader id 缺省回填 role 名，
// 以及 All 返回的成员顺序（leader 在前）。
func TestLoadTeams(t *testing.T) {
	dir := t.TempDir()
	writeTeamFile(t, dir, "default.yaml", `
name: default
leader:
  role: leader
members:
  - id: query-1
    role: query
`)
	writeTeamFile(t, dir, "site-a.yaml", `
name: site-a
leader: {id: commander, role: leader}
members:
  - {id: robot-a, role: robot, device: dev-01}
`)

	teams, err := LoadTeams(dir)
	if err != nil {
		t.Fatalf("LoadTeams 失败: %v", err)
	}
	if len(teams) != 2 {
		t.Fatalf("应加载 2 个 Team，实际: %d", len(teams))
	}

	def := teams["default"]
	if def == nil {
		t.Fatal("缺少 default Team")
	}
	if def.Leader.ID != "leader" || def.Leader.Role != "leader" {
		t.Errorf("leader id 应缺省回填 role 名: %+v", def.Leader)
	}
	all := def.All()
	if len(all) != 2 || all[0].ID != "leader" || all[1].ID != "query-1" {
		t.Errorf("All 顺序应为 leader 在前、成员按声明序: %+v", all)
	}

	siteA := teams["site-a"]
	if siteA == nil {
		t.Fatal("缺少 site-a Team")
	}
	if siteA.Leader.ID != "commander" {
		t.Errorf("site-a 字段不符: %+v", siteA)
	}
	if siteA.Members[0].Device != "dev-01" {
		t.Errorf("device 字段不符: %+v", siteA.Members[0])
	}
	if got := Names(teams); len(got) != 2 || got[0] != "default" || got[1] != "site-a" {
		t.Errorf("Names 应升序，实际: %v", got)
	}
}

// TestLoadTeamsMissingDir 验证目录不存在返回空表（单 leader 模式的合法形态）。
func TestLoadTeamsMissingDir(t *testing.T) {
	teams, err := LoadTeams(filepath.Join(t.TempDir(), "not-exist"))
	if err != nil {
		t.Fatalf("目录不存在不应报错: %v", err)
	}
	if len(teams) != 0 {
		t.Errorf("目录不存在应返回空表，实际: %v", teams)
	}
}

// TestLoadTeamsInvalid 验证非法定义逐项报错。
func TestLoadTeamsInvalid(t *testing.T) {
	cases := map[string]string{
		"缺 name":         "leader: {role: leader}\n",
		"缺 leader.role":  "name: default\nmembers: []\n",
		"成员缺 id":         "name: default\nleader: {role: leader}\nmembers:\n  - {role: query}\n",
		"成员缺 role":       "name: default\nleader: {role: leader}\nmembers:\n  - {id: query-1}\n",
		"成员 id 重复":       "name: default\nleader: {role: leader}\nmembers:\n  - {id: m-1, role: query}\n  - {id: m-1, role: query}\n",
		"与 leader id 重复": "name: default\nleader: {role: leader}\nmembers:\n  - {id: leader, role: query}\n",
	}
	for name, body := range cases {
		dir := t.TempDir()
		writeTeamFile(t, dir, "default.yaml", body)
		if _, err := LoadTeams(dir); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}

	// Team 名跨文件重复。
	dir := t.TempDir()
	writeTeamFile(t, dir, "a.yaml", "name: default\nleader: {role: leader}\n")
	writeTeamFile(t, dir, "b.yaml", "name: default\nleader: {role: leader}\n")
	if _, err := LoadTeams(dir); err == nil {
		t.Error("Team 名跨文件重复应报错")
	}
}
