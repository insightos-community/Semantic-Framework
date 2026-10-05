package bootstrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"insightos.cn/semantic-framework/internal/simulation"
)

func TestConfiguredSceneAssetCatalogLoadsEnvironmentFile(t *testing.T) {
	catalog := simulation.DefaultSceneAssetCatalog()
	catalog.CatalogVersion = "0.4.1-test"
	data, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(t.TempDir(), "asset-catalog.json")
	if err := os.WriteFile(filePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SEMANTIC_SIMULATION_ASSET_CATALOG", filePath)

	got, err := configuredSceneAssetCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if got.CatalogVersion != catalog.CatalogVersion ||
		len(got.Entries) != len(catalog.Entries) {
		t.Fatalf("环境配置目录没有生效: %+v", got)
	}
}

func TestConfiguredSceneAssetCatalogRejectsInvalidFile(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "asset-catalog.json")
	if err := os.WriteFile(filePath, []byte(`{"schema_version":"2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SEMANTIC_SIMULATION_ASSET_CATALOG", filePath)

	_, err := configuredSceneAssetCatalog()
	if err == nil ||
		!strings.Contains(err.Error(), "SEMANTIC_SIMULATION_ASSET_CATALOG") ||
		!strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("错误配置没有在启动装配阶段明确失败: %v", err)
	}
}
