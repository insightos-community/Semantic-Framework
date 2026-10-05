package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

func inspectSourceManifest(data []byte) (Package, error) {
	var recipe SourceBuild
	d := yaml.NewDecoder(strings.NewReader(string(data)))
	d.KnownFields(true)
	if err := d.Decode(&recipe); err != nil {
		return Package{}, err
	}
	if recipe.Kind != "robot_ability" || recipe.SchemaVersion != 1 || !componentSegment(recipe.Name) || !componentSegment(recipe.Version) {
		return Package{}, fmt.Errorf("源码包需要有效的 Robot Ability semantic-source.yaml")
	}
	return Package{Kind: "robot_ability_source", Name: recipe.Name, Version: recipe.Version}, nil
}

// 上传源码以同一个显式安装动作构建。包内需包含声明的依赖源码或 Wheel，
// 本地 CLI 则可直接引用工作区的兄弟仓库；二者最终产出相同的组件安装格式。
func BuildUploadedSource(ctx context.Context, archive, root, output string, progress func(string)) error {
	if err := ExtractZipContext(ctx, archive, root); err != nil {
		return err
	}
	body, err := os.ReadFile(filepath.Join(root, "semantic-source.yaml"))
	if err != nil {
		return err
	}
	if _, err := inspectSourceManifest(body); err != nil {
		return err
	}
	var recipe SourceBuild
	if err := yaml.Unmarshal(body, &recipe); err != nil {
		return err
	}
	paths := append([]string{}, recipe.Build.PythonProjects...)
	for _, path := range recipe.Build.AbilityDirectories {
		paths = append(paths, path)
	}
	for _, path := range recipe.Build.Files {
		paths = append(paths, path)
	}
	if recipe.Build.RequirementsLock != "" {
		paths = append(paths, recipe.Build.RequirementsLock)
	}
	for _, path := range paths {
		if path != "." && !safeRelative(path) {
			return fmt.Errorf("上传源码的依赖必须包含在包内: %s", path)
		}
	}
	return buildSource(ctx, root, output, filepath.Join(filepath.Dir(filepath.Dir(root)), "build-cache"), progress)
}
