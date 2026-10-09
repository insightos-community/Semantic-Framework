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

// Package storetest 为各包的测试提供“已迁移到最新 schema”的 SQLite 库。
//
// 为什么需要它：Migrate 要按版本号顺序执行 34 个迁移，每个迁移一个事务；
// 在 -race 与 coverage 同时开启时单次约 1.1 秒（本机实测：profile/llm 装配
// 0ms，Open 2ms，Migrate 1076–1102ms）。几乎每个数据库测试都从空库重建，
// 于是这个固定成本在包内叠加几十次，高过被测逻辑本身：agent/runtime 包 104
// 个测试共 91.8 秒，其中约 70 秒只是重复迁移。
//
// 这里的做法是：进程内只构建一份已迁移模板，每个测试拿到模板的私有文件
// 副本（复制 0–1ms，Open 15ms）。每个测试仍然拥有独占、已迁移、与直接调用
// Migrate 完全等价的库，因此测试间的隔离性与断言语义不变；只是不再重复
// 支付迁移成本。迁移代码本身仍被覆盖：模板构建就在同一测试二进制内执行。
// 同一台机器、同参数（-p 2 -race -coverprofile）复测：internal/agent/runtime
// 由 97.5 秒降到 8.2 秒，internal/store 由 96.1 秒降到 6.1 秒，全量由 5 分 40 秒
// 降到 1 分 38 秒；总覆盖率不变（59.0%）。
//
// 自带装配入口（不接受已打开的 Store，只接受库路径）的测试用 SeedMigratedAt，
// 效果相同：把模板放到该路径，随后 Migrate 只剩版本检查。
//
// 用法：在各包的 TestMain 中接入一次，即可共享同一份已迁移模板，例如
//
//	func TestMain(m *testing.M) { storetest.Main(m) }
package storetest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/log"
)

// TB 只包含辅助函数需要的能力，避免非测试包直接依赖 testing。
type TB interface {
	Helper()
	TempDir() string
	Fatalf(format string, args ...any)
	Cleanup(func())
}

const (
	driverSQLite = "sqlite"
	databaseName = "test.db"
)

var (
	templateOnce sync.Once
	templatePath string
	templateErr  error
	templateDir  string
)

// Main 包装 m.Run，并在测试进程退出前回收共享模板目录。各包按包注释中的
// TestMain 用法接入，避免每次测试都在临时目录留下模板副本。
func Main(m *testing.M) {
	code := m.Run()
	CleanupTemplate()
	os.Exit(code)
}

// CleanupTemplate 删除共享模板目录；未调用时模板会留在系统临时目录，
// 因此建议各测试包通过 Main 接入。
func CleanupTemplate() {
	if templateDir != "" {
		_ = os.RemoveAll(templateDir)
	}
}

// OpenMigrated 返回一个已迁移到最新 schema 的私有 Store，并在测试结束时关闭。
// 调用方无需再调用 Migrate。
func OpenMigrated(tb TB, logger *log.Logger) *store.Store {
	tb.Helper()
	path, err := templateSnapshot()
	if err != nil {
		tb.Fatalf("构建已迁移模板库失败: %v", err)
	}
	target := filepath.Join(tb.TempDir(), databaseName)
	if err := copyFile(path, target); err != nil {
		tb.Fatalf("复制已迁移模板库失败: %v", err)
	}
	st, err := store.Open(config.StoreConfig{Driver: driverSQLite, SQLitePath: target}, logger)
	if err != nil {
		tb.Fatalf("打开已迁移库失败: %v", err)
	}
	tb.Cleanup(func() { _ = st.Close() })
	return st
}

// OpenMigratedAt 与 OpenMigrated 相同，但允许调用方指定库文件位置。
func OpenMigratedAt(tb TB, logger *log.Logger, path string) *store.Store {
	tb.Helper()
	template, err := templateSnapshot()
	if err != nil {
		tb.Fatalf("构建已迁移模板库失败: %v", err)
	}
	if err := copyFile(template, path); err != nil {
		tb.Fatalf("复制已迁移模板库失败: %v", err)
	}
	st, err := store.Open(config.StoreConfig{Driver: driverSQLite, SQLitePath: path}, logger)
	if err != nil {
		tb.Fatalf("打开已迁移库失败: %v", err)
	}
	tb.Cleanup(func() { _ = st.Close() })
	return st
}

// SeedMigratedAt 把已迁移模板复制到调用方指定的位置，但不打开它；目标已存在时
// 保持原样。适合自行 Open 的装配入口（例如 bootstrap.Wire 只接受配置里的库路径）：
// 它们随后的 Migrate 只剩一次版本检查，而不再重放全部迁移。
// 目标已存在时的保留语义是为“同一路径二次启动”这类重启场景准备的：第二次调用
// 必须看到第一次留下的数据，而不是一份新的空库。
func SeedMigratedAt(tb TB, path string) {
	tb.Helper()
	if _, err := os.Stat(path); err == nil {
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		tb.Fatalf("检查目标库失败: %v", err)
	}
	template, err := templateSnapshot()
	if err != nil {
		tb.Fatalf("构建已迁移模板库失败: %v", err)
	}
	if err := copyFile(template, path); err != nil {
		tb.Fatalf("复制已迁移模板库失败: %v", err)
	}
}

// templateSnapshot 首次调用时构建模板并返回其路径。
func templateSnapshot() (string, error) {
	templateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "semantic-storetest-template")
		if err != nil {
			templateErr = err
			return
		}
		templateDir = dir
		path := filepath.Join(dir, databaseName)
		logger := log.New(log.Options{Level: log.LevelError, Writer: discard{}})
		st, err := store.Open(config.StoreConfig{Driver: driverSQLite, SQLitePath: path}, logger)
		if err != nil {
			templateErr = err
			return
		}
		if err := st.Migrate(); err != nil {
			_ = st.Close()
			templateErr = fmt.Errorf("Migrate: %w", err)
			return
		}
		// Close 会关闭唯一的连接，SQLite 在最后一个连接关闭时完成 WAL 检查点，
		// 因此主库文件此时自包含；只有它的副本才可安全当作独立库使用。
		if err := st.Close(); err != nil {
			templateErr = fmt.Errorf("Close: %w", err)
			return
		}
		templatePath = path
	})
	return templatePath, templateErr
}

func copyFile(source, target string) error {
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(target, content, 0o600)
}

// discard 让模板构建过程保持安静，避免在正常测试输出里混入迁移日志。
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
