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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill 在 root 下写一个技能文件：relDir 为技能目录相对路径
// （如 general/echo-guide），content 为 SKILL.md 全文。
func writeSkill(t *testing.T, root, relDir, content string) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(relDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("创建技能目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, skillFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("写入 SKILL.md 失败: %v", err)
	}
}

// TestLoadDir 验证递归加载：分类目录嵌套的技能被发现，frontmatter 标准字段
// 解析进 Skill，具身扩展字段进 Extensions 透传，正文不含 frontmatter。
func TestLoadDir(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "general/echo-guide", `---
name: echo-guide
description: 使用 system.echo 与 system.time 的查询指引
category: general
when_to_use: 需要回显文本或获取当前时间时
---

# 查询指引

先调用 system.time 获取当前时间。
`)
	writeSkill(t, root, "robot/grasp-product", `---
name: grasp-product
description: 通过视觉定位精确抓取目标商品
category: robot_skill
goal:
  description: 目标商品被稳定夹持
  timeout: 5m
safety_rules:
  - id: sr-1
    rule: 夹爪闭合前确认目标区域无障碍
component: [arm, gripper]
steps:
  - name: 定位目标
on_failure:
  - when: 目标未找到
    do: 上报 L2
---

# 抓取商品

执行要点正文。
`)

	skills, errs := LoadDir(root)
	if len(errs) != 0 {
		t.Fatalf("不应有加载错误，实际: %v", errs)
	}
	if len(skills) != 2 {
		t.Fatalf("应加载 2 个技能，实际: %d（%+v）", len(skills), skills)
	}

	echo := skills[0]
	if echo.Name != "echo-guide" || echo.Category != "general" ||
		echo.WhenToUse != "需要回显文本或获取当前时间时" {
		t.Errorf("标准字段解析不符: %+v", echo)
	}
	if !strings.Contains(echo.Body, "# 查询指引") || strings.Contains(echo.Body, "name:") {
		t.Errorf("正文应为 frontmatter 之后的 markdown，实际: %q", echo.Body)
	}
	if echo.Dir != filepath.Join(root, "general", "echo-guide") {
		t.Errorf("Dir 应为技能目录，实际: %q", echo.Dir)
	}
	if echo.Extensions != nil {
		t.Errorf("无扩展字段时 Extensions 应为 nil，实际: %+v", echo.Extensions)
	}

	grasp := skills[1]
	if grasp.Name != "grasp-product" || grasp.Category != "robot_skill" {
		t.Errorf("标准字段解析不符: %+v", grasp)
	}
	// 具身扩展字段只透传不消费：键原样保留，值保持 YAML 解析后的结构。
	for _, key := range []string{"goal", "safety_rules", "component", "steps", "on_failure"} {
		if _, ok := grasp.Extensions[key]; !ok {
			t.Errorf("Extensions 应透传 %q，实际: %+v", key, grasp.Extensions)
		}
	}
	goal, ok := grasp.Extensions["goal"].(map[string]any)
	if !ok || goal["timeout"] != "5m" {
		t.Errorf("goal 扩展值应保持 YAML 结构，实际: %+v", grasp.Extensions["goal"])
	}
}

// TestLoadDirSoftErrors 验证软错误纪律：单个文件失败（缺 name、YAML 非法、
// 无 frontmatter）收集进 errs 继续，成功项照常返回。
func TestLoadDirSoftErrors(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "general/good", `---
name: good
description: 好技能
---

正文。
`)
	writeSkill(t, root, "general/no-name", `---
description: 缺 name
---

正文。
`)
	writeSkill(t, root, "general/bad-yaml", `---
name: [非法
description: x
---

正文。
`)
	writeSkill(t, root, "general/no-frontmatter", `# 只有标题

没有 frontmatter 的文档不是技能。
`)

	skills, errs := LoadDir(root)
	if len(skills) != 1 || skills[0].Name != "good" {
		t.Errorf("成功项应只有 good，实际: %+v", skills)
	}
	if len(errs) != 3 {
		t.Fatalf("应收集 3 个软错误，实际: %d（%v）", len(errs), errs)
	}
}

// TestLoadDirHardError 验证技能目录本身不可读是硬错误（errs 唯一元素）。
func TestLoadDirHardError(t *testing.T) {
	skills, errs := LoadDir(filepath.Join(t.TempDir(), "not-exist"))
	if skills != nil {
		t.Errorf("目录不可读时 skills 应为 nil，实际: %+v", skills)
	}
	if len(errs) != 1 {
		t.Fatalf("硬错误应只有 1 个，实际: %d（%v）", len(errs), errs)
	}
}

// TestParseValidation 验证必填校验与默认值回填：
// 缺 name/description 失败；category 空回填 DefaultCategory；
// description 的空白被压缩（清单单行渲染约定）。
func TestParseValidation(t *testing.T) {
	if _, err := Parse([]byte("---\ndescription: 只有描述\n---\n正文"), "x"); err == nil ||
		!strings.Contains(err.Error(), "name") {
		t.Errorf("缺 name 应失败，实际: %v", err)
	}
	if _, err := Parse([]byte("---\nname: x\n---\n正文"), "x"); err == nil ||
		!strings.Contains(err.Error(), "description") {
		t.Errorf("缺 description 应失败，实际: %v", err)
	}

	s, err := Parse([]byte("---\nname: x\ndescription: \"  多  空白\t描述  \"\n---\n正文"), "x")
	if err != nil {
		t.Fatalf("合法技能应解析成功: %v", err)
	}
	if s.Category != DefaultCategory {
		t.Errorf("category 空应回填 %q，实际: %q", DefaultCategory, s.Category)
	}
	if s.Description != "多 空白 描述" {
		t.Errorf("description 空白应被压缩，实际: %q", s.Description)
	}
}
