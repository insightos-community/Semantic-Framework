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
	"os"
	"path/filepath"
	"testing"
)

// TestReadResourceRejectsTraversalAndExternalSymlink 验证管理界面的资源读取
// 边界不能被路径穿越或 Skill 包内的外部符号链接绕过。
func TestReadResourceRejectsTraversalAndExternalSymlink(t *testing.T) {
	root := t.TempDir()
	scripts := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(scripts, "external.txt")); err != nil {
		t.Fatal(err)
	}
	sk := Skill{Name: "safe", Dir: root}

	for _, path := range []string{"../secret.txt", "SKILL.md", "/etc/passwd"} {
		if _, _, err := ReadResource(sk, path); !errors.Is(err, ErrInvalidResourcePath) {
			t.Fatalf("非法路径 %q 应被拒绝，实际: %v", path, err)
		}
	}
	if _, _, err := ReadResource(sk, "scripts/external.txt"); !errors.Is(err, ErrInvalidResourcePath) {
		t.Fatalf("指向 Skill 外部的符号链接应被拒绝，实际: %v", err)
	}
}
