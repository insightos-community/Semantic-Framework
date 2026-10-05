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

import "strings"

// validateCatalogVisual 校验浏览器视觉描述。视觉模型是可选增强；缺失时 Studio
// 明确使用 FallbackGeometry，不能把 Runtime 的 MJCF/URDF 或宿主路径交给浏览器。
func validateCatalogVisual(field string, visual *VisualAssetDescriptor) []string {
	if visual == nil {
		return nil
	}
	problems := make([]string, 0)
	if !assetCatalogIDPattern.MatchString(visual.VisualID) {
		problems = append(problems, field+".visual.visual_id 必须是稳定的小写标识")
	}
	if strings.TrimSpace(visual.Version) == "" {
		problems = append(problems, field+".visual.version 不能为空")
	}
	for name, content := range map[string]*VisualAssetContent{
		"editor_visual": visual.EditorVisual,
		"map_visual":    visual.MapVisual,
	} {
		if content == nil {
			continue
		}
		if content.Format != "glb" {
			problems = append(problems, field+".visual."+name+".format 必须是 glb")
		}
		if !strings.HasPrefix(content.ContentURL, "/api/") ||
			strings.Contains(content.ContentURL, "..") {
			problems = append(problems,
				field+".visual."+name+".content_url 必须是安全的站内 API 地址")
		}
	}
	for _, value := range visual.LocalBounds {
		if value <= 0 {
			problems = append(problems, field+".visual.local_bounds 三个分量必须大于零")
			break
		}
	}
	if visual.PlacementAnchor != "bottom_center" && visual.PlacementAnchor != "origin" {
		problems = append(problems,
			field+".visual.placement_anchor 必须是 bottom_center 或 origin")
	}
	return problems
}
