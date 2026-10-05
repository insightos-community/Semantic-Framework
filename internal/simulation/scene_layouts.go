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
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// normalizeSceneIdentity 为重构前保存的单 Layout 文档补齐稳定身份。旧文档仍然
// 可以继续编辑和构建，但不会因为每次读取而生成不同的 Scene/Layout ID。
func normalizeSceneIdentity(document *SceneDocument) {
	if document == nil {
		return
	}
	if strings.TrimSpace(document.SceneID) == "" {
		document.SceneID = "scene-" + strings.TrimPrefix(document.ID, "scene-doc-")
	}
	if strings.TrimSpace(document.LayoutID) == "" {
		document.LayoutID = "layout-default"
	}
	if strings.TrimSpace(document.LayoutName) == "" {
		document.LayoutName = "Default"
	}
	if document.Tags == nil {
		document.Tags = []string{}
	}
}

// ListLayouts 返回同一逻辑场景的全部草稿和已发布版本。调用方按 layout_id 分组，
// 不能把同一 Layout 的历史已发布版本误当成多个 Layout。
func (s *SceneAuthoringService) ListLayouts(
	projectID, sceneID string,
) ([]SceneDocument, error) {
	documents, err := s.List(projectID)
	if err != nil {
		return nil, err
	}
	result := make([]SceneDocument, 0)
	for _, document := range documents {
		if document.SceneID == sceneID {
			result = append(result, document)
		}
	}
	if len(result) == 0 {
		return nil, ErrNotFound
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].LayoutName != result[j].LayoutName {
			return result[i].LayoutName < result[j].LayoutName
		}
		if result[i].Version != result[j].Version {
			return result[i].Version > result[j].Version
		}
		return result[i].UpdatedAt.After(result[j].UpdatedAt)
	})
	return result, nil
}

// CreateLayout 从一个已有 Layout 复制完整场景内容。新 Layout 是独立草稿和
// RuntimeBundle，因此后续切换始终走停止旧实例、加载新 Bundle 的受控流程。
func (s *SceneAuthoringService) CreateLayout(
	projectID, sourceDocumentID, name string,
) (SceneDocument, error) {
	source, err := s.Get(projectID, sourceDocumentID)
	if err != nil {
		return SceneDocument{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return SceneDocument{}, errors.New("layout name 不能为空")
	}
	documents, err := s.ListLayouts(projectID, source.SceneID)
	if err != nil {
		return SceneDocument{}, err
	}
	for _, document := range documents {
		if strings.EqualFold(document.LayoutName, name) {
			return SceneDocument{}, fmt.Errorf("%w: Layout 名称已存在", ErrConflict)
		}
	}
	next, err := cloneSceneDocument(source)
	if err != nil {
		return SceneDocument{}, err
	}
	next.ID = "scene-doc-" + uuid.NewString()
	next.LayoutID = "layout-" + uuid.NewString()
	next.LayoutName = name
	next.Status = "draft"
	next.Revision = 1
	now := time.Now().UTC()
	next.CreatedAt, next.UpdatedAt = now, now
	if err := s.save(&next); err != nil {
		return SceneDocument{}, err
	}
	return next, nil
}

// RenameLayout 只允许修改草稿显示名。已发布 Layout 的名称属于版本内容，必须
// 先 Fork 新草稿，避免历史 Scene Package 和运行证据发生漂移。
func (s *SceneAuthoringService) RenameLayout(
	projectID, documentID string,
	expectedRevision int64,
	name string,
) (SceneDocument, error) {
	document, err := s.Get(projectID, documentID)
	if err != nil {
		return SceneDocument{}, err
	}
	if document.Status == "published" {
		return SceneDocument{}, fmt.Errorf("%w: 已发布 Layout 不能重命名", ErrConflict)
	}
	if document.Revision != expectedRevision {
		return SceneDocument{}, fmt.Errorf("%w: Layout revision 已变化", ErrConflict)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return SceneDocument{}, errors.New("layout name 不能为空")
	}
	documents, err := s.ListLayouts(projectID, document.SceneID)
	if err != nil {
		return SceneDocument{}, err
	}
	for _, other := range documents {
		if other.LayoutID != document.LayoutID && strings.EqualFold(other.LayoutName, name) {
			return SceneDocument{}, fmt.Errorf("%w: Layout 名称已存在", ErrConflict)
		}
	}
	document.LayoutName = name
	document.Revision++
	document.UpdatedAt = time.Now().UTC()
	if err := s.save(&document); err != nil {
		return SceneDocument{}, err
	}
	return document, nil
}

// DeleteLayout 删除一个尚未发布的 Layout 草稿。至少保留一个不同 Layout；已发布
// 文件绝不由该操作删除，历史版本仍可导出和复现。
func (s *SceneAuthoringService) DeleteLayout(
	projectID, documentID string,
) error {
	document, err := s.Get(projectID, documentID)
	if err != nil {
		return err
	}
	if document.Status == "published" {
		return fmt.Errorf("%w: 已发布 Layout 不能删除", ErrConflict)
	}
	documents, err := s.ListLayouts(projectID, document.SceneID)
	if err != nil {
		return err
	}
	hasOther := false
	for _, other := range documents {
		if other.LayoutID != document.LayoutID {
			hasOther = true
			break
		}
	}
	if !hasOther {
		return fmt.Errorf("%w: 场景至少保留一个 Layout", ErrConflict)
	}
	path, err := s.documentPath(projectID, documentID)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

func cloneSceneDocument(source SceneDocument) (SceneDocument, error) {
	data, err := json.Marshal(source)
	if err != nil {
		return SceneDocument{}, err
	}
	var result SceneDocument
	if err := json.Unmarshal(data, &result); err != nil {
		return SceneDocument{}, err
	}
	normalizeSceneIdentity(&result)
	return result, nil
}
