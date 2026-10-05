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
	"io"
	"os"
	"path/filepath"
	"testing"

	"insightos.cn/semantic-framework/pkg/log"
)

// writeViewSkill 创建过滤视图测试所需的最小标准 Skill。
func writeViewSkill(t *testing.T, root, name string) {
	t.Helper()
	dir := filepath.Join(root, "general", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: 测试技能\ncategory: general\n---\n# 正文\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestViewIntersection 验证 Agent allowlist 是硬边界，Project 非空绑定只能
// 继续收窄；Project 空绑定保持“不增加限制”的既有迁移语义。
func TestViewIntersection(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"alpha", "beta", "gamma"} {
		writeViewSkill(t, root, name)
	}
	store, err := NewStore(root, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	if err != nil {
		t.Fatal(err)
	}

	unrestrictedProject := NewView(store, []string{"alpha", "beta", "missing"}, nil)
	if got := unrestrictedProject.List(); len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "beta" {
		t.Fatalf("Project 空绑定应保留 Agent 白名单内现有 Skill，实际: %+v", got)
	}
	restrictedProject := NewView(store, []string{"alpha", "beta"}, []string{"beta", "gamma"})
	if got := restrictedProject.List(); len(got) != 1 || got[0].Name != "beta" {
		t.Fatalf("有效集应为 Agent 与 Project 交集，实际: %+v", got)
	}
	if _, ok := restrictedProject.Get("alpha"); ok {
		t.Fatal("Project 未绑定的 Agent Skill 不应可读取")
	}
	if _, ok := NewView(store, nil, []string{"alpha"}).Get("alpha"); ok {
		t.Fatal("Agent 空白名单不应从 Project 获得额外 Skill")
	}
}

// TestViewTracksStoreReload 验证过滤视图不复制旧快照，Skill 文件热更新后
// 仍由同一视图读取当前 Store 内容。
func TestViewTracksStoreReload(t *testing.T) {
	root := t.TempDir()
	writeViewSkill(t, root, "alpha")
	store, err := NewStore(root, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	if err != nil {
		t.Fatal(err)
	}
	view := NewView(store, []string{"alpha", "beta"}, nil)
	writeViewSkill(t, root, "beta")
	if err := store.Reload(root); err != nil {
		t.Fatal(err)
	}
	if got := view.List(); len(got) != 2 {
		t.Fatalf("过滤视图应读取热更新后的源快照，实际: %+v", got)
	}
}
