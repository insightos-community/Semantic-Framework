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
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"insightos.cn/semantic-framework/pkg/log"
)

// sampleSkillsRoot 返回仓库模板 Skill 根目录。测试只用于验证随仓库交付的
// 脚本，安装路径转换由 semantic init 的既有测试覆盖。
func sampleSkillsRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "configs", "skills"))
	if err != nil {
		t.Fatalf("解析 Skill 根目录失败: %v", err)
	}
	return root
}

// TestSampleSkillsLoad 验证两个示例均符合当前标准 SKILL.md 解析契约，且未
// 携带自定义 runtime/permissions frontmatter。
func TestSampleSkillsLoad(t *testing.T) {
	store, err := NewStore(sampleSkillsRoot(t), log.New(log.Options{Writer: os.Stderr}))
	if err != nil {
		t.Fatalf("加载示例 Skill 失败: %v", err)
	}
	for _, name := range []string{"data-profile", "semantic-diagnostics"} {
		skill, ok := store.Get(name)
		if !ok || skill.Body == "" || skill.Dir == "" {
			t.Fatalf("示例 Skill %q 不完整: %+v", name, skill)
		}
	}
}

// TestDataProfileScript 验证 data-profile 使用 Python 标准库即可处理 CSV，
// 并生成 JSON 与 Markdown 两种实际报告。
func TestDataProfileScript(t *testing.T) {
	workspace := t.TempDir()
	input := filepath.Join(workspace, "sample.csv")
	if err := os.WriteFile(input, []byte("name,score\n甲,10\n乙,20\n丙,\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jsonOutput := filepath.Join(workspace, "profile.json")
	markdownOutput := filepath.Join(workspace, "profile.md")
	script := filepath.Join(sampleSkillsRoot(t), "data", "data-profile", "scripts", "profile.py")
	command := exec.Command("python3", script, "--input", input,
		"--json-output", jsonOutput, "--markdown-output", markdownOutput)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("data-profile 脚本失败: %v\n%s", err, output)
	}
	encoded, err := os.ReadFile(jsonOutput)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		RowCount int `json:"row_count"`
		Columns  map[string]struct {
			MissingCount int `json:"missing_count"`
		} `json:"columns"`
	}
	if err := json.Unmarshal(encoded, &result); err != nil || result.RowCount != 3 ||
		result.Columns["score"].MissingCount != 1 {
		t.Fatalf("画像结果不一致: result=%+v err=%v", result, err)
	}
	if markdown, err := os.ReadFile(markdownOutput); err != nil || len(markdown) == 0 {
		t.Fatalf("Markdown 报告未生成: err=%v", err)
	}
}

// TestSemanticDiagnosticsScript 验证诊断脚本在 Server 不可达时仍生成完整
// 报告，而不是把局部检查失败升级为整个 Skill 失败。
func TestSemanticDiagnosticsScript(t *testing.T) {
	workspace := t.TempDir()
	jsonOutput := filepath.Join(workspace, "diagnostics.json")
	markdownOutput := filepath.Join(workspace, "diagnostics.md")
	script := filepath.Join(sampleSkillsRoot(t), "system", "semantic-diagnostics", "scripts", "diagnose.py")
	command := exec.Command("python3", script, "--server-url", "http://127.0.0.1:1",
		"--json-output", jsonOutput, "--markdown-output", markdownOutput)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("semantic-diagnostics 脚本失败: %v\n%s", err, output)
	}
	encoded, err := os.ReadFile(jsonOutput)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil || result["platform"] == nil ||
		result["semantic_health"] == nil {
		t.Fatalf("诊断结果不完整: result=%+v err=%v", result, err)
	}
	if markdown, err := os.ReadFile(markdownOutput); err != nil || len(markdown) == 0 {
		t.Fatalf("诊断 Markdown 未生成: err=%v", err)
	}
}
