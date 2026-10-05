package simulation

import (
	"testing"
	"time"
)

func TestCatalogSceneStartTimeoutPreservesDefaultAndProfileBudget(t *testing.T) {
	if got := catalogSceneStartTimeout(RuntimeProfile{}); got != 3*time.Minute {
		t.Fatalf("未声明预算的现有 Runtime 默认值改变: %v", got)
	}
	if got := catalogSceneStartTimeout(RuntimeProfile{SceneStartTimeoutSeconds: 900}); got != 15*time.Minute {
		t.Fatalf("未采用 Runtime 声明的场景加载预算: %v", got)
	}
}
