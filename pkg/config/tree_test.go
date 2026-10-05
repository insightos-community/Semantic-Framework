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

package config

import (
	"errors"
	"strings"
	"testing"
)

// TestTreeRoundTrip 验证配置快照经 Tree 导出后键名为 yaml 风格，
// Duration 以 "10s" 风格字符串表示，且 DecodeTree 可无损还原。
func TestTreeRoundTrip(t *testing.T) {
	cfg := Default()
	tree, err := Tree(cfg)
	if err != nil {
		t.Fatalf("Tree 不应失败: %v", err)
	}

	// 键名为 yaml 风格（snake_case），而非 Go 字段名。
	server, ok := tree["server"].(map[string]any)
	if !ok {
		t.Fatalf("树应含 server 段，实际: %v", tree)
	}
	if _, ok := server["http_addr"]; !ok {
		t.Errorf("server 段应含 http_addr 键: %v", server)
	}
	if _, ok := server["HTTPAddr"]; ok {
		t.Error("树不应出现 Go 字段名 HTTPAddr")
	}
	if got := server["read_timeout"]; got != "10s" {
		t.Errorf("Duration 应序列化为 \"10s\"，实际: %v", got)
	}

	// 还原后与源配置等价（map 顺序无关，重新导出树比对）。
	restored, err := DecodeTree(tree)
	if err != nil {
		t.Fatalf("DecodeTree 不应失败: %v", err)
	}
	restoredTree, err := Tree(restored)
	if err != nil {
		t.Fatalf("再次 Tree 不应失败: %v", err)
	}
	h1, err := TreeHash(tree)
	if err != nil {
		t.Fatalf("TreeHash 不应失败: %v", err)
	}
	h2, err := TreeHash(restoredTree)
	if err != nil {
		t.Fatalf("TreeHash 不应失败: %v", err)
	}
	if h1 != h2 {
		t.Errorf("往返后哈希应一致: %q != %q", h1, h2)
	}
}

// TestTreeHashStable 验证同一快照的哈希稳定，任一值变化哈希随之变化。
func TestTreeHashStable(t *testing.T) {
	tree, err := Tree(Default())
	if err != nil {
		t.Fatalf("Tree 不应失败: %v", err)
	}
	h1, err := TreeHash(tree)
	if err != nil {
		t.Fatalf("TreeHash 不应失败: %v", err)
	}
	h2, err := TreeHash(tree)
	if err != nil {
		t.Fatalf("TreeHash 不应失败: %v", err)
	}
	if h1 != h2 || len(h1) != 64 {
		t.Errorf("哈希应稳定且为 SHA-256 hex（64 字符），实际: %q / %q", h1, h2)
	}

	changed, err := Tree(Default())
	if err != nil {
		t.Fatalf("Tree 不应失败: %v", err)
	}
	changed["log"].(map[string]any)["level"] = "debug"
	h3, err := TreeHash(changed)
	if err != nil {
		t.Fatalf("TreeHash 不应失败: %v", err)
	}
	if h3 == h1 {
		t.Error("值变化后哈希应变化")
	}
}

// TestDecodeTreeValidation 验证 DecodeTree 复用 schema 校验：
// 未知键聚合为 ValidationError（带 yaml 路径）。
func TestDecodeTreeValidation(t *testing.T) {
	tree, err := Tree(Default())
	if err != nil {
		t.Fatalf("Tree 不应失败: %v", err)
	}
	tree["server"].(map[string]any)["http-addr"] = ":9090" // 笔误键名

	_, err = DecodeTree(tree)
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("未知键应返回 ValidationError，实际: %v", err)
	}
	found := false
	for _, p := range verr.Problems {
		if strings.Contains(p, "server.http-addr") {
			found = true
		}
	}
	if !found {
		t.Errorf("问题清单应含 server.http-addr 路径，实际: %v", verr.Problems)
	}
}

// TestMergeTree 验证 merge patch 语义：覆盖、递归合并、nil 删除、base 不被修改。
func TestMergeTree(t *testing.T) {
	base := map[string]any{
		"llm": map[string]any{
			"default": "deepseek-chat",
			"providers": map[string]any{
				"mock": map[string]any{"model": "mock"},
			},
		},
		"log": map[string]any{"level": "info"},
	}
	patch := map[string]any{
		"llm": map[string]any{
			"default": "mock",
			"providers": map[string]any{
				"mock": nil, // 删除该端点
			},
		},
	}

	merged := MergeTree(base, patch)
	llm := merged["llm"].(map[string]any)
	if llm["default"] != "mock" {
		t.Errorf("default 应被覆盖为 mock，实际: %v", llm["default"])
	}
	providers := llm["providers"].(map[string]any)
	if _, ok := providers["mock"]; ok {
		t.Errorf("nil 应删除 providers.mock，实际: %v", providers)
	}
	if merged["log"].(map[string]any)["level"] != "info" {
		t.Error("未触及的 log.level 应保持不变")
	}

	// base 不被修改（快照树只读）。
	if base["llm"].(map[string]any)["default"] != "deepseek-chat" {
		t.Error("MergeTree 不应修改 base 树")
	}
	if _, ok := base["llm"].(map[string]any)["providers"].(map[string]any)["mock"]; !ok {
		t.Error("MergeTree 不应从 base 树删除键")
	}
}

// TestPatchPaths 验证 patch 叶子路径展开（含 nil 删除路径）且按字典序。
func TestPatchPaths(t *testing.T) {
	patch := map[string]any{
		"llm": map[string]any{
			"default": "mock",
			"providers": map[string]any{
				"foo": nil,
			},
		},
		"log": map[string]any{"level": "debug"},
	}
	paths := PatchPaths(patch)
	want := []string{"llm.default", "llm.providers.foo", "log.level"}
	if len(paths) != len(want) {
		t.Fatalf("路径数应为 %d，实际: %v", len(want), paths)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("paths[%d] 应为 %q，实际: %q", i, want[i], paths[i])
		}
	}
}

// TestValidateYAMLExported 验证导出的 ValidateYAML 与启动加载同一套规则。
func TestValidateYAMLExported(t *testing.T) {
	if err := ValidateYAML([]byte("log:\n  level: debug\n")); err != nil {
		t.Errorf("合法配置应通过: %v", err)
	}
	if err := ValidateYAML([]byte("log:\n  levl: debug\n")); err == nil {
		t.Error("未知键应被拒绝")
	}
}
