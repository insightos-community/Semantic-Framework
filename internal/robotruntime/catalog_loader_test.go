package robotruntime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCatalogReadsBundleMatchProfiles(t *testing.T) {
	root := t.TempDir()
	bundleDirectory := filepath.Join(root, "r1pro-mujoco", "0.5.0")
	if err := os.MkdirAll(bundleDirectory, 0o750); err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`apiVersion: semantic.insightos.cn/v1alpha1
kind: RobotRuntimeBundle
metadata:
  name: r1pro-mujoco
  version: 0.5.0
spec:
  robot:
    model: r1_pro_chassis
    backendProfiles:
      - backend: mujoco
        profile: r1pro-tote-mujoco-v1
      - backend: fake
        profile: r1pro-tote-fake-v1
`)
	if err := os.WriteFile(filepath.Join(bundleDirectory, "bundle.yaml"), manifest, 0o640); err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []MatchKey{
		{RobotModel: "r1_pro_chassis", Backend: "mujoco", BackendProfile: "r1pro-tote-mujoco-v1"},
		{RobotModel: "r1_pro_chassis", Backend: "fake", BackendProfile: "r1pro-tote-fake-v1"},
	} {
		bundle, resolveErr := catalog.Resolve(key)
		if resolveErr != nil {
			t.Fatalf("无法匹配 %+v: %v", key, resolveErr)
		}
		if bundle.Path != bundleDirectory || bundle.Name != "r1pro-mujoco" || bundle.Version != "0.5.0" {
			t.Fatalf("Bundle 信息错误: %+v", bundle)
		}
	}
}

func TestLoadCatalogAllowsInstallationBeforeRobotStart(t *testing.T) {
	catalog, err := LoadCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Resolve(MatchKey{RobotModel: "test", Backend: "fake", BackendProfile: "test"}); err == nil {
		t.Fatal("安装前仍不能启动 Robot")
	}
}
