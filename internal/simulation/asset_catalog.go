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

// SceneAssetPreview 只用于 Scene Editor 的快速几何预览。
//
// Placeholder 为 true 表示 Three.js 显示的是边界盒，不是 Runtime 的真实 mesh；
// 构建后的真实外观必须在 MuJoCo Physics Viewer 中确认。
type SceneAssetPreview struct {
	Shape       string     `json:"shape"`
	Size        [3]float64 `json:"size"`
	Color       string     `json:"color"`
	Placeholder bool       `json:"placeholder"`
}

// VisualAssetContent 指向 Framework 内容服务提供的浏览器模型。
// ContentURL 必须是站内 API 地址，不能包含 Runtime 宿主路径。
type VisualAssetContent struct {
	ContentURL string `json:"content_url"`
	Format     string `json:"format"`
}

// VisualAssetDescriptor 让 Scene Editor 与 Semantic Map 解析同一视觉资产。
// Runtime 原生模型仍由 RuntimeBindings 关联，浏览器不会解析 MJCF 或 URDF。
type VisualAssetDescriptor struct {
	VisualID         string              `json:"visual_id"`
	Version          string              `json:"version"`
	EditorVisual     *VisualAssetContent `json:"editor_visual,omitempty"`
	MapVisual        *VisualAssetContent `json:"map_visual,omitempty"`
	FallbackGeometry SceneAssetPreview   `json:"fallback_geometry"`
	LocalBounds      [3]float64          `json:"local_bounds"`
	PlacementAnchor  string              `json:"placement_anchor"`
	RuntimeBindings  map[string]string   `json:"runtime_bindings,omitempty"`
}

// SceneAssetCatalogEntry 是允许进入 SceneDocument 的已发布逻辑资产。
//
// AssetKey 由产品维护的目录给出，浏览器不能提交宿主绝对路径。Plugin 会再次检查
// 该标识与实际 MJCF 文件、model 和 kind 一致。
type SceneAssetCatalogEntry struct {
	CatalogID          string                 `json:"catalog_id"`
	Label              string                 `json:"label"`
	NodeKind           string                 `json:"node_kind"`
	Tags               []string               `json:"tags,omitempty"`
	Asset              SceneAsset             `json:"asset"`
	Preview            SceneAssetPreview      `json:"preview"`
	Visual             *VisualAssetDescriptor `json:"visual,omitempty"`
	DefaultProperties  map[string]any         `json:"default_properties"`
	Source             string                 `json:"source"`
	License            string                 `json:"license"`
	DistributionStatus string                 `json:"distribution_status"`
}

// SceneAssetCatalog 是带版本的完整资产目录文件。
//
// SchemaVersion 表示 JSON 结构版本；CatalogVersion 表示资产选择结果的版本。
// 两者分开后，增加资产不会被误认为接口结构发生变化。
type SceneAssetCatalog struct {
	SchemaVersion  string                   `json:"schema_version"`
	CatalogVersion string                   `json:"catalog_version"`
	Entries        []SceneAssetCatalogEntry `json:"entries"`
}

// AssetCatalog 返回服务启动时保存目录的深拷贝。
func (s *SceneAuthoringService) AssetCatalog() []SceneAssetCatalogEntry {
	return s.Catalog().Entries
}

// DefaultSceneAssetCatalog 只用于测试和没有配置资产仓的本地开发。
// 产品部署应通过 SEMANTIC_SIMULATION_ASSET_CATALOG 指向受版本管理的 JSON。
func DefaultSceneAssetCatalog() SceneAssetCatalog {
	return SceneAssetCatalog{
		SchemaVersion: "1", CatalogVersion: "0.4.0-dev.0",
		Entries: []SceneAssetCatalogEntry{
			{
				CatalogID: "r1-pro-chassis", Label: "R1 Pro", NodeKind: "robot",
				Tags: []string{"native-mujoco", "r1pro"},
				Asset: SceneAsset{
					ID:       "robot-r1-pro-chassis-v1",
					AssetKey: "robot/r1_pro_chassis/config/r1_pro_chassis.xml",
					Kind:     "robot", Metadata: map[string]any{"model": "r1_pro_chassis"},
				},
				Preview: SceneAssetPreview{
					Shape: "box", Size: [3]float64{0.8, 0.55, 1.2},
					Color: "#315cec", Placeholder: true,
				},
				DefaultProperties: map[string]any{
					"model": "r1_pro_chassis", "sensor_names": []string{"camera"},
				},
				Source: "unknown", License: "pending", DistributionStatus: "internal-only",
			},
			{
				CatalogID: "box", Label: "Box", NodeKind: "object",
				Tags: []string{"native-mujoco", "depalletizing"},
				Asset: SceneAsset{
					ID: "object-box-v1", AssetKey: "assets/objects/box.xml",
					Kind: "object", Metadata: map[string]any{"model": "box", "category": "box"},
				},
				Preview: SceneAssetPreview{
					Shape: "box", Size: [3]float64{0.5, 0.5, 0.5},
					Color: "#d89b45", Placeholder: true,
				},
				DefaultProperties: map[string]any{
					"model": "box", "category": "box", "size": []float64{0.5, 0.5, 0.5},
					"mass": 0.5, "static": false, "interactive": true,
					"material": map[string]any{"rgba": []float64{0.85, 0.55, 0.2, 1}},
					"collision": map[string]any{
						"enabled": true, "friction": []float64{1, 0.005, 0.0001},
					},
				},
				Source: "unknown", License: "pending", DistributionStatus: "internal-only",
			},
			{
				CatalogID: "pallet", Label: "Pallet", NodeKind: "object",
				Tags: []string{"native-mujoco", "depalletizing"},
				Asset: SceneAsset{
					ID: "object-pallet-v1", AssetKey: "assets/objects/box.xml",
					Kind: "object", Metadata: map[string]any{"model": "box", "category": "pallet"},
				},
				Preview: SceneAssetPreview{
					Shape: "box", Size: [3]float64{1.2, 1, 0.15},
					Color: "#5f6f93", Placeholder: true,
				},
				DefaultProperties: map[string]any{
					"model": "box", "category": "pallet", "size": []float64{1.2, 1, 0.15},
					"static": true, "interactive": false,
					"material": map[string]any{"rgba": []float64{0.15, 0.22, 0.4, 1}},
					"collision": map[string]any{
						"enabled": true, "friction": []float64{1.2, 0.01, 0.0001},
					},
				},
				Source: "unknown", License: "pending", DistributionStatus: "internal-only",
			},
			{
				CatalogID: "target", Label: "Target", NodeKind: "object",
				Tags: []string{"native-mujoco", "depalletizing"},
				Asset: SceneAsset{
					ID: "object-target-v1", AssetKey: "assets/objects/target.xml",
					Kind: "object", Metadata: map[string]any{"model": "target", "category": "target"},
				},
				Preview: SceneAssetPreview{
					Shape: "box", Size: [3]float64{1.4, 1.4, 0.05},
					Color: "#40bf8c", Placeholder: true,
				},
				DefaultProperties: map[string]any{
					"model": "target", "category": "target", "size": []float64{1.4, 1.4, 0.05},
					"static": true, "interactive": true,
					"material":  map[string]any{"rgba": []float64{0.25, 0.75, 0.55, 0.38}},
					"collision": map[string]any{"enabled": false},
				},
				Source: "unknown", License: "pending", DistributionStatus: "internal-only",
			},
		},
	}
}
