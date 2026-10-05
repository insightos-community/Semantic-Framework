package store

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"insightos.cn/semantic-framework/pkg/config"
)

// 为什么这里有一份与 internal/store/storetest 等价的实现：本包测试不能导入
// storetest，那会形成 store[test] → storetest → store 的测试导入环。耗时依据
// 与设计取舍见 storetest 包注释：Migrate 在 -race + coverage 下单次约 1.1 秒，
// 而本包有 70 多个测试要用库，逐个从空库迁移会让固定成本高过被测逻辑。
var (
	migratedFixtureOnce sync.Once
	migratedFixturePath string
	migratedFixtureDir  string
	migratedFixtureErr  error
)

// openMigratedStore 在临时目录打开一个已迁移到最新 schema 的 Store，
// 测试结束自动关闭。
func openMigratedStore(t *testing.T) *Store {
	t.Helper()
	return openMigratedStoreAt(t, filepath.Join(t.TempDir(), "test.db"))
}

// openMigratedStoreAt 与 openMigratedStore 相同，但使用调用方给定的库路径。
// 路径已存在时保留既有内容，只做打开（服务重启、同库复用等场景依赖这一点）。
func openMigratedStoreAt(t *testing.T, path string) *Store {
	t.Helper()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		content, readErr := os.ReadFile(migratedFixtureTemplate(t))
		if readErr != nil {
			t.Fatalf("读取已迁移模板失败: %v", readErr)
		}
		if writeErr := os.WriteFile(path, content, 0o600); writeErr != nil {
			t.Fatalf("复制已迁移模板失败: %v", writeErr)
		}
	}
	st, err := Open(config.StoreConfig{Driver: driverSQLite, SQLitePath: path}, testLogger())
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// migratedFixtureTemplate 首次调用时构建模板并返回其路径。模板本身仍由本包
// 真实的 Open + Migrate 产生，因此迁移代码依旧被这棵测试树覆盖。
func migratedFixtureTemplate(t *testing.T) string {
	t.Helper()
	migratedFixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "semantic-store-migrated-fixture")
		if err != nil {
			migratedFixtureErr = err
			return
		}
		migratedFixtureDir = dir
		seed := filepath.Join(dir, "template.db")
		st, err := Open(config.StoreConfig{Driver: driverSQLite, SQLitePath: seed}, testLogger())
		if err != nil {
			migratedFixtureErr = err
			return
		}
		if err := st.Migrate(); err != nil {
			_ = st.Close()
			migratedFixtureErr = err
			return
		}
		// 最后一个连接关闭时 SQLite 完成 WAL 检查点，主库文件此时自包含，
		// 只有它的副本才可以当作独立库使用。
		if err := st.Close(); err != nil {
			migratedFixtureErr = err
			return
		}
		migratedFixturePath = seed
	})
	if migratedFixtureErr != nil {
		t.Fatalf("构建已迁移模板库失败: %v", migratedFixtureErr)
	}
	return migratedFixturePath
}

// TestMain 负责回收共享的已迁移模板库。
func TestMain(m *testing.M) {
	code := m.Run()
	if migratedFixtureDir != "" {
		_ = os.RemoveAll(migratedFixtureDir)
	}
	os.Exit(code)
}
