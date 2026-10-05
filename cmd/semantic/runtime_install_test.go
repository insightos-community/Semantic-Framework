package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"insightos.cn/semantic-framework/internal/simulation"
)

func TestMaterializeRuntimePackChecksExistingContent(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "source"), filepath.Join(root, "installed")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	file := func(name, value string) simulation.RuntimePackFile {
		t.Helper()
		if err := os.WriteFile(filepath.Join(source, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(value))
		return simulation.RuntimePackFile{Path: name, SHA256: hex.EncodeToString(sum[:])}
	}
	manifest := simulation.RuntimePackManifest{
		SchemaVersion: 1, PackID: "native-mujoco", PackVersion: "0.4.0",
		Profile: simulation.RuntimeProfile{RuntimeProfileID: "native-mujoco", Engine: "mujoco", Loader: "native"},
		Runner:  "native-mujoco", PythonVersion: "3.10.16", Endpoint: "http://127.0.0.1:8090",
		RequirementsLock:  file("requirements.lock", "dependency==1"),
		Wheels:            []simulation.RuntimePackFile{file("runtime.whl", "original")},
		Wheelhouse:        []simulation.RuntimePackFile{file("dependency.whl", "dependency")},
		Licenses:          []simulation.RuntimePackFile{file("LICENSE", "license")},
		VerificationFiles: []simulation.RuntimePackFile{file("version.json", "{}")},
	}
	writeManifest := func() {
		t.Helper()
		data, err := yaml.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, "runtime-pack.yaml"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeManifest()
	if err := materializeRuntimePack(source, target); err != nil {
		t.Fatal(err)
	}
	if err := materializeRuntimePack(source, target); err != nil {
		t.Fatalf("相同内容应可复用: %v", err)
	}
	manifest.Wheels[0] = file("runtime.whl", "new build")
	writeManifest()
	if err := materializeRuntimePack(source, target); err == nil || !strings.Contains(err.Error(), "内容已改变") {
		t.Fatalf("同版本不同内容不能假报安装成功: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(target, "runtime.whl"))
	if err != nil || string(data) != "original" {
		t.Fatal("拒绝冲突时必须保留已安装文件")
	}
}

func TestActivateRuntimeInstallationCopiesSceneResources(t *testing.T) {
	root := t.TempDir()
	paths := runtimePaths{
		runtimes: filepath.Join(root, "configs", "runtimes.d"),
		scenes:   filepath.Join(root, "configs", "scenes.d"),
	}
	for _, dir := range []string{paths.runtimes, paths.scenes} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	pack := filepath.Join(root, "runtime-packs", "native-mujoco", "0.4.0")
	catalog := `schema_version: 1
catalog_version: 0.4.0
entries:
  - scene_id: demo
    name: Demo
    engine: mujoco
    loader: native
    compatible_runtime_profile: native-mujoco
    versions:
      - version: 1.0.0
        runtime_scene_key: demo
        published: true
        robot_models: [r1]
        variants: [{variant_id: layout001, name: Layout 001, kind: layout}]
        authoring: {mode: none}
`
	for path, data := range map[string]string{
		"catalog/catalog.yaml": catalog, "catalog/previews/demo.svg": "<svg/>",
	} {
		absolute := filepath.Join(pack, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := simulation.RuntimePackManifest{
		SceneCatalog:   simulation.RuntimePackFile{Path: "catalog/catalog.yaml"},
		SceneResources: []simulation.RuntimePackFile{{Path: "catalog/previews/demo.svg"}},
	}
	scenePath := filepath.Join(paths.scenes, "local-native", "catalog.yaml")
	installation := simulation.RuntimeInstallation{
		SchemaVersion: 2, InstallationID: "local-native",
		Profile: simulation.RuntimeProfile{RuntimeProfileID: "native-mujoco", Name: "Native",
			Engine: "mujoco", Loader: "native", APIVersion: "v1"},
		PackID: "native-mujoco", PackVersion: "0.4.0", Runner: "native-mujoco",
		LaunchMode: "process", EnvironmentPath: filepath.Join(root, "runtime-envs", "local-native", "0.4.0"),
		PackPath: pack, SceneCatalogPath: scenePath, Endpoint: "http://127.0.0.1:18090",
		Enabled: true, InstalledVersion: "0.4.0",
	}
	if err := activateRuntimeInstallation(paths, installation, manifest, pack, false); err != nil {
		t.Fatalf("激活 installation 失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(scenePath), "previews", "demo.svg")); err != nil {
		t.Fatalf("场景资源未安装: %v", err)
	}
	loaded, err := simulation.LoadSceneCatalog(paths.scenes)
	if err != nil {
		t.Fatal(err)
	}
	items := loaded.List("native-mujoco")
	if len(items) != 1 || items[0].SceneID != "demo" {
		t.Fatalf("catalog=%+v", items)
	}
	if _, err := simulation.LoadRuntimeInstallationFile(filepath.Join(paths.runtimes, "local-native.yaml")); err != nil {
		t.Fatalf("生成的 installation 无法重新加载: %v", err)
	}
}

func TestInstallationIDAndPathContainment(t *testing.T) {
	if installationIDValid("../escape") || installationIDValid("Upper") || !installationIDValid("local-native.1") {
		t.Fatal("installation id 校验错误")
	}
	root := t.TempDir()
	if !pathInside(root, filepath.Join(root, "child")) || pathInside(root, root) || pathInside(root, filepath.Dir(root)) {
		t.Fatal("卸载路径边界校验错误")
	}
}

func TestRuntimePackArchiveEntryAndExtractedTreeSafety(t *testing.T) {
	for _, value := range []string{"catalog/", "catalog/catalog.yaml", "wheels/runtime.whl"} {
		if err := validateRuntimeArchiveEntry(value); err != nil {
			t.Fatalf("正常 Pack 路径 %q 被拒绝: %v", value, err)
		}
	}
	for _, value := range []string{"../escape", "/absolute", "catalog//item", "C:/pack"} {
		if err := validateRuntimeArchiveEntry(value); err == nil {
			t.Fatalf("不安全 Pack 路径 %q 未被拒绝", value)
		}
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "regular"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateExtractedRuntimePackTree(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "regular"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := validateExtractedRuntimePackTree(root); err == nil {
		t.Fatal("Runtime Pack 中的符号链接必须被拒绝")
	}
}

func TestReleaseDoctorRejectsDevelopmentPackBeforeRuntimeProbe(t *testing.T) {
	item := simulation.RuntimeInstallation{
		PackVersion: "0.4.0-dev.0", Development: false, Enabled: true,
	}
	err := diagnoseRuntimeInstallation(item, false, true)
	if err == nil || err.Error() != "开发版本 Runtime Pack 不得用于 RC 或正式制品验收" {
		t.Fatalf("err=%v", err)
	}
}
