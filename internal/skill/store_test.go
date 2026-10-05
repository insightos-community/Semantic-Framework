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
	"strings"
	"testing"
	"time"

	"insightos.cn/semantic-framework/pkg/log"
)

// testLogger 返回丢弃输出的日志器（单测便捷路径）。
func testLogger() *log.Logger {
	return log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
}

// TestStoreReloadAtomic 验证 Reload 的原子替换语义：
// 新快照 = 重载后的全部成功项（新增出现、删除消失、坏文件跳过），
// 替换一次性生效（读路径不会看到半更新的快照）。
func TestStoreReloadAtomic(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "general/a", "---\nname: a\ndescription: 技能 A\n---\n正文 A\n")
	writeSkill(t, root, "general/b", "---\nname: b\ndescription: 技能 B\n---\n正文 B\n")

	st, err := NewStore(root, testLogger())
	if err != nil {
		t.Fatalf("NewStore 失败: %v", err)
	}
	if got := len(st.List()); got != 2 {
		t.Fatalf("初始应有 2 个技能，实际: %d", got)
	}

	// 变更：新增 c、删除 b、把 a 改成坏文件（缺 description）。
	writeSkill(t, root, "general/c", "---\nname: c\ndescription: 技能 C\n---\n正文 C\n")
	writeSkill(t, root, "general/a", "---\nname: a\n---\n坏文件\n")
	if err := os.RemoveAll(filepath.Join(root, "general", "b")); err != nil {
		t.Fatalf("删除技能目录失败: %v", err)
	}

	if err := st.Reload(root); err != nil {
		t.Fatalf("Reload 失败: %v", err)
	}
	if _, ok := st.Get("a"); ok {
		t.Errorf("坏文件不应进快照（a 缺 description）")
	}
	if _, ok := st.Get("b"); ok {
		t.Errorf("已删除的 b 不应留在快照")
	}
	c, ok := st.Get("c")
	if !ok || c.Body != "正文 C" {
		t.Errorf("新增的 c 应进快照，实际: %+v (ok=%v)", c, ok)
	}
	if got := st.List(); len(got) != 1 {
		t.Errorf("重载后应只有 1 个技能，实际: %+v", got)
	}
}

// TestStoreReloadKeepsSnapshotOnHardError 验证目录不可读时保留旧快照：
// 一次坏变更（如目录被误删/挂载丢失）不打掉在用的技能集。
func TestStoreReloadKeepsSnapshotOnHardError(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "general/a", "---\nname: a\ndescription: 技能 A\n---\n正文 A\n")

	st, err := NewStore(root, testLogger())
	if err != nil {
		t.Fatalf("NewStore 失败: %v", err)
	}
	if err := st.Reload(filepath.Join(t.TempDir(), "not-exist")); err == nil {
		t.Fatalf("目录不可读应返回错误")
	}
	if _, ok := st.Get("a"); !ok {
		t.Errorf("硬错误后旧快照应保留（a 仍在）")
	}
}

// TestStoreSummary 验证清单摘要渲染：按 category 分组、组间与组内按名
// 升序、每技能一行 "- name: description (category)"；空快照返回空串。
func TestStoreSummary(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "general/zeta", "---\nname: zeta\ndescription: 最后\n---\n正文\n")
	writeSkill(t, root, "robot/grasp", "---\nname: grasp\ndescription: 抓取\ncategory: robot_skill\n---\n正文\n")
	writeSkill(t, root, "general/alpha", "---\nname: alpha\ndescription: 最前\n---\n正文\n")

	st, err := NewStore(root, testLogger())
	if err != nil {
		t.Fatalf("NewStore 失败: %v", err)
	}
	want := "### general\n" +
		"- alpha: 最前 (general)\n" +
		"- zeta: 最后 (general)\n" +
		"\n" +
		"### robot_skill\n" +
		"- grasp: 抓取 (robot_skill)"
	if got := st.Summary(); got != want {
		t.Errorf("Summary 渲染不符:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	empty := &Store{skills: map[string]Skill{}}
	if got := empty.Summary(); got != "" {
		t.Errorf("空快照 Summary 应为空串，实际: %q", got)
	}
}

// TestStoreWatchHotUpdate 验证监听热更：写入新技能文件与修改既有技能，
// 经 500ms 去抖后自动 Reload 生效（轮询等待，上限 5s）。
func TestStoreWatchHotUpdate(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "general/a", "---\nname: a\ndescription: 技能 A\n---\n正文 A\n")

	st, err := NewStore(root, testLogger())
	if err != nil {
		t.Fatalf("NewStore 失败: %v", err)
	}
	if err := st.Start(); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer st.Stop()

	// 新增技能目录 + SKILL.md：父目录（已挂载监听）先收到目录创建事件。
	writeSkill(t, root, "general/hot", "---\nname: hot\ndescription: 热更技能\n---\n正文 HOT\n")
	waitFor(t, "新增技能热更生效", func() bool {
		_, ok := st.Get("hot")
		return ok
	})

	// 修改既有技能描述：去抖后 Summary 反映新文本。
	writeSkill(t, root, "general/a", "---\nname: a\ndescription: 技能 A 改\n---\n正文 A2\n")
	waitFor(t, "修改技能热更生效", func() bool {
		sk, ok := st.Get("a")
		return ok && sk.Description == "技能 A 改" && sk.Body == "正文 A2"
	})
	if !strings.Contains(st.Summary(), "技能 A 改") {
		t.Errorf("Summary 应包含热更后的描述，实际: %q", st.Summary())
	}
}

// waitFor 轮询 cond 直到为真（间隔 50ms，上限 5s），超时失败。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s：5s 内未满足条件", what)
}
