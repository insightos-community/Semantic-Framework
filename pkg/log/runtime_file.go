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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	// runtimeLogMaxSizeMB 限制单个运行日志文件大小，避免长期运行无限占用磁盘。
	runtimeLogMaxSizeMB = 100
	// runtimeLogMaxBackups 保留最近的轮转文件，旧文件由成熟的 lumberjack 组件清理。
	runtimeLogMaxBackups = 5
	// runtimeLogMaxAgeDays 为轮转日志保留天数。
	runtimeLogMaxAgeDays = 14
)

// RuntimeFile 表示 Semantic Server 的持久化运行日志。
// 它实现 io.WriteCloser，可直接与标准输出组成 io.MultiWriter。
type RuntimeFile struct {
	path   string
	writer *lumberjack.Logger
}

// OpenRuntimeFile 根据当前实例的 SQLite 路径打开滚动日志文件。
//
// 标准安装结构为 <instance>/data/semantic.db 与
// <instance>/logs/semantic-server.jsonl。日志与数据库共享实例根目录，确保 Server 使用 -c 或
// SEMANTIC_CONFIG 切换安装实例时，日志不会继续写入固定的仓库目录。
// 目录与文件权限分别收敛到 0700 和 0600，避免运行日志意外暴露请求信息。
func OpenRuntimeFile(sqlitePath string) (*RuntimeFile, error) {
	if strings.TrimSpace(sqlitePath) == "" {
		return nil, fmt.Errorf("SQLite 路径为空，无法确定运行日志目录")
	}

	databaseDir := filepath.Dir(filepath.Clean(sqlitePath))
	instanceRoot := databaseDir
	if filepath.Base(databaseDir) == "data" {
		instanceRoot = filepath.Dir(databaseDir)
	}
	logDir := filepath.Join(instanceRoot, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建运行日志目录失败: %w", err)
	}
	// MkdirAll 不会收紧已有目录的权限，因此需要显式修正。
	if err := os.Chmod(logDir, 0o700); err != nil {
		return nil, fmt.Errorf("设置运行日志目录权限失败: %w", err)
	}

	logPath := filepath.Join(logDir, "semantic-server.jsonl")
	// lumberjack 第一次写入时才打开文件。这里预先创建并收紧权限，
	// 使启动完成前日志路径就可被诊断工具发现，也避免继承过宽权限。
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("创建运行日志文件失败: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("关闭运行日志预创建文件失败: %w", err)
	}
	if err := os.Chmod(logPath, 0o600); err != nil {
		return nil, fmt.Errorf("设置运行日志文件权限失败: %w", err)
	}

	return &RuntimeFile{
		path: logPath,
		writer: &lumberjack.Logger{
			Filename:   logPath,
			MaxSize:    runtimeLogMaxSizeMB,
			MaxBackups: runtimeLogMaxBackups,
			MaxAge:     runtimeLogMaxAgeDays,
			Compress:   true,
			LocalTime:  true,
		},
	}, nil
}

// Path 返回当前实例实际使用的日志文件路径。
func (f *RuntimeFile) Path() string {
	if f == nil {
		return ""
	}
	return f.path
}

// Write 将结构化日志写入滚动文件。
func (f *RuntimeFile) Write(p []byte) (int, error) {
	if f == nil || f.writer == nil {
		return 0, fmt.Errorf("运行日志文件未初始化")
	}
	return f.writer.Write(p)
}

// Close 关闭当前打开的日志文件；重复关闭由 lumberjack 安全处理。
func (f *RuntimeFile) Close() error {
	if f == nil || f.writer == nil {
		return nil
	}
	return f.writer.Close()
}

var _ io.WriteCloser = (*RuntimeFile)(nil)
