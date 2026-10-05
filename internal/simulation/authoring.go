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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Transform 使用米和 xyzw 四元数。
type Transform struct {
	Position       [3]float64 `json:"position"`
	QuaternionXYZW [4]float64 `json:"quaternion_xyzw"`
	Scale          [3]float64 `json:"scale"`
}

// SceneAsset 引用资产仓中的可分发资源，不接受任意宿主绝对路径。
type SceneAsset struct {
	ID       string         `json:"id"`
	AssetKey string         `json:"asset_key"`
	Kind     string         `json:"kind"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// SceneNode 是场景树中的对象、Robot、传感器、相机、灯光或分组。
type SceneNode struct {
	ID         string         `json:"id"`
	ParentID   string         `json:"parent_id,omitempty"`
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	AssetID    string         `json:"asset_id,omitempty"`
	Transform  Transform      `json:"transform"`
	Properties map[string]any `json:"properties,omitempty"`
	// Extensions 保存 Runtime 专有设置。公共字段不能出现 body、geom 等引擎
	// 标识；具体 Runtime 必须校验自己命名空间中的每个设置，不能静默忽略。
	Extensions map[string]any `json:"extensions,omitempty"`
}

// ProjectLayoutAuthoring 记录 Project Layout 的只读公共来源和编辑边界。
// 这些字段由 Framework 创建草稿时写入，Studio 不能通过整文档更新覆盖。
type ProjectLayoutAuthoring struct {
	Mode             string `json:"mode"`
	ProjectSceneID   string `json:"project_scene_id"`
	CatalogSceneID   string `json:"catalog_scene_id"`
	SceneVersion     string `json:"scene_version"`
	SourceVariantID  string `json:"source_variant_id,omitempty"`
	Initialization   string `json:"initialization"`
	RuntimeProfileID string `json:"runtime_profile_id"`
	TemplateRef      string `json:"template_ref"`
	// SourceAssetCatalogVersion 是公共模板发布时使用的目录版本，仅用于追溯。
	// AssetCatalogVersion 是本草稿实际完成资源解析时使用的目录版本。
	SourceAssetCatalogVersion string   `json:"source_asset_catalog_version,omitempty"`
	AssetCatalogVersion       string   `json:"asset_catalog_version"`
	AllowedAssetTags          []string `json:"allowed_asset_tags,omitempty"`
	LockedNodes               []string `json:"locked_nodes,omitempty"`
}

// LayoutPreviewArtifact 是 Layout 每个 revision 对应的确定性预览证据。
// Path 是 Project 工作区内的相对路径，公共响应不暴露宿主绝对路径。
type LayoutPreviewArtifact struct {
	LayoutID       string `json:"layout_id"`
	Revision       int64  `json:"revision"`
	RuntimeProfile string `json:"runtime_profile"`
	Generator      string `json:"generator"`
	Path           string `json:"path"`
	MediaType      string `json:"media_type"`
}

// SceneDocument 是 Three.js 编辑器和构建器共同使用的引擎无关草稿。
type SceneDocument struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	// SceneID 把多个独立构建的 Layout 归入同一个逻辑场景。Layout 仍分别
	// 构建 RuntimeBundle，运行时切换采用 stop + reload，不热改 MjModel。
	SceneID         string                     `json:"scene_id"`
	LayoutID        string                     `json:"layout_id"`
	LayoutName      string                     `json:"layout_name"`
	Name            string                     `json:"name"`
	Description     string                     `json:"description,omitempty"`
	Category        string                     `json:"category,omitempty"`
	Tags            []string                   `json:"tags,omitempty"`
	Preview         string                     `json:"preview,omitempty"`
	PreviewArtifact *LayoutPreviewArtifact     `json:"preview_artifact,omitempty"`
	SceneKind       string                     `json:"scene_kind"`
	Revision        int64                      `json:"revision"`
	Status          string                     `json:"status"`
	Version         int64                      `json:"version"`
	Assets          []SceneAsset               `json:"assets"`
	Nodes           []SceneNode                `json:"nodes"`
	Regions         []SceneNode                `json:"regions"`
	Physics         map[string]any             `json:"physics"`
	Extensions      map[string]json.RawMessage `json:"extensions,omitempty"`
	Authoring       *ProjectLayoutAuthoring    `json:"authoring,omitempty"`
	CreatedAt       time.Time                  `json:"created_at"`
	UpdatedAt       time.Time                  `json:"updated_at"`
}

// ValidationIssue 直接指出编辑器应聚焦的节点和字段。
type ValidationIssue struct {
	Level   string `json:"level"`
	NodeID  string `json:"node_id,omitempty"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// ValidationResult 是校验和构建共同返回的结果。
type ValidationResult struct {
	Valid  bool              `json:"valid"`
	Issues []ValidationIssue `json:"issues"`
}

// SceneAuthoringService 管理草稿、校验、构建和发布。
// 正在运行的 MjModel 不会读取草稿文件，只有显式构建后才能启动新版本。
type SceneAuthoringService struct {
	store   *FileStore
	catalog SceneAssetCatalog
}

func NewSceneAuthoringService(store *FileStore) *SceneAuthoringService {
	service, err := NewSceneAuthoringServiceWithCatalog(
		store, DefaultSceneAssetCatalog(),
	)
	if err != nil {
		panic("内建开发 Asset Catalog 无效: " + err.Error())
	}
	return service
}

func (s *SceneAuthoringService) Create(
	projectID, name string,
) (SceneDocument, error) {
	name = strings.TrimSpace(name)
	if projectID == "" || name == "" {
		return SceneDocument{}, errors.New("project_id 和 name 不能为空")
	}
	now := time.Now().UTC()
	document := SceneDocument{
		ID: "scene-doc-" + uuid.NewString(), ProjectID: projectID, Name: name,
		Revision: 1, Status: "draft", Version: 0, SceneKind: "scene_document",
		LayoutID: "layout-" + uuid.NewString(), LayoutName: "Default",
		Assets: []SceneAsset{}, Nodes: []SceneNode{}, Regions: []SceneNode{},
		Physics: map[string]any{
			"gravity_m_s2":     []float64{0, 0, -9.81},
			"timestep_seconds": 0.002,
		},
		CreatedAt: now, UpdatedAt: now,
	}
	document.SceneID = "scene-" + uuid.NewString()
	refreshLayoutPreview(&document)
	if err := s.save(&document); err != nil {
		return SceneDocument{}, err
	}
	return document, nil
}

// CreateProjectLayout 从公共场景随版本发布的只读模板创建 Project 草稿。
// source 已由 SceneCatalogService 严格加载；目录版本仅用于追溯，兼容性由模板
// 实际引用的资产 ID、完整资产描述、素材标签与锁定节点共同判定。
func (s *SceneAuthoringService) CreateProjectLayout(
	projectID, projectSceneID, name, initialization, sourceVariantID string,
	entry SceneCatalogEntry, version SceneCatalogVersion, source SceneDocument,
) (SceneDocument, error) {
	name = strings.TrimSpace(name)
	if projectID == "" || projectSceneID == "" || name == "" {
		return SceneDocument{}, errors.New("project_id、project_scene_id 和 name 不能为空")
	}
	if version.Authoring.Mode != "layout_only" {
		return SceneDocument{}, fmt.Errorf("%w: 当前公共场景版本不支持 Layout 编辑", ErrConflict)
	}
	if initialization != "copy_variant" && initialization != "empty_layout" {
		return SceneDocument{}, errors.New("initialization 只允许 copy_variant 或 empty_layout")
	}
	if initialization == "copy_variant" && strings.TrimSpace(sourceVariantID) == "" {
		return SceneDocument{}, errors.New("复制官方 Layout 时 source_variant_id 不能为空")
	}
	document, err := cloneSceneDocument(source)
	if err != nil {
		return SceneDocument{}, err
	}
	now := time.Now().UTC()
	document.ID = "scene-doc-" + uuid.NewString()
	document.ProjectID = projectID
	document.SceneID = "project-layouts-" + projectSceneID
	document.LayoutID = "layout-" + uuid.NewString()
	document.LayoutName = name
	document.Name = entry.Name + " · " + name
	document.Description = entry.Description
	document.Category = "project-layout"
	document.Tags = append([]string{}, entry.Tags...)
	document.SceneKind = "scene_document"
	document.Revision = 1
	document.Status = "draft"
	document.Version = 0
	document.CreatedAt, document.UpdatedAt = now, now
	document.Authoring = &ProjectLayoutAuthoring{
		Mode: "layout_only", ProjectSceneID: projectSceneID,
		CatalogSceneID: entry.SceneID, SceneVersion: version.Version,
		SourceVariantID: sourceVariantID, Initialization: initialization,
		RuntimeProfileID:          entry.CompatibleRuntimeProfile,
		TemplateRef:               version.Authoring.TemplateRef,
		SourceAssetCatalogVersion: version.Authoring.AssetCatalogVersion,
		AssetCatalogVersion:       s.catalog.CatalogVersion,
		AllowedAssetTags:          append([]string{}, version.Authoring.AllowedAssetTags...),
		LockedNodes:               append([]string{}, version.Authoring.LockedNodes...),
	}
	if err := validateLockedNodesPresent(document); err != nil {
		return SceneDocument{}, err
	}
	validation := s.Validate(document)
	if !validation.Valid {
		return SceneDocument{}, fmt.Errorf("%w: 公共 Layout 模板无效: %s",
			ErrConflict, formatIssues(validation.Issues))
	}
	refreshLayoutPreview(&document)
	if err := s.save(&document); err != nil {
		return SceneDocument{}, err
	}
	return document, nil
}

func (s *SceneAuthoringService) List(projectID string) ([]SceneDocument, error) {
	root, err := s.documentsRoot(projectID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	result := make([]SceneDocument, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		document, loadErr := s.Get(projectID, strings.TrimSuffix(entry.Name(), ".json"))
		if loadErr == nil {
			result = append(result, document)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].UpdatedAt.After(result[j].UpdatedAt)
	})
	return result, nil
}

func (s *SceneAuthoringService) Get(projectID, documentID string) (SceneDocument, error) {
	path, err := s.documentPath(projectID, documentID)
	if err != nil {
		return SceneDocument{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return SceneDocument{}, ErrNotFound
	}
	if err != nil {
		return SceneDocument{}, err
	}
	var document SceneDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return SceneDocument{}, fmt.Errorf("解析场景草稿失败: %w", err)
	}
	if document.ProjectID != projectID || document.ID != documentID {
		return SceneDocument{}, ErrNotFound
	}
	normalizeSceneIdentity(&document)
	return document, nil
}

func (s *SceneAuthoringService) Update(
	projectID, documentID string,
	expectedRevision int64,
	next SceneDocument,
) (SceneDocument, error) {
	current, err := s.Get(projectID, documentID)
	if err != nil {
		return SceneDocument{}, err
	}
	if current.Revision != expectedRevision {
		return SceneDocument{}, fmt.Errorf("%w: 场景草稿 revision 已变化", ErrConflict)
	}
	if current.Status == "published" {
		return SceneDocument{}, fmt.Errorf("%w: 已发布版本不能直接覆盖", ErrConflict)
	}
	next.ID = current.ID
	next.ProjectID = current.ProjectID
	next.SceneID = current.SceneID
	next.LayoutID = current.LayoutID
	next.LayoutName = current.LayoutName
	next.Authoring = current.Authoring
	next.Status = "draft"
	next.Version = current.Version
	if next.SceneKind == "" {
		next.SceneKind = current.SceneKind
	}
	if err := validateLockedNodeChanges(current, next); err != nil {
		return SceneDocument{}, err
	}
	// revision 在校验后才递增；失败的整文档 PUT 与操作接口一样不会保存半成品。
	validation := s.Validate(next)
	if !validation.Valid {
		return SceneDocument{}, fmt.Errorf("%w: 场景文档无效: %s",
			ErrConflict, formatIssues(validation.Issues))
	}
	next.Revision = current.Revision + 1
	next.CreatedAt = current.CreatedAt
	next.UpdatedAt = time.Now().UTC()
	refreshLayoutPreview(&next)
	if err := s.save(&next); err != nil {
		return SceneDocument{}, err
	}
	return next, nil
}

func (s *SceneAuthoringService) Validate(document SceneDocument) ValidationResult {
	normalizeSceneIdentity(&document)
	issues := make([]ValidationIssue, 0)
	if strings.TrimSpace(document.SceneID) == "" || strings.TrimSpace(document.LayoutID) == "" {
		issues = append(issues, ValidationIssue{Level: "error", Field: "scene_id",
			Message: "scene_id 和 layout_id 不能为空"})
	}
	if strings.TrimSpace(document.LayoutName) == "" {
		issues = append(issues, ValidationIssue{Level: "error", Field: "layout_name",
			Message: "Layout 名称不能为空"})
	}
	if strings.TrimSpace(document.Name) == "" {
		issues = append(issues, ValidationIssue{Level: "error", Field: "name", Message: "场景名称不能为空"})
	}
	assets := make(map[string]struct{}, len(document.Assets))
	for _, asset := range document.Assets {
		if asset.ID == "" || asset.AssetKey == "" {
			issues = append(issues, ValidationIssue{Level: "error", Field: "assets", Message: "资产必须包含 id 和 asset_key"})
			continue
		}
		if filepath.IsAbs(asset.AssetKey) || strings.Contains(asset.AssetKey, "..") {
			issues = append(issues, ValidationIssue{Level: "error", Field: "asset_key", Message: "asset_key 必须是资产仓内的相对标识"})
		}
		if _, exists := assets[asset.ID]; exists {
			issues = append(issues, ValidationIssue{Level: "error", Field: "assets", Message: "资产 id 重复: " + asset.ID})
		}
		assets[asset.ID] = struct{}{}
	}
	nodes := make(map[string]struct{}, len(document.Nodes)+len(document.Regions))
	all := append(append([]SceneNode{}, document.Nodes...), document.Regions...)
	for _, node := range all {
		if node.ID == "" || node.Name == "" || node.Kind == "" {
			issues = append(issues, ValidationIssue{Level: "error", NodeID: node.ID, Message: "节点必须包含 id、name 和 kind"})
			continue
		}
		if _, exists := nodes[node.ID]; exists {
			issues = append(issues, ValidationIssue{Level: "error", NodeID: node.ID, Message: "节点 id 重复"})
		}
		nodes[node.ID] = struct{}{}
		if node.AssetID != "" {
			if _, ok := assets[node.AssetID]; !ok {
				issues = append(issues, ValidationIssue{Level: "error", NodeID: node.ID, Field: "asset_id", Message: "引用的资产不存在"})
			}
		}
		if zeroQuaternion(node.Transform.QuaternionXYZW) {
			issues = append(issues, ValidationIssue{Level: "error", NodeID: node.ID, Field: "quaternion_xyzw", Message: "四元数不能为零"})
		}
		if nonPositiveScale(node.Transform.Scale) {
			issues = append(issues, ValidationIssue{Level: "error", NodeID: node.ID, Field: "scale", Message: "缩放必须大于零"})
		}
	}
	for _, node := range all {
		if node.ParentID != "" {
			if _, ok := nodes[node.ParentID]; !ok {
				issues = append(issues, ValidationIssue{Level: "error", NodeID: node.ID, Field: "parent_id", Message: "父节点不存在"})
			}
		}
	}
	issues = append(issues, validateAssetKinds(document)...)
	issues = append(issues, s.validatePublishedAssets(document)...)
	issues = append(issues, validateScenePhysics(document)...)
	issues = append(issues, validateSceneGraph(document)...)
	return ValidationResult{Valid: len(issues) == 0, Issues: issues}
}

func (s *SceneAuthoringService) Publish(
	projectID, documentID string,
	expectedRevision int64,
) (SceneDocument, error) {
	document, err := s.Get(projectID, documentID)
	if err != nil {
		return SceneDocument{}, err
	}
	if document.Revision != expectedRevision {
		return SceneDocument{}, fmt.Errorf("%w: 场景草稿 revision 已变化", ErrConflict)
	}
	bundle, err := s.store.FindRuntimeBundle(projectID, documentID, expectedRevision)
	if err != nil {
		return SceneDocument{}, fmt.Errorf("%w: 发布前必须完成 RuntimeBundle 构建", ErrConflict)
	}
	if bundle.SceneVersion != document.Version+1 {
		return SceneDocument{}, fmt.Errorf("%w: RuntimeBundle 场景版本不是当前下一版本", ErrConflict)
	}
	document.Status = "published"
	document.Version = bundle.SceneVersion
	document.Revision++
	document.UpdatedAt = time.Now().UTC()
	refreshLayoutPreview(&document)
	if err := s.save(&document); err != nil {
		return SceneDocument{}, err
	}
	return document, nil
}

func (s *SceneAuthoringService) ForkPublished(
	projectID, documentID, name string,
) (SceneDocument, error) {
	current, err := s.Get(projectID, documentID)
	if err != nil {
		return SceneDocument{}, err
	}
	if current.Status != "published" {
		return SceneDocument{}, fmt.Errorf("%w: 只有已发布场景需要创建新草稿", ErrConflict)
	}
	next := current
	next.ID = "scene-doc-" + uuid.NewString()
	next.Name = strings.TrimSpace(name)
	if next.Name == "" {
		next.Name = current.Name + " 副本"
	}
	next.Status = "draft"
	next.Revision = 1
	now := time.Now().UTC()
	next.CreatedAt, next.UpdatedAt = now, now
	refreshLayoutPreview(&next)
	if err := s.save(&next); err != nil {
		return SceneDocument{}, err
	}
	return next, nil
}

func validateLockedNodesPresent(document SceneDocument) error {
	if document.Authoring == nil {
		return nil
	}
	for _, nodeID := range document.Authoring.LockedNodes {
		if _, _, found := findSceneNode(&document, nodeID); !found {
			return fmt.Errorf("%w: Layout 模板缺少锁定节点 %s", ErrConflict, nodeID)
		}
	}
	return nil
}

func validateLockedNodeChanges(current, next SceneDocument) error {
	if current.Authoring == nil {
		return nil
	}
	for _, nodeID := range current.Authoring.LockedNodes {
		before, _, beforeFound := findSceneNode(&current, nodeID)
		after, _, afterFound := findSceneNode(&next, nodeID)
		if !beforeFound || !afterFound {
			return fmt.Errorf("%w: 锁定节点 %s 不能删除", ErrConflict, nodeID)
		}
		if before.Kind != after.Kind || before.AssetID != after.AssetID {
			return fmt.Errorf("%w: 锁定节点 %s 不能替换类型或资产", ErrConflict, nodeID)
		}
	}
	return nil
}

func (s *SceneAuthoringService) save(document *SceneDocument) error {
	if document == nil {
		return errors.New("场景文档不能为空")
	}
	normalizeSceneIdentity(document)
	if err := s.persistLayoutPreview(document); err != nil {
		return err
	}
	path, err := s.documentPath(document.ProjectID, document.ID)
	if err != nil {
		return err
	}
	return writeJSONAtomic(path, document)
}

func (s *SceneAuthoringService) documentsRoot(projectID string) (string, error) {
	root, err := s.store.simulationRoot(projectID)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, "scene-documents")
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	return path, nil
}

func (s *SceneAuthoringService) documentPath(projectID, documentID string) (string, error) {
	if !strings.HasPrefix(documentID, "scene-doc-") || strings.ContainsAny(documentID, "/\\") {
		return "", ErrNotFound
	}
	root, err := s.documentsRoot(projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, documentID+".json"), nil
}

func zeroQuaternion(value [4]float64) bool {
	return value == [4]float64{}
}

func nonPositiveScale(value [3]float64) bool {
	return value[0] <= 0 || value[1] <= 0 || value[2] <= 0
}
