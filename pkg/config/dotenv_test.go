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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDotEnv 在临时目录写入 .env 文件并返回路径。
func writeDotEnv(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入临时 .env 失败: %v", err)
	}
	return path
}

// TestLoadDotEnvBasic 验证 KEY=VALUE 解析：export 前缀、单双引号、
// 整行与行尾注释、空行均被正确处理。
func TestLoadDotEnvBasic(t *testing.T) {
	dir := t.TempDir()
	path := writeDotEnv(t, dir, ".env", `
# 整行注释
SEMANTIC_TEST_DOTENV_PLAIN=plain
export SEMANTIC_TEST_DOTENV_EXPORT=exported
SEMANTIC_TEST_DOTENV_DOUBLE="quoted value"
SEMANTIC_TEST_DOTENV_SINGLE='single # 不是注释'
SEMANTIC_TEST_DOTENV_TRAIL=trail # 行尾注释
`)

	res, err := loadDotEnvFiles([]string{path})
	if err != nil {
		t.Fatalf("loadDotEnvFiles 不应返回错误: %v", err)
	}
	if len(res.Files) != 1 || len(res.Files[0].Keys) != 5 {
		t.Fatalf("应加载 1 个文件 5 个键，实际: %+v", res.Files)
	}

	cases := map[string]string{
		"SEMANTIC_TEST_DOTENV_PLAIN":  "plain",
		"SEMANTIC_TEST_DOTENV_EXPORT": "exported",
		"SEMANTIC_TEST_DOTENV_DOUBLE": "quoted value",
		"SEMANTIC_TEST_DOTENV_SINGLE": "single # 不是注释",
		"SEMANTIC_TEST_DOTENV_TRAIL":  "trail",
	}
	for key, want := range cases {
		if got := os.Getenv(key); got != want {
			t.Errorf("%s 应为 %q，实际: %q", key, want, got)
		}
	}
}

// TestLoadDotEnvPriority 验证优先级：进程 env > 先加载的文件 > 后加载的文件，
// 已存在的键一律不被覆盖。
func TestLoadDotEnvPriority(t *testing.T) {
	t.Setenv("SEMANTIC_TEST_DOTENV_PROCESS", "from-process")

	dir := t.TempDir()
	local := writeDotEnv(t, dir, ".env.local", `
SEMANTIC_TEST_DOTENV_PROCESS=from-file
SEMANTIC_TEST_DOTENV_SHARED=from-local
`)
	user := writeDotEnv(t, dir, ".env.user", `
SEMANTIC_TEST_DOTENV_SHARED=from-user
`)

	res, err := loadDotEnvFiles([]string{local, user})
	if err != nil {
		t.Fatalf("loadDotEnvFiles 不应返回错误: %v", err)
	}

	if got := os.Getenv("SEMANTIC_TEST_DOTENV_PROCESS"); got != "from-process" {
		t.Errorf("进程 env 不应被 .env 覆盖，实际: %q", got)
	}
	if got := os.Getenv("SEMANTIC_TEST_DOTENV_SHARED"); got != "from-local" {
		t.Errorf("先加载的 ./.env 应压住后加载的用户级文件，实际: %q", got)
	}
	// 进程 env 占用的键不应计入任何文件的 Keys。
	for _, f := range res.Files {
		for _, k := range f.Keys {
			if k == "SEMANTIC_TEST_DOTENV_PROCESS" {
				t.Errorf("被进程 env 占用的键不应计入加载结果: %+v", res.Files)
			}
		}
	}
}

// TestLoadDotEnvMissing 验证不存在的文件被静默忽略。
func TestLoadDotEnvMissing(t *testing.T) {
	res, err := loadDotEnvFiles([]string{filepath.Join(t.TempDir(), "not-exist.env")})
	if err != nil {
		t.Fatalf("文件不存在不应返回错误: %v", err)
	}
	if len(res.Files) != 0 {
		t.Errorf("不存在的文件不应出现在结果中: %+v", res.Files)
	}
}

// TestLoadDotEnvMalformed 验证非法行（缺少 =）导致聚合前的明确报错，
// 报错信息携带文件路径与行号。
func TestLoadDotEnvMalformed(t *testing.T) {
	dir := t.TempDir()
	path := writeDotEnv(t, dir, ".env", "SEMANTIC_TEST_DOTENV_OK=1\nBAD LINE\n")
	if _, err := loadDotEnvFiles([]string{path}); err == nil {
		t.Fatal("非法行应导致加载失败")
	} else if got := err.Error(); !containsAll(got, path, "第 2 行") {
		t.Errorf("错误应携带文件与行号，实际: %v", err)
	}
}

// containsAll 断言 s 同时包含全部子串。
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
