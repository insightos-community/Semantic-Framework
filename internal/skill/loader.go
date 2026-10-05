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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// skillFileName 是每个技能目录中固定的技能文件名（架构文档 06 §4.1）。
const skillFileName = "SKILL.md"

// DefaultCategory 是 frontmatter 未声明 category 时的默认分类：
// Summary 按分类分组渲染，空分类会让分组标题失去意义，统一回填。
const DefaultCategory = "general"

// standardFrontmatterKeys 是 SKILL.md frontmatter 的标准字段集
// （架构文档 06 §4.2 的最小集）：解析进 Skill 的命名字段；
// 标准字段以外的键全部进 Extensions 透传。
var standardFrontmatterKeys = map[string]struct{}{
	"name": {}, "description": {}, "category": {}, "when_to_use": {},
}

// frontmatter 是 SKILL.md YAML frontmatter 标准字段的反序列化结构。
type frontmatter struct {
	// Name 技能名（必填）。
	Name string `yaml:"name"`

	// Description 一句话描述（必填）。
	Description string `yaml:"description"`

	// Category 技能分类（可选，空回填 DefaultCategory）。
	Category string `yaml:"category"`

	// WhenToUse 适用时机描述（可选）。
	WhenToUse string `yaml:"when_to_use"`
}

// LoadDir 递归扫描 dir 加载全部技能：每技能独占一个目录、含 SKILL.md；
// 发现 SKILL.md 的目录不再下钻（其中的 scripts/references 是技能资源而非
// 技能）；隐藏目录（. 开头）跳过。返回顺序按文件系统遍历序（字典序）。
//
// 软错误纪律：单个文件失败（读取/解析/必填校验）收集进 errs 继续扫描，
// skills 只含成功项；dir 本身不可读是硬错误（errs 的唯一元素）。
func LoadDir(dir string) (skills []Skill, errs []error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, []error{fmt.Errorf("技能目录 %s 不可读: %w", dir, err)}
	}
	if !info.IsDir() {
		return nil, []error{fmt.Errorf("技能路径 %s 不是目录", dir)}
	}

	// WalkDir 的内部错误（非根目录）不中断遍历：单点失败按软错误收集。
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			errs = append(errs, fmt.Errorf("扫描 %s 失败: %w", path, walkErr))
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path != dir && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		manifest := filepath.Join(path, skillFileName)
		if _, err := os.Stat(manifest); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("检查技能文件 %s 失败: %w", manifest, err))
			}
			return nil // 无 SKILL.md 的目录：继续下钻（分类目录等）
		}
		s, err := LoadFile(manifest)
		if err != nil {
			errs = append(errs, err)
		} else {
			skills = append(skills, *s)
		}
		return filepath.SkipDir // 技能目录不下钻
	})
	return skills, errs
}

// LoadFile 加载单个 SKILL.md：读取 → 解析 → 必填校验。
func LoadFile(path string) (*Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取技能文件 %s 失败: %w", path, err)
	}
	s, err := Parse(data, filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("解析技能文件 %s 失败: %w", path, err)
	}
	return s, nil
}

// Parse 解析 SKILL.md 内容（dir 为技能目录，记入 Skill.Dir）：
// frontmatter 切分（--- 包围的 YAML 头）→ 标准字段解析 + 扩展字段透传 →
// 必填校验（name/description 缺失即失败）。
func Parse(data []byte, dir string) (*Skill, error) {
	fm, body := splitFrontMatter(string(data))

	var std frontmatter
	var all map[string]any
	if fm != "" {
		if err := yaml.Unmarshal([]byte(fm), &std); err != nil {
			return nil, fmt.Errorf("frontmatter 不是合法 YAML: %w", err)
		}
		if err := yaml.Unmarshal([]byte(fm), &all); err != nil {
			return nil, fmt.Errorf("frontmatter 不是合法 YAML: %w", err)
		}
	}
	if std.Name == "" {
		return nil, errors.New("缺少必填字段 name（无 frontmatter 的技能同样不满足）")
	}
	if std.Description == "" {
		return nil, errors.New("缺少必填字段 description")
	}
	if std.Category == "" {
		std.Category = DefaultCategory
	}
	// 描述约定是一句话：压缩空白保证清单单行渲染不被多行描述破坏。
	std.Description = strings.Join(strings.Fields(std.Description), " ")

	// 扩展字段透传：标准键以外的 frontmatter 键原样保留（无扩展时为 nil）。
	var extensions map[string]any
	for key, value := range all {
		if _, ok := standardFrontmatterKeys[key]; ok {
			continue
		}
		if extensions == nil {
			extensions = make(map[string]any)
		}
		extensions[key] = value
	}

	return &Skill{
		Name:        std.Name,
		Description: std.Description,
		Category:    std.Category,
		WhenToUse:   std.WhenToUse,
		Body:        body,
		Dir:         dir,
		Extensions:  extensions,
	}, nil
}

// splitFrontMatter 把 SKILL.md 按 --- 分隔线切成 frontmatter 与正文。
// 文件未以 --- 开头（或找不到收尾分隔线）时 frontmatter 为空、正文为全文：
// 随后的必填校验必然失败（无 name/description），即"无 frontmatter 不是技能"。
func splitFrontMatter(content string) (fm, body string) {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "---") {
		return "", content
	}
	rest := content[len("---"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", content
	}
	return strings.TrimSpace(rest[:end]), strings.TrimSpace(rest[end+len("\n---"):])
}
