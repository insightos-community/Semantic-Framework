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

package handlers

import (
	"strings"
	"testing"

	"insightos.cn/semantic-framework/internal/simulation"
)

func TestProjectVisualCatalogUsesOneVersionedAssetForEditorAndMap(t *testing.T) {
	entries := simulation.DefaultSceneAssetCatalog().Entries
	decorated := projectVisualCatalog("project-visual", entries)
	if len(decorated) < 3 {
		t.Fatal("默认资产目录不完整")
	}
	for _, entry := range decorated {
		if entry.Visual == nil {
			t.Fatalf("资产 %s 没有视觉描述", entry.CatalogID)
		}
		visual := entry.Visual
		if visual.EditorVisual == nil || visual.MapVisual == nil ||
			visual.EditorVisual.ContentURL != visual.MapVisual.ContentURL {
			t.Fatalf("Editor 与 Map 未复用同一视觉内容: %+v", visual)
		}
		if !strings.Contains(visual.EditorVisual.ContentURL,
			"/projects/project-visual/simulation/visual-assets/") {
			t.Fatalf("视觉内容 URL 未限制在 Project: %s", visual.EditorVisual.ContentURL)
		}
		if visual.EditorVisual.Format != "glb" {
			t.Fatalf("视觉格式应为 glb: %+v", visual.EditorVisual)
		}
		if visual.Version != nativeVisualContentVersion {
			t.Fatalf("视觉内容版本未淘汰旧缓存: %+v", visual)
		}
	}
	var pallet, robot simulation.SceneAssetCatalogEntry
	for _, entry := range decorated {
		switch entry.CatalogID {
		case "pallet":
			pallet = entry
		case "r1-pro-chassis":
			robot = entry
		}
	}
	if pallet.Visual.VisualID != "pallet" {
		t.Fatalf("Pallet 不能错误复用 box visual_id: %+v", pallet.Visual)
	}
	if robot.Visual.VisualID != "r1_pro_chassis" ||
		robot.Visual.LocalBounds[2] < 1.6 {
		t.Fatalf("R1 Pro 应使用真实模型边界: %+v", robot.Visual)
	}
}
