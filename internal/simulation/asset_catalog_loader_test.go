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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSceneAssetCatalogLoadAndServiceIsolation(t *testing.T) {
	catalog := DefaultSceneAssetCatalog()
	catalog.CatalogVersion = "0.4.0-test.1"
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(t.TempDir(), "assets.json")
	if err := os.WriteFile(filePath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadSceneAssetCatalog(filePath)
	if err != nil {
		t.Fatalf("加载有效目录失败: %v", err)
	}
	service, err := NewSceneAuthoringServiceWithCatalog(
		NewFileStore(fixedWorkspace{root: t.TempDir()}), loaded,
	)
	if err != nil {
		t.Fatalf("注入有效目录失败: %v", err)
	}

	// 构造参数和返回值都是深拷贝，外部修改不能改变服务后续的发布资产校验。
	loaded.Entries[0].Asset.Metadata["model"] = "mutated-input"
	first := service.Catalog()
	if first.CatalogVersion != "0.4.0-test.1" ||
		first.Entries[0].Asset.Metadata["model"] != "r1_pro_chassis" {
		t.Fatalf("服务没有保存独立目录副本: %+v", first)
	}
	first.Entries[0].Asset.Metadata["model"] = "mutated-output"
	second := service.Catalog()
	if second.Entries[0].Asset.Metadata["model"] != "r1_pro_chassis" {
		t.Fatalf("调用方修改了服务内目录: %+v", second.Entries[0])
	}
}

func TestSceneAssetCatalogRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name string
		edit func(*SceneAssetCatalog)
		want string
	}{
		{
			name: "schema", want: "schema_version",
			edit: func(c *SceneAssetCatalog) { c.SchemaVersion = "2" },
		},
		{
			name: "catalog version", want: "catalog_version",
			edit: func(c *SceneAssetCatalog) { c.CatalogVersion = "latest" },
		},
		{
			name: "duplicate catalog id", want: "catalog_id 重复",
			edit: func(c *SceneAssetCatalog) {
				c.Entries[1].CatalogID = c.Entries[0].CatalogID
			},
		},
		{
			name: "duplicate asset id", want: "asset.id 重复",
			edit: func(c *SceneAssetCatalog) {
				c.Entries[1].Asset.ID = c.Entries[0].Asset.ID
			},
		},
		{
			name: "absolute key", want: "规范相对路径",
			edit: func(c *SceneAssetCatalog) { c.Entries[0].Asset.AssetKey = "/tmp/robot.xml" },
		},
		{
			name: "traversal key", want: "规范相对路径",
			edit: func(c *SceneAssetCatalog) { c.Entries[0].Asset.AssetKey = "../robot.xml" },
		},
		{
			name: "kind mismatch", want: "必须与 node_kind 一致",
			edit: func(c *SceneAssetCatalog) { c.Entries[0].Asset.Kind = "object" },
		},
		{
			name: "preview", want: "placeholder 必须为 true",
			edit: func(c *SceneAssetCatalog) { c.Entries[0].Preview.Placeholder = false },
		},
		{
			name: "source", want: "source 不能为空",
			edit: func(c *SceneAssetCatalog) { c.Entries[0].Source = "" },
		},
		{
			name: "license", want: "license 不能为空",
			edit: func(c *SceneAssetCatalog) { c.Entries[0].License = "" },
		},
		{
			name: "distribution", want: "distribution_status",
			edit: func(c *SceneAssetCatalog) { c.Entries[0].DistributionStatus = "public" },
		},
		{
			name: "unconfirmed redistributable", want: "不能标记为 redistributable",
			edit: func(c *SceneAssetCatalog) {
				c.Entries[0].License = "pending"
				c.Entries[0].DistributionStatus = "redistributable"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			catalog, err := cloneSceneAssetCatalog(DefaultSceneAssetCatalog())
			if err != nil {
				t.Fatal(err)
			}
			test.edit(&catalog)
			if err := catalog.Validate(); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("错误 = %v，期望包含 %q", err, test.want)
			}
		})
	}
}

func TestLoadSceneAssetCatalogUsesStrictJSON(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "assets.json")
	valid, err := json.Marshal(DefaultSceneAssetCatalog())
	if err != nil {
		t.Fatal(err)
	}
	withUnknown := strings.Replace(
		string(valid), `"schema_version":"1"`,
		`"schema_version":"1","unknown_field":true`, 1,
	)
	if err := os.WriteFile(filePath, []byte(withUnknown), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSceneAssetCatalog(filePath); err == nil ||
		!strings.Contains(err.Error(), "unknown_field") {
		t.Fatalf("未知字段没有被拒绝: %v", err)
	}

	if err := os.WriteFile(filePath, append(valid, []byte("{}")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSceneAssetCatalog(filePath); err == nil ||
		!strings.Contains(err.Error(), "只能包含一个 JSON 对象") {
		t.Fatalf("尾部 JSON 没有被拒绝: %v", err)
	}
}

// SEMANTIC_TEST_ASSET_CATALOG 供资产仓 Pipeline 复用同一套 Framework 校验。
func TestConfiguredRepositoryAssetCatalog(t *testing.T) {
	filePath := strings.TrimSpace(os.Getenv("SEMANTIC_TEST_ASSET_CATALOG"))
	if filePath == "" {
		t.Skip("未设置 SEMANTIC_TEST_ASSET_CATALOG")
	}
	catalog, err := LoadSceneAssetCatalog(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Entries) < 4 {
		t.Fatalf("资产仓目录条目不足: %+v", catalog.Entries)
	}
}

func TestProjectLayoutAcceptsCatalogVersionChangeWhenAssetsRemainCompatible(t *testing.T) {
	catalog := DefaultSceneAssetCatalog()
	catalog.CatalogVersion = "0.4.2"
	service, err := NewSceneAuthoringServiceWithCatalog(
		NewFileStore(fixedWorkspace{root: t.TempDir()}), catalog,
	)
	if err != nil {
		t.Fatal(err)
	}
	robot := catalog.Entries[0]
	source := SceneDocument{
		ID: "public-layout", ProjectID: "public", SceneID: "depalletizing-r1pro",
		LayoutID: "layout001", LayoutName: "布局 001", Name: "公共布局",
		SceneKind: "scene_document", Revision: 1, Status: "published", Version: 1,
		Assets: []SceneAsset{robot.Asset},
		Nodes: []SceneNode{{
			ID: "robot-r1-pro", Name: "R1 Pro", Kind: "robot", AssetID: robot.Asset.ID,
			Transform: Transform{
				QuaternionXYZW: [4]float64{0, 0, 0, 1},
				Scale:          [3]float64{1, 1, 1},
			},
			Properties: map[string]any{"model": "r1_pro_chassis"},
		}},
	}
	entry := SceneCatalogEntry{
		SceneID: "depalletizing-r1pro", Name: "R1 Pro 拆码垛",
		CompatibleRuntimeProfile: "native-mujoco",
	}
	version := SceneCatalogVersion{
		Version: "1.0.0",
		Authoring: SceneAuthoringDescriptor{
			Mode: "layout_only", TemplateRef: "authoring/scene-template.json",
			AssetCatalogVersion: "0.4.0", AllowedAssetTags: []string{"native-mujoco"},
			LockedNodes: []string{"robot-r1-pro"},
		},
	}

	document, err := service.CreateProjectLayout(
		"project-1", "project-scene-1", "兼容布局", "copy_variant", "layout001",
		entry, version, source,
	)
	if err != nil {
		t.Fatalf("目录版本变化但资产未变化时不应阻塞: %v", err)
	}
	if document.Authoring.SourceAssetCatalogVersion != "0.4.0" ||
		document.Authoring.AssetCatalogVersion != "0.4.2" {
		t.Fatalf("没有同时记录模板版本与实际解析版本: %+v", document.Authoring)
	}

	incompatible := source
	incompatible.Assets = append([]SceneAsset{}, source.Assets...)
	incompatible.Assets[0].AssetKey = "robot/removed-or-replaced.xml"
	_, err = service.CreateProjectLayout(
		"project-1", "project-scene-1", "不兼容布局", "copy_variant", "layout001",
		entry, version, incompatible,
	)
	if err == nil || !strings.Contains(err.Error(), "资产描述与发布目录不一致") {
		t.Fatalf("同一资产 ID 的定义变化必须拒绝: %v", err)
	}
}
