package install

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// BuildInstallPackage 将已有组件包按清单封装，不重新构建 Wheel 或下载模型。
// 只收集清单引用的文件，输出采用 Store，避免对已压缩的模型包重复压缩。
func BuildInstallPackage(source, destination string) error {
	body, err := os.ReadFile(filepath.Join(source, "semantic-package.yaml"))
	if err != nil {
		return err
	}
	var manifest struct {
		Components []PackageEntry `yaml:"components"`
	}
	if err := yaml.Unmarshal(body, &manifest); err != nil {
		return err
	}
	files := []*zip.File{}
	for _, entry := range manifest.Components {
		if !safeRelative(entry.File) {
			return fmt.Errorf("包内组件路径无效: %s", entry.File)
		}
		path := filepath.Join(source, entry.File)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("组件需要普通文件: %s", entry.File)
		}
		child, err := InspectArchive(path)
		if err != nil {
			return fmt.Errorf("组件 %s: %w", entry.ID, err)
		}
		if child.Kind == "package" || child.Kind == "runtime" {
			return fmt.Errorf("组件 %s 请单独导入，安装包只封装一层项目组件", entry.ID)
		}
		header := zip.FileHeader{Name: entry.File, UncompressedSize64: uint64(info.Size())}
		header.SetMode(0640)
		files = append(files, &zip.File{FileHeader: header})
	}
	if _, err := inspectInstallPackage(body, files); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0750); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(destination), ".package-build-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	z := zip.NewWriter(temp)
	f, err := z.Create("semantic-package.yaml")
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		return err
	}
	for _, file := range files {
		entry, err := z.CreateHeader(&file.FileHeader)
		if err != nil {
			return err
		}
		input, err := os.Open(filepath.Join(source, file.Name))
		if err != nil {
			return err
		}
		_, err = io.Copy(entry, input)
		_ = input.Close()
		if err != nil {
			return err
		}
	}
	if err := z.Close(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), destination)
}

// PackageEntry 只组织已有组件包，不复制 Ability、Skill 的版本和执行契约。
// Requires 使用同一安装包内的条目 ID，选装时自动补齐依赖并确定安装顺序。
type PackageEntry struct {
	ID       string   `yaml:"id" json:"id"`
	File     string   `yaml:"file" json:"file"`
	Requires []string `yaml:"requires,omitempty" json:"requires,omitempty"`
}

func inspectInstallPackage(data []byte, files []*zip.File) (Package, error) {
	var manifest struct {
		SchemaVersion int            `yaml:"schema_version"`
		Name          string         `yaml:"name"`
		Version       string         `yaml:"version"`
		Components    []PackageEntry `yaml:"components"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		return Package{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Package{}, fmt.Errorf("安装包需要唯一的 YAML 清单文档")
	}
	if manifest.SchemaVersion != 1 || !componentSegment(manifest.Name) || !componentSegment(manifest.Version) || len(manifest.Components) == 0 {
		return Package{}, fmt.Errorf("安装包需要 schema_version=1、名称、版本和组件清单")
	}
	paths := map[string]*zip.File{}
	for _, f := range files {
		if paths[f.Name] != nil {
			return Package{}, fmt.Errorf("安装包路径重复: %s", f.Name)
		}
		paths[f.Name] = f
	}
	var expanded uint64
	for _, entry := range manifest.Components {
		file := paths[entry.File]
		if !safeRelative(entry.File) || file == nil || !file.Mode().IsRegular() || !strings.HasSuffix(entry.File, ".zip") {
			return Package{}, fmt.Errorf("组件 %s 需要包内有效的 ZIP 文件: %s", entry.ID, entry.File)
		}
		if file.UncompressedSize64 > uint64(MaxUploadBytes) {
			return Package{}, fmt.Errorf("组件 %s 超过安装大小限制", entry.ID)
		}
		if file.UncompressedSize64 > 64<<30-expanded {
			return Package{}, fmt.Errorf("安装包展开超过 64 GiB")
		}
		expanded += file.UncompressedSize64
	}
	if _, err := SelectPackageEntries(manifest.Components, nil); err != nil {
		return Package{}, err
	}
	return Package{Kind: "package", Name: manifest.Name, Version: manifest.Version, Components: manifest.Components}, nil
}

// nil 表示安装全部；显式空数组表示用户未选择。依赖按拓扑顺序安装，
// 不用清单书写顺序推测 SDK、Ability、模型之间的关系。
func SelectPackageEntries(entries []PackageEntry, selected []string) ([]PackageEntry, error) {
	byID := map[string]PackageEntry{}
	for _, entry := range entries {
		if !componentSegment(entry.ID) || byID[entry.ID].ID != "" {
			return nil, fmt.Errorf("组件 ID 无效或重复: %s", entry.ID)
		}
		byID[entry.ID] = entry
	}
	if selected == nil {
		for _, entry := range entries {
			selected = append(selected, entry.ID)
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("请至少选择一个组件")
	}
	state := map[string]int{}
	var ordered []PackageEntry
	var visit func(string) error
	visit = func(id string) error {
		entry, ok := byID[id]
		if !ok {
			return fmt.Errorf("安装包缺少组件或依赖: %s", id)
		}
		if state[id] == 1 {
			return fmt.Errorf("组件依赖存在循环: %s", id)
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		for _, dependency := range entry.Requires {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		state[id] = 2
		ordered = append(ordered, entry)
		return nil
	}
	for _, id := range selected {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

// BatchInstall 仅增加批量编排。所有选中子包先完成类型检查，然后逐个交给
// 原安装器；组件身份仍使用各自内容摘要，因此单独上传同一组件可以复用安装。
// Runtime 独立安装；包内不嵌套另一份安装集合，避免隐式展开复杂交付层级。
func BatchInstall(ctx context.Context, projectID, archive string, options Options, installer Installer, progress func(string)) ([]string, error) {
	pkg, err := InspectArchive(archive)
	if err != nil {
		return nil, err
	}
	entries, err := SelectPackageEntries(pkg.Components, options.Components)
	if err != nil {
		return nil, err
	}
	z, err := zip.OpenReader(archive)
	if err != nil {
		return nil, err
	}
	defer z.Close()
	// 临时子包与原包放在同一安装磁盘，避免大型权重挤占系统 /tmp；安装后删除。
	temp, err := os.MkdirTemp(filepath.Dir(archive), ".package-install-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temp)
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		progress("检查组件 " + entry.ID)
		reader, err := z.Open(entry.File)
		if err != nil {
			return nil, err
		}
		id, err := CopyArchive(&archiveContextReader{ctx: ctx, reader: reader}, filepath.Join(temp, entry.ID+".zip"))
		_ = reader.Close()
		if err != nil {
			return nil, err
		}
		child, err := InspectArchive(filepath.Join(temp, entry.ID+".zip"))
		if err != nil {
			return nil, fmt.Errorf("组件 %s: %w", entry.ID, err)
		}
		if child.Kind == "package" || child.Kind == "runtime" {
			return nil, fmt.Errorf("组件 %s: Runtime 请独立安装，组件集合只支持一层", entry.ID)
		}
		records = append(records, Record{Package: child, ID: id})
	}
	var resources []string
	options.Components = nil
	// 安装期间只更新绑定，整包完成后的 Robot 生效由调用方统一处理，
	// 避免装一个组件就重启一次 Robot，或提前启动尚未完整配置的模型。
	options.ApplyNow = false
	for i, record := range records {
		if err := ctx.Err(); err != nil {
			return resources, err
		}
		entry := entries[i]
		report := func(message string) { progress(fmt.Sprintf("[%d/%d] %s：%s", i+1, len(entries), entry.ID, message)) }
		report(fmt.Sprintf("准备安装 %s %s", record.Name, record.Version))
		result, err := installer(ctx, projectID, record, filepath.Join(temp, entry.ID+".zip"), options, report)
		resources = append(resources, result...)
		if err != nil {
			return resources, fmt.Errorf("组件 %s 安装失败（之前已完成的组件保留）: %w", entry.ID, err)
		}
		report("已完成")
	}
	return resources, nil
}
