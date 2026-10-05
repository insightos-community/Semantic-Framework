package robotruntime

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadCatalogReadsReadinessTimeout 验证 bundle.yaml 的
// spec.runtime.readinessTimeout 会被解析进 Bundle：加载大模型的包冷启动慢，
// 需要按包声明放宽等待上限。
func TestLoadCatalogReadsReadinessTimeout(t *testing.T) {
	root := t.TempDir()
	bundleDirectory := filepath.Join(root, "franka-libero", "0.1.0")
	if err := os.MkdirAll(bundleDirectory, 0o750); err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`apiVersion: semantic.insightos.cn/v1alpha1
kind: RobotRuntimeBundle
metadata:
  name: franka-libero
  version: 0.1.0
spec:
  robot:
    model: franka_panda
    backendProfiles:
      - backend: mujoco
        profile: libero-robosuite-1.4
  runtime:
    readinessTimeout: 110s
`)
	if err := os.WriteFile(filepath.Join(bundleDirectory, "bundle.yaml"), manifest, 0o640); err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := catalog.Resolve(MatchKey{
		RobotModel: "franka_panda", Backend: "mujoco", BackendProfile: "libero-robosuite-1.4",
	})
	if err != nil {
		t.Fatalf("无法匹配 franka-libero: %v", err)
	}
	if bundle.ReadinessTimeout != 110*time.Second {
		t.Fatalf("readinessTimeout 应为 110s，实际: %s", bundle.ReadinessTimeout)
	}
}

// TestLoadCatalogToleratesInvalidReadinessTimeout 验证可选字段非法时按未声明
// 处理：就绪超时是可选优化项，不应因为写错一个时长让整个包加载失败。
func TestLoadCatalogToleratesInvalidReadinessTimeout(t *testing.T) {
	for _, raw := range []string{"不是时长", "-5s", "0s"} {
		t.Run(raw, func(t *testing.T) {
			root := t.TempDir()
			bundleDirectory := filepath.Join(root, "bundle")
			if err := os.MkdirAll(bundleDirectory, 0o750); err != nil {
				t.Fatal(err)
			}
			manifest := []byte(`apiVersion: semantic.insightos.cn/v1alpha1
kind: RobotRuntimeBundle
metadata:
  name: tolerant
  version: 0.1.0
spec:
  robot:
    model: tolerant_model
    backendProfiles:
      - backend: fake
        profile: tolerant-fake
  runtime:
    readinessTimeout: "` + raw + `"
`)
			if err := os.WriteFile(filepath.Join(bundleDirectory, "bundle.yaml"), manifest, 0o640); err != nil {
				t.Fatal(err)
			}
			catalog, err := LoadCatalog(root)
			if err != nil {
				t.Fatalf("非法 readinessTimeout 不应让加载失败: %v", err)
			}
			bundle, err := catalog.Resolve(MatchKey{
				RobotModel: "tolerant_model", Backend: "fake", BackendProfile: "tolerant-fake",
			})
			if err != nil {
				t.Fatalf("无法匹配: %v", err)
			}
			if bundle.ReadinessTimeout != 0 {
				t.Fatalf("非法值应回退为 0（未声明），实际: %s", bundle.ReadinessTimeout)
			}
		})
	}
}
