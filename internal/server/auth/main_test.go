package auth

import (
	"testing"

	"insightos.cn/semantic-framework/internal/store/storetest"
)

// TestMain 负责回收共享的已迁移模板库（见 storetest.OpenMigrated）。
func TestMain(m *testing.M) { storetest.Main(m) }
