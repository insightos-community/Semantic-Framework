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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// MemberDef 是 Team 成员的声明：ID 为实例标识（roster/事件归因用），
// Role 指向角色 profile（configs/agents/<role>/）。
type MemberDef struct {
	// ID 成员实例标识；leader 省略时取 role 名。
	ID string `yaml:"id"`

	// Role 成员角色（角色 profile 目录名）。
	Role string `yaml:"role"`

	// Device 可选：worker 角色的绑定设备（设备绑定落地后消费，本轮只解析）。
	Device string `yaml:"device"`
}

// Def 是一个 Team 的定义（架构文档 04 §4）：
// 1 个 leader（协调者）+ N 个请求式成员。
type Def struct {
	// Name Team 名（server 路由到 Team 的标识）。
	Name string `yaml:"name"`

	// Leader 协调者成员（用户-facing 入口）。
	Leader MemberDef `yaml:"leader"`

	// Members 其余成员（工作者/观察者/服务者）。
	Members []MemberDef `yaml:"members"`
}

// All 返回 leader 在内的全部成员（leader 在前），供装配顺序遍历。
func (d *Def) All() []MemberDef {
	all := make([]MemberDef, 0, len(d.Members)+1)
	all = append(all, d.Leader)
	return append(all, d.Members...)
}

// teamYAML 是 Team 定义文件的原始反序列化结构。
type teamYAML struct {
	Name    string      `yaml:"name"`
	Leader  MemberDef   `yaml:"leader"`
	Members []MemberDef `yaml:"members"`
}

// LoadTeams 加载目录下全部 Team 定义（*.yaml），按 Team 名索引。
// 目录不存在视为"未配置 Team"（返回空表，单 leader 模式的合法形态）；
// 目录存在但文件非法（解析失败/校验不过）直接报错——声明了 Team 却
// 装配错形态不应静默降级。
func LoadTeams(dir string) (map[string]*Def, error) {
	entries, err := os.ReadDir(dir)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		return map[string]*Def{}, nil
	default:
		return nil, fmt.Errorf("读取 Team 定义目录 %s 失败: %w", dir, err)
	}

	teams := make(map[string]*Def, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		def, err := loadTeamFile(path)
		if err != nil {
			return nil, err
		}
		if _, dup := teams[def.Name]; dup {
			return nil, fmt.Errorf("Team 名 %q 重复定义（%s）", def.Name, path)
		}
		teams[def.Name] = def
	}
	return teams, nil
}

// loadTeamFile 解析并校验单个 Team 定义文件。
func loadTeamFile(path string) (*Def, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 Team 定义 %s 失败: %w", path, err)
	}
	var raw teamYAML
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("解析 Team 定义 %s 失败: %w", path, err)
	}

	def := &Def{
		Name:    raw.Name,
		Leader:  raw.Leader,
		Members: raw.Members,
	}
	if err := def.validate(path); err != nil {
		return nil, err
	}
	return def, nil
}

// validate 校验 Team 定义并回填默认值：name/leader.role 必填，
// 成员 id/role 必填且 id 全局唯一（含 leader）。
func (d *Def) validate(path string) error {
	if d.Name == "" {
		return fmt.Errorf("Team 定义 %s 缺少 name", path)
	}
	if d.Leader.Role == "" {
		return fmt.Errorf("Team %q 缺少 leader.role", d.Name)
	}
	if d.Leader.ID == "" {
		d.Leader.ID = d.Leader.Role
	}

	seen := map[string]struct{}{d.Leader.ID: {}}
	for i, m := range d.Members {
		if m.ID == "" {
			return fmt.Errorf("Team %q 第 %d 个成员缺少 id", d.Name, i+1)
		}
		if m.Role == "" {
			return fmt.Errorf("Team %q 成员 %q 缺少 role", d.Name, m.ID)
		}
		if _, dup := seen[m.ID]; dup {
			return fmt.Errorf("Team %q 成员 id %q 重复", d.Name, m.ID)
		}
		seen[m.ID] = struct{}{}
	}
	return nil
}

// Names 返回全部 Team 名（升序），日志与调试用。
func Names(teams map[string]*Def) []string {
	names := make([]string, 0, len(teams))
	for name := range teams {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
