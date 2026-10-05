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
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/simulation"
)

const nativeVisualContentVersion = "3"

func nativeVisualID(entry simulation.SceneAssetCatalogEntry) string {
	model, _ := entry.Asset.Metadata["model"].(string)
	category, _ := entry.Asset.Metadata["category"].(string)
	if category == "" {
		category, _ = entry.DefaultProperties["category"].(string)
	}
	switch category {
	case "box", "pallet", "target":
		return category
	}
	return strings.TrimSpace(model)
}

func containsTag(tags []string, expected string) bool {
	for _, tag := range tags {
		if tag == expected {
			return true
		}
	}
	return false
}

// projectVisualCatalog 为离线逻辑目录附加 Project 绑定 Runtime 的安全内容 URL。
// URL 不包含 endpoint 或资产路径；Runtime 离线时目录仍可浏览，真正加载 GLB 时
// Framework 才 Ensure Runtime。
func projectVisualCatalog(
	projectID string, entries []simulation.SceneAssetCatalogEntry,
) []simulation.SceneAssetCatalogEntry {
	for index := range entries {
		entry := &entries[index]
		if !containsTag(entry.Tags, "native-mujoco") {
			continue
		}
		visualID := nativeVisualID(*entry)
		if visualID == "" {
			continue
		}
		descriptor := simulation.VisualAssetDescriptor{}
		if entry.Visual != nil {
			descriptor = *entry.Visual
		}
		descriptor.VisualID = visualID
		descriptor.Version = nativeVisualContentVersion
		descriptor.FallbackGeometry = entry.Preview
		descriptor.LocalBounds = entry.Preview.Size
		if visualID == "r1_pro_chassis" {
			// 来自 Runtime 同一 MJCF 默认姿态的实际视觉边界，避免把 Robot
			// 按旧占位盒的 1.2m 高度错误缩小。
			descriptor.LocalBounds = [3]float64{0.636, 0.674953, 1.695695}
		}
		if descriptor.PlacementAnchor == "" {
			descriptor.PlacementAnchor = "bottom_center"
		}
		if descriptor.RuntimeBindings == nil {
			descriptor.RuntimeBindings = map[string]string{
				"native-mujoco": visualID,
			}
		}
		contentURL := "/api/v1/projects/" + url.PathEscape(projectID) +
			"/simulation/visual-assets/" + url.PathEscape(visualID) +
			"/" + url.PathEscape(descriptor.Version) + ".glb"
		descriptor.EditorVisual = &simulation.VisualAssetContent{
			ContentURL: contentURL, Format: "glb",
		}
		descriptor.MapVisual = &simulation.VisualAssetContent{
			ContentURL: contentURL, Format: "glb",
		}
		entry.Visual = &descriptor
	}
	return entries
}

// HandleVisualAsset 代理绑定 Runtime 的版本化视觉内容。
// 浏览器不能提交本地路径，也不能绕过 Project Runtime 绑定选择别的引擎。
func (h *SimulationHandler) HandleVisualAsset(w http.ResponseWriter, r *http.Request) {
	if !h.projectOwned(w, r) {
		return
	}
	visualID := chi.URLParam(r, "visual_id")
	version := chi.URLParam(r, "version")
	content, mediaType, err := h.simulation.ProjectVisualAsset(
		r.Context(), chi.URLParam(r, "id"), visualID, version,
	)
	if h.writeError(w, err) {
		return
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Semantic-Visual", visualID+"@"+version)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}
