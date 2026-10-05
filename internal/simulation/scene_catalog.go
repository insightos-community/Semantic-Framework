// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package simulation

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// SceneAuthoringDescriptor 只描述公共场景是否允许派生 Project Layout。
// 公共版本本身始终只读；layout_only 也只能从已登记模板创建草稿。
type SceneAuthoringDescriptor struct {
	Mode                string         `yaml:"mode" json:"mode"`
	TemplateRef         string         `yaml:"template_ref,omitempty" json:"template_ref,omitempty"`
	AssetCatalogVersion string         `yaml:"asset_catalog_version,omitempty" json:"asset_catalog_version,omitempty"`
	AllowedAssetTags    []string       `yaml:"allowed_asset_tags,omitempty" json:"allowed_asset_tags,omitempty"`
	LockedNodes         []string       `yaml:"locked_nodes,omitempty" json:"locked_nodes,omitempty"`
	PreviewCamera       map[string]any `yaml:"preview_camera,omitempty" json:"preview_camera,omitempty"`
}

// EvaluationDescriptor 属于具体场景版本。Runtime 支持 evaluator 只是必要条件，
// 普通 native 场景没有本字段时，Framework 与 Studio 都不能显示评测入口。
type EvaluationDescriptor struct {
	Provider           string   `yaml:"provider" json:"provider"`
	EvaluationKind     string   `yaml:"evaluation_kind" json:"evaluation_kind"`
	Metrics            []string `yaml:"metrics" json:"metrics"`
	SupportsComparison bool     `yaml:"supports_comparison" json:"supports_comparison"`
	AvailableVariants  []string `yaml:"available_variants" json:"available_variants,omitempty"`
}

type SceneCatalogVariant struct {
	VariantID       string         `yaml:"variant_id" json:"variant_id"`
	Name            string         `yaml:"name" json:"name"`
	Kind            string         `yaml:"kind" json:"kind"`
	Description     string         `yaml:"description" json:"description,omitempty"`
	Preview         string         `yaml:"preview,omitempty" json:"preview,omitempty"`
	AuthoringRef    string         `yaml:"authoring_ref,omitempty" json:"authoring_ref,omitempty"`
	Parameters      map[string]any `yaml:"parameters" json:"parameters,omitempty"`
	RuntimeSceneKey string         `yaml:"runtime_scene_key,omitempty" json:"runtime_scene_key,omitempty"`
	RuntimeBundleID string         `yaml:"runtime_bundle_id,omitempty" json:"runtime_bundle_id,omitempty"`
	RuntimeLayout   string         `yaml:"runtime_layout,omitempty" json:"runtime_layout,omitempty"`
}

type SceneCatalogVersion struct {
	Version         string                   `yaml:"version" json:"version"`
	RuntimeSceneKey string                   `yaml:"runtime_scene_key" json:"runtime_scene_key"`
	Published       bool                     `yaml:"published" json:"published"`
	RobotModels     []string                 `yaml:"robot_models" json:"robot_models"`
	Variants        []SceneCatalogVariant    `yaml:"variants" json:"variants"`
	Capabilities    []string                 `yaml:"capabilities" json:"capabilities"`
	Evaluation      *EvaluationDescriptor    `yaml:"evaluation,omitempty" json:"evaluation,omitempty"`
	Authoring       SceneAuthoringDescriptor `yaml:"authoring" json:"authoring"`
}

// SceneCatalogEntry 是跨 Project 的只读场景目录项。目录由安装包、资产仓、导入包
// 或 Project 发布动作更新；浏览目录不要求 Runtime 在线。
type SceneCatalogEntry struct {
	PreviewPreparation *ScenePreviewStatus `yaml:"-" json:"preview_preparation,omitempty"`
	// ContentRoot 是场景包内的数据目录；加载目录时解析为绝对路径，仅传给 Runtime。
	// Framework 不解释其中的 BDDL、初态或引擎资产。
	ContentRoot              string                `yaml:"content_root,omitempty" json:"-"`
	SceneID                  string                `yaml:"scene_id" json:"scene_id"`
	Name                     string                `yaml:"name" json:"name"`
	Description              string                `yaml:"description" json:"description,omitempty"`
	Tags                     []string              `yaml:"tags" json:"tags"`
	Engine                   string                `yaml:"engine" json:"engine"`
	Loader                   string                `yaml:"loader" json:"loader"`
	Source                   string                `yaml:"source" json:"source"`
	CompatibleRuntimeProfile string                `yaml:"compatible_runtime_profile" json:"compatible_runtime_profile"`
	Preview                  string                `yaml:"preview" json:"preview,omitempty"`
	Versions                 []SceneCatalogVersion `yaml:"versions" json:"versions"`
}

type sceneCatalogFile struct {
	SchemaVersion  int                 `yaml:"schema_version"`
	CatalogVersion string              `yaml:"catalog_version"`
	Entries        []SceneCatalogEntry `yaml:"entries"`
}

// SceneCatalogService 在内存中维护启动期严格加载的只读索引。
type SceneCatalogService struct {
	directory          string
	mu                 sync.RWMutex
	version            string
	items              map[string]SceneCatalogEntry
	order              []string
	authoringDocuments map[string]SceneDocument
}

func LoadSceneCatalog(dir string) (*SceneCatalogService, error) {
	service := &SceneCatalogService{directory: dir, items: map[string]SceneCatalogEntry{}, authoringDocuments: map[string]SceneDocument{}}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && path != dir && strings.HasPrefix(entry.Name(), ".") {
			return filepath.SkipDir
		}
		if entry.IsDir() || (filepath.Ext(entry.Name()) != ".yaml" && filepath.Ext(entry.Name()) != ".yml") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("读取场景目录 %s 失败: %w", path, readErr)
		}
		var file sceneCatalogFile
		decoder := yaml.NewDecoder(strings.NewReader(string(data)))
		decoder.KnownFields(true)
		if decodeErr := decoder.Decode(&file); decodeErr != nil {
			return fmt.Errorf("解析场景目录 %s 失败: %w", path, decodeErr)
		}
		if file.SchemaVersion != 1 || strings.TrimSpace(file.CatalogVersion) == "" {
			return fmt.Errorf("场景目录 %s 缺少 schema_version=1 或 catalog_version", path)
		}
		if service.version == "" || file.CatalogVersion > service.version {
			service.version = file.CatalogVersion
		}
		for _, item := range file.Entries {
			if err := resolveSceneContent(filepath.Dir(path), &item); err != nil {
				return err
			}
			if validationErr := validateSceneCatalogEntry(item); validationErr != nil {
				return fmt.Errorf("场景目录 %s 无效: %w", path, validationErr)
			}
			if _, exists := service.items[item.SceneID]; exists {
				return fmt.Errorf("场景目录 scene_id 重复: %s", item.SceneID)
			}
			if err := loadSceneAuthoringDocuments(service, item, func(ref string) ([]byte, error) {
				return os.ReadFile(filepath.Join(filepath.Dir(path), filepath.FromSlash(ref)))
			}); err != nil {
				return fmt.Errorf("场景目录 %s 的编辑模板无效: %w", path, err)
			}
			service.items[item.SceneID] = item
			service.order = append(service.order, item.SceneID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(service.order)
	if len(service.order) == 0 {
		// 干净安装在首个 Runtime Pack 安装前允许为空。Studio 会明确显示
		// “尚未安装 Runtime”，而不是让 Server 启动失败。
		service.version = "empty"
	}
	return service, nil
}

func resolveSceneContent(root string, item *SceneCatalogEntry) error {
	if item.ContentRoot == "" {
		return nil
	}
	if err := validateRuntimePackRelativePath(item.ContentRoot); err != nil {
		return err
	}
	base, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	// root 自身可能含符号链接（macOS 的 /var、被链接的安装根）。先规范化 base，
	// 否则与 EvalSymlinks 后的 resolved 比较会在 Rel 处误判为越界。
	if evaluated, evalErr := filepath.EvalSymlinks(base); evalErr == nil {
		base = evaluated
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(base, item.ContentRoot))
	if err != nil {
		return fmt.Errorf("场景内容未安装: %w", err)
	}
	rel, err := filepath.Rel(base, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("场景内容必须位于目录包内")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return errors.New("场景内容必须是目录")
	}
	item.ContentRoot = resolved
	return nil
}

func (s *SceneCatalogService) contentRoot(profileID, sceneKey string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	root := ""
	for _, item := range s.items {
		if item.CompatibleRuntimeProfile != profileID {
			continue
		}
		for _, version := range item.Versions {
			if version.RuntimeSceneKey != sceneKey {
				continue
			}
			if root != "" && item.ContentRoot != "" && root != item.ContentRoot {
				return "", errors.New("场景内容来源重复，请保留一个已安装版本")
			}
			if item.ContentRoot != "" {
				root = item.ContentRoot
			}
		}
	}
	return root, nil
}

// LoadSceneCatalogFS 从编译期内置模板读取离线场景目录。它只在未运行
// semantic init 的开发/测试进程中作为只读默认值使用；生产目录仍从磁盘加载。
func LoadSceneCatalogFS(fsys fs.FS, dir string) (*SceneCatalogService, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("读取内置场景目录失败: %w", err)
	}
	service := &SceneCatalogService{items: map[string]SceneCatalogEntry{}, authoringDocuments: map[string]SceneDocument{}}
	for _, entry := range entries {
		if entry.IsDir() || (filepath.Ext(entry.Name()) != ".yaml" &&
			filepath.Ext(entry.Name()) != ".yml") {
			continue
		}
		path := filepath.ToSlash(filepath.Join(dir, entry.Name()))
		data, readErr := fs.ReadFile(fsys, path)
		if readErr != nil {
			return nil, fmt.Errorf("读取内置场景目录 %s 失败: %w", path, readErr)
		}
		if err := mergeSceneCatalogFile(service, data, path, func(ref string) ([]byte, error) {
			return fs.ReadFile(fsys, filepath.ToSlash(filepath.Join(filepath.Dir(path), ref)))
		}); err != nil {
			return nil, err
		}
	}
	return finishSceneCatalog(service)
}

func mergeSceneCatalogFile(service *SceneCatalogService, data []byte, path string, readRef func(string) ([]byte, error)) error {
	var file sceneCatalogFile
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		return fmt.Errorf("解析场景目录 %s 失败: %w", path, err)
	}
	if file.SchemaVersion != 1 || strings.TrimSpace(file.CatalogVersion) == "" {
		return fmt.Errorf("场景目录 %s 缺少 schema_version=1 或 catalog_version", path)
	}
	if service.version == "" || file.CatalogVersion > service.version {
		service.version = file.CatalogVersion
	}
	for _, item := range file.Entries {
		if err := validateSceneCatalogEntry(item); err != nil {
			return fmt.Errorf("场景目录 %s 无效: %w", path, err)
		}
		if _, exists := service.items[item.SceneID]; exists {
			return fmt.Errorf("场景目录 scene_id 重复: %s", item.SceneID)
		}
		if err := loadSceneAuthoringDocuments(service, item, readRef); err != nil {
			return fmt.Errorf("场景目录 %s 的编辑模板无效: %w", path, err)
		}
		service.items[item.SceneID] = item
		service.order = append(service.order, item.SceneID)
	}
	return nil
}

func finishSceneCatalog(service *SceneCatalogService) (*SceneCatalogService, error) {
	sort.Strings(service.order)
	if len(service.order) == 0 {
		return nil, errors.New("场景目录中没有可用条目")
	}
	return service, nil
}

func validateSceneCatalogEntry(item SceneCatalogEntry) error {
	if item.SceneID == "" || item.Name == "" || item.Engine == "" || item.Loader == "" ||
		item.CompatibleRuntimeProfile == "" || len(item.Versions) == 0 {
		return fmt.Errorf("scene_id、name、engine、loader、compatible_runtime_profile 和 versions 不能为空")
	}
	versions := map[string]struct{}{}
	for _, version := range item.Versions {
		if version.Version == "" || version.RuntimeSceneKey == "" || !version.Published {
			return fmt.Errorf("场景 %s 只能登记带版本和 runtime_scene_key 的已发布版本", item.SceneID)
		}
		if _, exists := versions[version.Version]; exists {
			return fmt.Errorf("场景 %s 的版本 %s 重复", item.SceneID, version.Version)
		}
		versions[version.Version] = struct{}{}
		if err := validateSceneAuthoring(item.SceneID, version); err != nil {
			return err
		}
		variants := map[string]struct{}{}
		for _, variant := range version.Variants {
			if variant.VariantID == "" || variant.Name == "" || variant.Kind == "" {
				return fmt.Errorf("场景 %s 的 variant 字段不完整", item.SceneID)
			}
			if _, exists := variants[variant.VariantID]; exists {
				return fmt.Errorf("场景 %s 的 variant_id %s 重复", item.SceneID, variant.VariantID)
			}
			variants[variant.VariantID] = struct{}{}
		}
		if version.Evaluation != nil &&
			(version.Evaluation.Provider == "" || version.Evaluation.EvaluationKind == "") {
			return fmt.Errorf("场景 %s 的 EvaluationDescriptor 不完整", item.SceneID)
		}
	}
	return nil
}

func validateSceneAuthoring(sceneID string, version SceneCatalogVersion) error {
	mode := strings.TrimSpace(version.Authoring.Mode)
	if mode == "" || mode == "none" {
		if version.Authoring.TemplateRef != "" || len(version.Authoring.LockedNodes) > 0 {
			return fmt.Errorf("场景 %s 的只读版本不能声明编辑模板", sceneID)
		}
		return nil
	}
	if mode != "layout_only" {
		return fmt.Errorf("场景 %s 的 authoring.mode 只允许 none 或 layout_only", sceneID)
	}
	if version.Authoring.TemplateRef == "" || version.Authoring.AssetCatalogVersion == "" {
		return fmt.Errorf("场景 %s 的 layout_only 版本缺少 template_ref 或 asset_catalog_version", sceneID)
	}
	if err := validateAuthoringRef(version.Authoring.TemplateRef); err != nil {
		return fmt.Errorf("场景 %s 的 template_ref 无效: %w", sceneID, err)
	}
	for _, variant := range version.Variants {
		if variant.AuthoringRef == "" {
			return fmt.Errorf("场景 %s 的可编辑 variant %s 缺少 authoring_ref", sceneID, variant.VariantID)
		}
		if err := validateAuthoringRef(variant.AuthoringRef); err != nil {
			return fmt.Errorf("场景 %s 的 variant %s authoring_ref 无效: %w", sceneID, variant.VariantID, err)
		}
	}
	return nil
}

func validateAuthoringRef(ref string) error {
	clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(ref)))
	if clean == "." || clean == "" || strings.HasPrefix(clean, "../") ||
		strings.HasPrefix(clean, "/") || strings.Contains(clean, ":") {
		return errors.New("必须是场景目录内的规范相对路径")
	}
	return nil
}

func authoringDocumentKey(sceneID, versionID, variantID string) string {
	return sceneID + "\x00" + versionID + "\x00" + variantID
}

func loadSceneAuthoringDocuments(
	service *SceneCatalogService, item SceneCatalogEntry, readRef func(string) ([]byte, error),
) error {
	for _, version := range item.Versions {
		if version.Authoring.Mode != "layout_only" {
			continue
		}
		refs := map[string]string{"": version.Authoring.TemplateRef}
		for _, variant := range version.Variants {
			refs[variant.VariantID] = variant.AuthoringRef
		}
		for variantID, ref := range refs {
			data, err := readRef(ref)
			if err != nil {
				return fmt.Errorf("读取 %s 失败: %w", ref, err)
			}
			var document SceneDocument
			decoder := json.NewDecoder(strings.NewReader(string(data)))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&document); err != nil {
				return fmt.Errorf("解析 %s 失败: %w", ref, err)
			}
			service.authoringDocuments[authoringDocumentKey(item.SceneID, version.Version, variantID)] = document
		}
	}
	return nil
}

// AuthoringDocument 返回公共目录随版本发布的只读源。variantID 为空时返回
// 只保留模板基础结构的空 Layout；非空时返回对应官方 Layout。
func (s *SceneCatalogService) AuthoringDocument(
	sceneID, versionID, variantID string,
) (SceneDocument, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	document, ok := s.authoringDocuments[authoringDocumentKey(sceneID, versionID, variantID)]
	if !ok {
		return SceneDocument{}, fmt.Errorf("%w: 场景版本没有可编辑源", ErrNotFound)
	}
	return cloneSceneDocument(document)
}

func (s *SceneCatalogService) List(profileID string) []SceneCatalogEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]SceneCatalogEntry, 0, len(s.order))
	for _, id := range s.order {
		item := s.items[id]
		if profileID == "" || item.CompatibleRuntimeProfile == profileID {
			result = append(result, item)
		}
	}
	return result
}

func (s *SceneCatalogService) Get(sceneID string) (SceneCatalogEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, ok := s.items[sceneID]
	if !ok {
		return SceneCatalogEntry{}, fmt.Errorf("%w: 场景目录项不存在: %s", ErrNotFound, sceneID)
	}
	return item, nil
}

func (s *SceneCatalogService) Version(sceneID, versionID string) (SceneCatalogEntry, SceneCatalogVersion, error) {
	item, err := s.Get(sceneID)
	if err != nil {
		return SceneCatalogEntry{}, SceneCatalogVersion{}, err
	}
	for _, version := range item.Versions {
		if version.Version == versionID {
			return item, version, nil
		}
	}
	return SceneCatalogEntry{}, SceneCatalogVersion{},
		fmt.Errorf("%w: 场景 %s 不存在版本 %s", ErrNotFound, sceneID, versionID)
}

func (s *SceneCatalogService) CatalogVersion() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
}

// ReplaceSourceEntries 原子替换一个来源发布的目录项。Project 发布内容使用
// project:<project_id> 作为来源；替换不会覆盖安装包或其他 Project 的同名 ID。
func (s *SceneCatalogService) ReplaceSourceEntries(
	source string, entries []SceneCatalogEntry,
) error {
	if !strings.HasPrefix(source, "project:") || strings.TrimSpace(source[8:]) == "" {
		return errors.New("动态场景目录来源必须是 project:<project_id>")
	}
	for index := range entries {
		entries[index].Source = source
		if err := validateSceneCatalogEntry(entries[index]); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range entries {
		if existing, ok := s.items[entry.SceneID]; ok && existing.Source != source {
			return fmt.Errorf("%w: scene_id %s 已由 %s 使用", ErrConflict,
				entry.SceneID, existing.Source)
		}
	}
	for id, item := range s.items {
		if item.Source == source {
			delete(s.items, id)
		}
	}
	for _, entry := range entries {
		s.items[entry.SceneID] = entry
	}
	s.order = s.order[:0]
	for id := range s.items {
		s.order = append(s.order, id)
	}
	sort.Strings(s.order)
	return nil
}
