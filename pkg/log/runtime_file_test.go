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

package log

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenRuntimeFileFollowsInstallationRoot(t *testing.T) {
	instanceDir := filepath.Join(t.TempDir(), "instance")
	dataDir := filepath.Join(instanceDir, "data")
	logFile, err := OpenRuntimeFile(filepath.Join(dataDir, "semantic.db"))
	if err != nil {
		t.Fatalf("打开运行日志失败: %v", err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	wantPath := filepath.Join(instanceDir, "logs", "semantic-server.jsonl")
	if logFile.Path() != wantPath {
		t.Fatalf("日志路径应跟随数据库目录，want=%q got=%q", wantPath, logFile.Path())
	}

	logger := New(Options{Level: LevelInfo, Writer: logFile})
	logger.Info("运行日志测试", "trace_id", "trace-test")
	if err := logFile.Close(); err != nil {
		t.Fatalf("关闭运行日志失败: %v", err)
	}

	content, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("读取运行日志失败: %v", err)
	}
	if !strings.Contains(string(content), `"msg":"运行日志测试"`) ||
		!strings.Contains(string(content), `"trace_id":"trace-test"`) {
		t.Fatalf("运行日志缺少结构化字段: %s", content)
	}
}

func TestOpenRuntimeFileWithoutDataDirectoryStaysBesideDatabase(t *testing.T) {
	instanceDir := filepath.Join(t.TempDir(), "custom-instance")
	logFile, err := OpenRuntimeFile(filepath.Join(instanceDir, "semantic.db"))
	if err != nil {
		t.Fatalf("打开自定义实例运行日志失败: %v", err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	wantPath := filepath.Join(instanceDir, "logs", "semantic-server.jsonl")
	if logFile.Path() != wantPath {
		t.Fatalf("自定义数据库路径的日志目录错误，want=%q got=%q", wantPath, logFile.Path())
	}
}

func TestOpenRuntimeFileRestrictsPermissions(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	logFile, err := OpenRuntimeFile(filepath.Join(dataDir, "semantic.db"))
	if err != nil {
		t.Fatalf("打开运行日志失败: %v", err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	dirInfo, err := os.Stat(filepath.Dir(logFile.Path()))
	if err != nil {
		t.Fatalf("读取日志目录信息失败: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("日志目录权限应为 0700，实际为 %o", got)
	}

	fileInfo, err := os.Stat(logFile.Path())
	if err != nil {
		t.Fatalf("读取日志文件信息失败: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("日志文件权限应为 0600，实际为 %o", got)
	}
}

func TestOpenRuntimeFileRejectsEmptySQLitePath(t *testing.T) {
	if _, err := OpenRuntimeFile("  "); err == nil {
		t.Fatal("SQLite 路径为空时应返回错误")
	}
}
