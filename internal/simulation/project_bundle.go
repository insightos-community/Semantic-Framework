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
	"fmt"
)

// ValidateProjectDocumentProfile 只按 Project Profile 校验可移植场景文档；
// 具体 Installation 在注册 RuntimeBundle 时按偏好或用户选择解析。
func (s *Service) ValidateProjectDocumentProfile(
	projectID string, document SceneDocument,
) (string, error) {
	// NewService 是单 Runtime 的测试/迁移构造器，没有 Project Store。产品
	// Bootstrap 总会配置 installation catalog 和 binding resolver。
	if s.installations == nil || s.projectBindings == nil {
		profileID, err := s.registry.DefaultProfileID()
		if err != nil {
			return "", err
		}
		if err := s.ValidateDocumentProfile(document, profileID); err != nil {
			return "", err
		}
		return profileID, nil
	}
	profile, err := s.ProjectRuntimeProfile(projectID)
	if err != nil {
		return "", err
	}
	if !profile.Capabilities.EditableScene {
		return "", fmt.Errorf("%w: Runtime Profile %s 不支持可编辑场景",
			ErrConflict, profile.RuntimeProfileID)
	}
	sceneKind := document.SceneKind
	if sceneKind == "" {
		sceneKind = "scene_document"
	}
	if !profileSupportsSceneKind(profile, sceneKind) {
		return "", fmt.Errorf("%w: 场景类型 %s 与 Project Runtime 不兼容",
			ErrConflict, sceneKind)
	}
	return profile.RuntimeProfileID, nil
}

// RegisterProjectRuntimeBundle 在真正注册时选择可用 Installation。Bundle
// 仍以 Profile 为可移植身份，受管 Runtime 重启后可在兼容安装上重新注册。
func (s *Service) RegisterProjectRuntimeBundle(
	ctx context.Context, projectID string, bundle RuntimeBundle,
) (RuntimeBundleResult, error) {
	if s.installations == nil || s.projectBindings == nil {
		return s.RegisterRuntimeBundle(ctx, bundle)
	}
	installation, err := s.SelectProjectRuntimeInstallation(projectID, bundle.RuntimeProfileID, "")
	if err != nil {
		return RuntimeBundleResult{}, err
	}
	if err := validateInstallationAvailable(installation); err != nil {
		return RuntimeBundleResult{}, err
	}
	if bundle.RuntimeProfileID != installation.Profile.RuntimeProfileID {
		return RuntimeBundleResult{}, fmt.Errorf(
			"%w: RuntimeBundle profile %s 与 Project Runtime %s 不一致",
			ErrConflict, bundle.RuntimeProfileID, installation.Profile.RuntimeProfileID)
	}
	if _, err := s.EnsureRuntimeInstallation(ctx, installation.InstallationID); err != nil {
		return RuntimeBundleResult{}, err
	}
	binding, err := s.registry.Binding(installation.InstallationID)
	if err != nil {
		return RuntimeBundleResult{}, err
	}
	result, err := binding.Client.RegisterRuntimeBundle(ctx, bundle)
	if err != nil {
		return RuntimeBundleResult{}, err
	}
	if err := validateRuntimeBundleResult(bundle, result); err != nil {
		return result, err
	}
	return result, nil
}
