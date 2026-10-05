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
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// RuntimeSceneDocument 是交给 Runtime 编译的纯场景内容。
// Project、草稿状态、revision 和发布时间都属于 Framework，不发送给 Plugin。
type RuntimeSceneDocument struct {
	ID          string                     `json:"id"`
	Name        string                     `json:"name"`
	Description string                     `json:"description,omitempty"`
	SceneKind   string                     `json:"scene_kind"`
	Assets      []SceneAsset               `json:"assets"`
	Nodes       []SceneNode                `json:"nodes"`
	Regions     []SceneNode                `json:"regions"`
	Physics     map[string]any             `json:"physics"`
	Extensions  map[string]json.RawMessage `json:"extensions,omitempty"`
}

func NewRuntimeSceneDocument(document SceneDocument) RuntimeSceneDocument {
	return RuntimeSceneDocument{
		ID: document.ID, Name: document.Name, Description: document.Description,
		SceneKind: document.SceneKind, Assets: document.Assets,
		Nodes: document.Nodes, Regions: document.Regions,
		Physics: document.Physics, Extensions: document.Extensions,
	}
}

// RuntimeBundle 是 SceneDocument 通过 Framework 校验后交给特定 Runtime profile
// 构建的不可变输入。它不包含 Framework 主机路径，Runtime 只能解析文档和资产标识。
type RuntimeBundle struct {
	RuntimeBundleID  string               `json:"runtime_bundle_id"`
	DocumentID       string               `json:"document_id"`
	Revision         int64                `json:"revision"`
	SceneVersion     int64                `json:"scene_version"`
	SceneKey         string               `json:"scene_key"`
	RuntimeProfileID string               `json:"runtime_profile_id"`
	Document         RuntimeSceneDocument `json:"document"`
	Validation       ValidationResult     `json:"validation"`
	CreatedAt        time.Time            `json:"created_at"`
}

// RuntimeBundleResult 是 Runtime 对构建输入的校验和编译结果。
type RuntimeBundleResult struct {
	RuntimeBundleID  string            `json:"runtime_bundle_id"`
	SceneKey         string            `json:"scene_key"`
	RuntimeProfileID string            `json:"runtime_profile_id"`
	RuntimeSceneKey  string            `json:"runtime_scene_key"`
	Valid            bool              `json:"valid"`
	Issues           []ValidationIssue `json:"issues"`
	Descriptor       *SceneDescriptor  `json:"descriptor,omitempty"`
}

// SceneBuildService 连接 Framework 场景草稿和外部 Runtime 构建。
// 草稿保存、revision 和发布版本始终由 Framework 管理。
type SceneBuildService struct {
	authoring  *SceneAuthoringService
	simulation *Service
}

func NewSceneBuildService(authoring *SceneAuthoringService, service *Service) *SceneBuildService {
	return &SceneBuildService{authoring: authoring, simulation: service}
}

func (s *SceneBuildService) List(projectID string) ([]RuntimeBundle, error) {
	return s.authoring.store.ListRuntimeBundles(projectID)
}

func (s *SceneBuildService) Build(
	ctx context.Context, projectID, documentID, profileID string,
) (RuntimeBundle, RuntimeBundleResult, error) {
	document, err := s.authoring.Get(projectID, documentID)
	if err != nil {
		return RuntimeBundle{}, RuntimeBundleResult{}, err
	}
	validation := s.authoring.ValidateForBuild(document)
	if !validation.Valid {
		return RuntimeBundle{DocumentID: document.ID, Revision: document.Revision,
			Validation: validation}, RuntimeBundleResult{}, ErrConflict
	}
	projectProfileID, err := s.simulation.ValidateProjectDocumentProfile(projectID, document)
	if err != nil {
		return RuntimeBundle{}, RuntimeBundleResult{}, err
	}
	if profileID != "" && profileID != projectProfileID {
		return RuntimeBundle{}, RuntimeBundleResult{}, ErrConflict
	}
	profileID = projectProfileID
	bundle := RuntimeBundle{
		RuntimeBundleID: "runtime-bundle-" + uuid.NewString(),
		DocumentID:      document.ID, Revision: document.Revision,
		SceneVersion: document.Version + 1, SceneKey: document.ID,
		RuntimeProfileID: profileID, Document: NewRuntimeSceneDocument(document),
		Validation: validation, CreatedAt: time.Now().UTC(),
	}
	result, err := s.simulation.RegisterProjectRuntimeBundle(ctx, projectID, bundle)
	if err != nil {
		return bundle, result, err
	}
	// 只有 Runtime 确认可以构建后才保存，避免被拒绝的输入满足发布前置条件。
	if err := s.authoring.store.SaveRuntimeBundle(projectID, bundle); err != nil {
		return RuntimeBundle{}, RuntimeBundleResult{}, err
	}
	return bundle, result, nil
}
