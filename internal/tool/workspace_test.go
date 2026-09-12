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

package tool

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveWorkspacePath 验证普通相对路径可解析，目录越界和符号链接逃逸
// 均被共享边界函数拒绝。
func TestResolveWorkspacePath(t *testing.T) {
	workspace := t.TempDir()
	file := filepath.Join(workspace, "reports", "result.json")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatal(err)
	}
	abs, rel, err := ResolveWorkspacePath(workspace, "reports/result.json")
	if err != nil || abs != file || rel != "reports/result.json" {
		t.Fatalf("相对路径解析不一致: abs=%q rel=%q err=%v", abs, rel, err)
	}
	if _, _, err := ResolveWorkspacePath(workspace, "../outside"); err == nil {
		t.Fatal("目录越界应被拒绝")
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveWorkspacePath(workspace, "escape"); err == nil {
		t.Fatal("符号链接逃逸应被拒绝")
	}
}
