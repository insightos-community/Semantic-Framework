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
	"fmt"
	"sort"
	"strings"
)

// RefreshProjectPublishedSceneCatalog 从 Project 工作区重建自建场景目录。
//
// 每次发布都是一个不可修改的目录版本；该版本包含发布时各 Layout 的最新版本。
// RuntimeBundle 身份保存在具体 Layout 上，启动与切换不会错误复用其他 Layout。
// 方法只读取 Project 文件并原子替换对应来源，因此 Server 重启后也可以恢复目录。
func (s *Service) RefreshProjectPublishedSceneCatalog(
	projectID string, authoring *SceneAuthoringService,
) ([]SceneCatalogEntry, error) {
	if s.sceneCatalog == nil || authoring == nil {
		return nil, nil
	}
	profile, err := s.ProjectRuntimeProfile(projectID)
	if err != nil {
		return nil, err
	}
	documents, err := authoring.List(projectID)
	if err != nil {
		return nil, err
	}
	bundles, err := authoring.store.ListRuntimeBundles(projectID)
	if err != nil {
		return nil, err
	}
	bundleByRevision := make(map[string]RuntimeBundle, len(bundles))
	for _, bundle := range bundles {
		bundleByRevision[documentRevisionKey(bundle.DocumentID, bundle.Revision)] = bundle
	}

	byScene := make(map[string][]SceneDocument)
	for _, document := range documents {
		normalizeSceneIdentity(&document)
		if document.Status == "published" {
			byScene[document.SceneID] = append(byScene[document.SceneID], document)
		}
	}
	entries := make([]SceneCatalogEntry, 0, len(byScene))
	for sceneID, published := range byScene {
		sort.Slice(published, func(i, j int) bool {
			if !published[i].UpdatedAt.Equal(published[j].UpdatedAt) {
				return published[i].UpdatedAt.Before(published[j].UpdatedAt)
			}
			return published[i].ID < published[j].ID
		})
		latestByLayout := make(map[string]SceneDocument)
		versions := make([]SceneCatalogVersion, 0, len(published))
		for _, event := range published {
			eventBundle, ok := bundleByRevision[documentRevisionKey(event.ID, event.Revision-1)]
			if !ok {
				return nil, fmt.Errorf("%w: 已发布 Layout %s 缺少 RuntimeBundle",
					ErrConflict, event.LayoutName)
			}
			if eventBundle.RuntimeProfileID != profile.RuntimeProfileID {
				return nil, fmt.Errorf("%w: 已发布 Layout %s 与 Project Runtime 不兼容",
					ErrConflict, event.LayoutName)
			}
			latestByLayout[event.LayoutID] = event
			variants, err := publishedLayoutVariants(latestByLayout, bundleByRevision)
			if err != nil {
				return nil, err
			}
			versions = append(versions, SceneCatalogVersion{
				Version:         fmt.Sprintf("published-%d-%s", event.UpdatedAt.UnixNano(), event.ID),
				RuntimeSceneKey: eventBundle.SceneKey,
				Published:       true,
				RobotModels:     append([]string{}, profile.Capabilities.RobotModels...),
				Variants:        variants,
				Capabilities:    projectSceneCapabilities(profile.Capabilities),
			})
		}
		// 目录首先展示最新发布版本；历史 ProjectSceneReference 仍能解析旧版本。
		for left, right := 0, len(versions)-1; left < right; left, right = left+1, right-1 {
			versions[left], versions[right] = versions[right], versions[left]
		}
		latest := published[len(published)-1]
		entries = append(entries, SceneCatalogEntry{
			SceneID:                  sceneID,
			Name:                     latest.Name,
			Description:              latest.Description,
			Tags:                     append([]string{}, latest.Tags...),
			Engine:                   profile.Engine,
			Loader:                   profile.Loader,
			CompatibleRuntimeProfile: profile.RuntimeProfileID,
			Preview:                  latest.Preview,
			Versions:                 versions,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	if err := s.sceneCatalog.ReplaceSourceEntries("project:"+projectID, entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func publishedLayoutVariants(
	documents map[string]SceneDocument,
	bundles map[string]RuntimeBundle,
) ([]SceneCatalogVariant, error) {
	variants := make([]SceneCatalogVariant, 0, len(documents))
	for _, document := range documents {
		bundle, ok := bundles[documentRevisionKey(document.ID, document.Revision-1)]
		if !ok {
			return nil, fmt.Errorf("%w: 已发布 Layout %s 缺少 RuntimeBundle",
				ErrConflict, document.LayoutName)
		}
		variants = append(variants, SceneCatalogVariant{
			VariantID:       document.LayoutID,
			Name:            document.LayoutName,
			Kind:            "layout",
			Description:     document.Description,
			RuntimeSceneKey: bundle.SceneKey,
			RuntimeBundleID: bundle.RuntimeBundleID,
			RuntimeLayout:   fmt.Sprintf("version-%d", bundle.SceneVersion),
		})
	}
	sort.Slice(variants, func(i, j int) bool { return variants[i].Name < variants[j].Name })
	return variants, nil
}

func documentRevisionKey(documentID string, revision int64) string {
	return fmt.Sprintf("%s:%d", documentID, revision)
}

func projectSceneCapabilities(capabilities RuntimeCapability) []string {
	result := []string{"scene_details", "layouts"}
	if capabilities.EditableScene {
		result = append(result, "scene_editor")
	}
	if capabilities.Viewer {
		result = append(result, "viewer")
	}
	if capabilities.SceneReset {
		result = append(result, "reset")
	}
	if capabilities.SceneStep {
		result = append(result, "step")
	}
	sort.Strings(result)
	return result
}

// SceneCatalogForProject 隔离 Project 自建场景。静态安装目录对所有 Project 可见，
// project:<id> 来源只对所属 Project 返回，避免曾经浏览过的其他 Project 内容泄漏。
func (s *Service) SceneCatalogForProject(projectID, profileID string) []SceneCatalogEntry {
	entries := s.SceneCatalog(profileID)
	if strings.TrimSpace(projectID) == "" {
		result := entries[:0]
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Source, "project:") {
				result = append(result, entry)
			}
		}
		return result
	}
	source := "project:" + projectID
	result := entries[:0]
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Source, "project:") || entry.Source == source {
			result = append(result, entry)
		}
	}
	return result
}
