package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
	"insightos.cn/semantic-framework/internal/simulation"
)

// Runtime 与 Ability 共用 Wheel 构建和依赖锁收集；产物继续使用原有 Runtime
// Pack 契约。模板只描述 Profile 和资源，构建器补齐本次实际文件的摘要，框架
// 不解释引擎、场景或模型业务，也不在构建时启动仿真。
func finalizeRuntimeBuild(root string, source Component) error {
	path := filepath.Join(root, "runtime-pack.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("Runtime 配方需要 build.files 中的 runtime-pack.yaml: %w", err)
	}
	var manifest simulation.RuntimePackManifest
	decoder := yaml.NewDecoder(strings.NewReader(string(body)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		return err
	}
	if manifest.PackID != source.Name || manifest.PackVersion != source.Version {
		return fmt.Errorf("Runtime 源码配方与 Pack 模板的名称/版本不一致")
	}
	local := map[string]bool{}
	for _, wheel := range manifest.Wheels {
		local[wheel.Path] = true
	}
	manifest.Wheelhouse = nil
	if err := os.MkdirAll(filepath.Join(root, "wheelhouse"), 0750); err != nil {
		return err
	}
	wheels, err := os.ReadDir(filepath.Join(root, "wheels"))
	if err != nil {
		return err
	}
	for _, wheel := range wheels {
		name := "wheels/" + wheel.Name()
		if strings.HasSuffix(name, ".whl") && !local[name] {
			target := "wheelhouse/" + wheel.Name()
			if err := os.Rename(filepath.Join(root, name), filepath.Join(root, target)); err != nil {
				return err
			}
			manifest.Wheelhouse = append(manifest.Wheelhouse, simulation.RuntimePackFile{Path: target})
		}
	}
	fill := func(file *simulation.RuntimePackFile) error {
		if !safeRelative(file.Path) {
			return fmt.Errorf("Runtime 构建文件路径无效: %s", file.Path)
		}
		digest, err := ArchiveDigest(filepath.Join(root, filepath.FromSlash(file.Path)))
		file.SHA256 = digest
		return err
	}
	for _, file := range []*simulation.RuntimePackFile{&manifest.RequirementsLock, manifest.Settings, &manifest.SceneCatalog, &manifest.SmokeRequest} {
		if file != nil && file.Path != "" {
			if err := fill(file); err != nil {
				return err
			}
		}
	}
	for _, files := range [][]simulation.RuntimePackFile{manifest.Wheels, manifest.Wheelhouse, manifest.Licenses, manifest.VerificationFiles, manifest.SceneResources} {
		for index := range files {
			if err := fill(&files[index]); err != nil {
				return err
			}
		}
	}
	body, err = yaml.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, body, 0640); err != nil {
		return err
	}
	_, err = simulation.LoadRuntimePack(root)
	return err
}
