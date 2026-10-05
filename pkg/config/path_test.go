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
	"path/filepath"
	"testing"
)

// TestDefaultPath 验证默认安装位置稳定落在当前用户的 ~/.semantic/configs。
func TestDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath 失败: %v", err)
	}
	want := filepath.Join(home, ".semantic", "configs", "semantic-server.yaml")
	if path != want {
		t.Fatalf("默认配置路径 = %q，期望 %q", path, want)
	}
}

// TestResolvePathPriority 验证配置目标严格遵循 -c、SEMANTIC_CONFIG、默认
// 安装位置的优先级，保证前端写回目标与 Server 启动读取目标一致。
func TestResolvePathPriority(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	envPath := filepath.Join(t.TempDir(), "from-env.yaml")
	t.Setenv(ConfigPathEnv, envPath)

	got, err := ResolvePath("")
	if err != nil || got != envPath {
		t.Fatalf("env 路径解析不符: got=%q err=%v", got, err)
	}
	explicit := filepath.Join(t.TempDir(), "explicit.yaml")
	got, err = ResolvePath(explicit)
	if err != nil || got != explicit {
		t.Fatalf("显式路径应优先: got=%q err=%v", got, err)
	}

	t.Setenv(ConfigPathEnv, "")
	got, err = ResolvePath("")
	if err != nil {
		t.Fatalf("默认路径解析失败: %v", err)
	}
	want, _ := filepath.Abs(filepath.Join(home, ".semantic", "configs", "semantic-server.yaml"))
	if got != want {
		t.Fatalf("默认路径 = %q，期望 %q", got, want)
	}
}
