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
	"reflect"
	"slices"
)

func (s *SceneAuthoringService) validatePublishedAssets(
	document SceneDocument,
) []ValidationIssue {
	published := make(map[string]SceneAsset)
	publishedEntries := make(map[string]SceneAssetCatalogEntry)
	for _, entry := range s.AssetCatalog() {
		published[entry.Asset.ID] = entry.Asset
		publishedEntries[entry.Asset.ID] = entry
	}
	issues := make([]ValidationIssue, 0)
	for _, asset := range document.Assets {
		expected, exists := published[asset.ID]
		if !exists {
			issues = append(issues, ValidationIssue{
				Level: "error", Field: "assets." + asset.ID,
				Message: "资产不在当前发布目录中",
			})
			continue
		}
		if !reflect.DeepEqual(expected, asset) {
			issues = append(issues, ValidationIssue{
				Level: "error", Field: "assets." + asset.ID,
				Message: "资产描述与发布目录不一致",
			})
		}
		if document.Authoring != nil && len(document.Authoring.AllowedAssetTags) > 0 {
			allowed := false
			for _, tag := range publishedEntries[asset.ID].Tags {
				if slices.Contains(document.Authoring.AllowedAssetTags, tag) {
					allowed = true
					break
				}
			}
			if !allowed {
				issues = append(issues, ValidationIssue{
					Level: "error", Field: "assets." + asset.ID,
					Message: "资产不属于当前公共场景允许的素材集合",
				})
			}
		}
	}
	return issues
}
