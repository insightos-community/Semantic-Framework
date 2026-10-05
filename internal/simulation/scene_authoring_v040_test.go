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
	"errors"
	"strings"
	"testing"
)

func authoringTestTransform(position [3]float64) Transform {
	return Transform{
		Position: position, QuaternionXYZW: [4]float64{0, 0, 0, 1},
		Scale: [3]float64{1, 1, 1},
	}
}

func TestSceneAuthoringCatalogOperationsPublishAndFork(t *testing.T) {
	store := NewFileStore(fixedWorkspace{root: t.TempDir()})
	authoring := NewSceneAuthoringService(store)
	catalog := authoring.AssetCatalog()
	if len(catalog) < 3 {
		t.Fatalf("资产目录不完整: %+v", catalog)
	}
	if !catalog[0].Preview.Placeholder {
		t.Fatal("R1 Pro 的 Three.js 边界盒必须明确标记为几何占位")
	}

	document, err := authoring.Create("project-authoring", "场景编辑闭环")
	if err != nil {
		t.Fatal(err)
	}
	robotAsset := catalog[0]
	boxAsset := catalog[1]
	operations := []SceneOperation{
		{
			Type: SceneOperationAddRobot,
			Node: &SceneNode{
				ID: "robot-1", Name: "R1 Pro", Kind: "robot",
				AssetID:    robotAsset.Asset.ID,
				Transform:  authoringTestTransform([3]float64{0, 0, 0.01}),
				Properties: robotAsset.DefaultProperties,
			},
		},
		{
			Type: SceneOperationAttachAsset, NodeID: "robot-1",
			AssetID: robotAsset.Asset.ID, Asset: &robotAsset.Asset,
		},
		{
			Type: SceneOperationCreateNode,
			Node: &SceneNode{
				ID: "box-1", Name: "Box", Kind: "object",
				AssetID:    boxAsset.Asset.ID,
				Transform:  authoringTestTransform([3]float64{0.5, 0, 0.25}),
				Properties: boxAsset.DefaultProperties,
			},
		},
		{
			Type: SceneOperationAttachAsset, NodeID: "box-1",
			AssetID: boxAsset.Asset.ID, Asset: &boxAsset.Asset,
		},
		{
			Type: SceneOperationAddCamera,
			Node: &SceneNode{
				ID: "camera-1", Name: "overview", Kind: "camera",
				Transform: authoringTestTransform([3]float64{2, -3, 2}),
				Properties: map[string]any{
					"sensor_kind": "rgb", "width": 640, "height": 480,
					"fps": 15, "fovy": 50,
				},
			},
		},
		{
			Type: SceneOperationCreateNode,
			Node: &SceneNode{
				ID: "light-1", Name: "key light", Kind: "light",
				Transform: authoringTestTransform([3]float64{2, -2, 4}),
				Properties: map[string]any{
					"direction":  []float64{0, 0, -1},
					"diffuse":    []float64{0.8, 0.8, 0.8},
					"specular":   []float64{0.2, 0.2, 0.2},
					"castshadow": true, "active": true,
				},
			},
		},
		{
			Type: SceneOperationAddRegion,
			Node: &SceneNode{
				ID: "target-1", Name: "Target", Kind: "region",
				Transform: authoringTestTransform([3]float64{1.5, 0, 0.025}),
				Properties: map[string]any{
					"model": "target", "category": "region",
					"size":   []float64{1.4, 1.4, 0.05},
					"static": true, "interactive": true,
					"material":  map[string]any{"rgba": []float64{0.25, 0.75, 0.55, 0.38}},
					"collision": map[string]any{"enabled": false},
				},
			},
		},
	}
	document, err = authoring.ApplyOperations(
		document.ProjectID, document.ID, document.Revision, operations,
	)
	if err != nil {
		t.Fatalf("场景操作闭环失败: %v", err)
	}
	if len(document.Assets) != 2 || len(document.Nodes) != 4 || len(document.Regions) != 1 {
		t.Fatalf("场景内容未完整保存: %+v", document)
	}
	if validation := authoring.Validate(document); !validation.Valid {
		t.Fatalf("完整场景未通过 Framework 校验: %+v", validation.Issues)
	}
	if err := store.SaveRuntimeBundle(document.ProjectID, RuntimeBundle{
		RuntimeBundleID: "runtime-bundle-authoring",
		DocumentID:      document.ID, Revision: document.Revision, SceneVersion: 1,
		SceneKey: document.ID, RuntimeProfileID: "native-mujoco",
		Document:   NewRuntimeSceneDocument(document),
		Validation: ValidationResult{Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	published, err := authoring.Publish(document.ProjectID, document.ID, document.Revision)
	if err != nil {
		t.Fatalf("发布场景失败: %v", err)
	}
	if published.Status != "published" || published.Version != 1 {
		t.Fatalf("发布结果不正确: %+v", published)
	}
	_, err = authoring.ApplyOperations(
		document.ProjectID, document.ID, published.Revision,
		[]SceneOperation{{Type: SceneOperationDeleteNode, NodeID: "box-1"}},
	)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("已发布版本不应允许场景操作覆盖: %v", err)
	}

	forked, err := authoring.ForkPublished(document.ProjectID, document.ID, "场景编辑闭环 v2")
	if err != nil {
		t.Fatalf("创建新版本草稿失败: %v", err)
	}
	if forked.Status != "draft" || forked.Revision != 1 || forked.Version != published.Version {
		t.Fatalf("新版本草稿没有继承发布内容: %+v", forked)
	}
	original, err := authoring.Get(document.ProjectID, document.ID)
	if err != nil || original.Status != "published" {
		t.Fatalf("创建新草稿覆盖了原发布版本: document=%+v err=%v", original, err)
	}
}

func TestSceneAuthoringRejectsUnsupportedStandaloneNodeWithLocation(t *testing.T) {
	store := NewFileStore(fixedWorkspace{root: t.TempDir()})
	authoring := NewSceneAuthoringService(store)
	document, err := authoring.Create("project-authoring", "非法独立节点")
	if err != nil {
		t.Fatal(err)
	}
	_, err = authoring.ApplyOperations(
		document.ProjectID, document.ID, document.Revision,
		[]SceneOperation{{
			Type: SceneOperationCreateNode,
			Node: &SceneNode{
				ID: "joint-1", Name: "Joint", Kind: "joint",
				Transform: authoringTestTransform([3]float64{}),
			},
		}},
	)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "独立 joint 节点") {
		t.Fatalf("独立 Joint 应被定位并拒绝: %v", err)
	}
}

func TestSceneAuthoringSeparatesDraftAndBuildValidation(t *testing.T) {
	store := NewFileStore(fixedWorkspace{root: t.TempDir()})
	authoring := NewSceneAuthoringService(store)
	document, err := authoring.Create("project-authoring", "分步编辑")
	if err != nil {
		t.Fatal(err)
	}
	document, err = authoring.ApplyOperations(
		document.ProjectID, document.ID, document.Revision,
		[]SceneOperation{{
			Type: SceneOperationAddRobot,
			Node: &SceneNode{
				ID: "robot-draft", Name: "尚未选择资产的 Robot",
				Transform: authoringTestTransform([3]float64{}),
			},
		}},
	)
	if err != nil {
		t.Fatalf("不完整 Robot 应能作为草稿保存: %v", err)
	}
	if validation := authoring.Validate(document); !validation.Valid {
		t.Fatalf("草稿结构校验不应要求立即挂资产: %+v", validation.Issues)
	}
	validation := authoring.ValidateForBuild(document)
	if validation.Valid || validation.Issues[0].NodeID != "robot-draft" {
		t.Fatalf("构建校验应定位缺少资产的 Robot: %+v", validation.Issues)
	}
}
