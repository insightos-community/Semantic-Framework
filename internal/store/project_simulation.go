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

package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CountProjectsUsingRuntime 供管理员 CLI 在卸载前检查当前偏好。Project 引用
// Profile 而不是 Installation，因此清除偏好后不会阻止另一套兼容安装接管。
func (s *Store) CountProjectsUsingRuntime(installationID string) (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM projects
		WHERE preferred_runtime_installation_id = ?`, installationID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("检查 Runtime 的 Project 偏好失败: %w", err)
	}
	return count, nil
}

// ProjectSceneReference 是 Project 对全局只读场景目录中某个已发布版本的引用。
// 它不复制 Runtime 本机路径；每次启动由 Project Profile 重新选择兼容 Installation。
type ProjectSceneReference struct {
	ProjectSceneID   string    `json:"project_scene_id"`
	ProjectID        string    `json:"project_id"`
	CatalogSceneID   string    `json:"catalog_scene_id"`
	SceneVersion     string    `json:"scene_version"`
	DefaultVariantID string    `json:"default_variant_id,omitempty"`
	AddedAt          time.Time `json:"added_at"`
}

// SetProjectRuntimePreference 更新 Project 的默认 Profile 和当前 Server 上的安装
// 偏好。活动 Scene 的真实 Installation 保存在 RuntimeState，不会被这里改写。
func (s *Store) SetProjectRuntimePreference(
	ownerID, projectID, profileID, preferredInstallationID string,
) (Project, error) {
	profileID = strings.TrimSpace(profileID)
	preferredInstallationID = strings.TrimSpace(preferredInstallationID)
	if profileID == "" {
		return Project{}, fmt.Errorf("runtime_profile_id 不能为空")
	}
	s.projectWriteMu.Lock()
	defer s.projectWriteMu.Unlock()
	project, err := s.GetProject(projectID)
	if err != nil || project.OwnerID != ownerID {
		return Project{}, ErrNotFound
	}
	if project.ArchivedAt != nil {
		return Project{}, ErrProjectArchived
	}
	if project.RuntimeProfileID == profileID &&
		project.PreferredRuntimeInstallationID == preferredInstallationID {
		return project, nil
	}
	result, err := s.db.Exec(`UPDATE projects SET runtime_profile_id = ?,
		preferred_runtime_installation_id = ?,
		revision = revision + 1, updated_at = ? WHERE id = ? AND owner_id = ?
		AND archived_at IS NULL`, profileID, preferredInstallationID,
		time.Now().UTC(), projectID, ownerID)
	if err != nil {
		return Project{}, fmt.Errorf("保存 Project Runtime 偏好失败: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return Project{}, ErrInvalidState
	}
	return s.GetProject(projectID)
}

// RememberProjectRuntimePreference 保存服务端已验证的 Profile/Installation 组合。
// 它供启动成功和 v21 数据一次性回填使用，不是绕过用户归属的公共写接口。
func (s *Store) RememberProjectRuntimePreference(
	projectID, profileID, installationID string,
) error {
	result, err := s.db.Exec(`UPDATE projects SET runtime_profile_id = ?,
		preferred_runtime_installation_id = ?,
		revision = revision + 1, updated_at = ? WHERE id = ? AND archived_at IS NULL`,
		strings.TrimSpace(profileID), strings.TrimSpace(installationID), time.Now().UTC(), projectID)
	if err != nil {
		return fmt.Errorf("保存 Project Runtime 启动偏好失败: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotFound
	}
	return nil
}

// AddProjectSceneReference 保存经过 Simulation Service 兼容性校验的引用。
func (s *Store) AddProjectSceneReference(reference ProjectSceneReference) (ProjectSceneReference, error) {
	if reference.ProjectID == "" || reference.CatalogSceneID == "" ||
		reference.SceneVersion == "" {
		return ProjectSceneReference{}, fmt.Errorf("Project 场景引用字段不完整")
	}
	if reference.ProjectSceneID == "" {
		reference.ProjectSceneID = "project-scene-" + uuid.NewString()
	}
	if reference.AddedAt.IsZero() {
		reference.AddedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(`INSERT INTO project_scene_references
		(project_scene_id, project_id, catalog_scene_id, scene_version,
		 default_variant_id, added_at)
		VALUES (?, ?, ?, ?, ?, ?)`, reference.ProjectSceneID, reference.ProjectID,
		reference.CatalogSceneID, reference.SceneVersion,
		reference.DefaultVariantID, reference.AddedAt)
	if err != nil {
		return ProjectSceneReference{}, fmt.Errorf("保存 Project 场景引用失败: %w", err)
	}
	return reference, nil
}

// ListProjectSceneReferences 返回 Project 的稳定场景资源列表。
func (s *Store) ListProjectSceneReferences(projectID string) ([]ProjectSceneReference, error) {
	rows, err := s.db.Query(`SELECT project_scene_id, project_id, catalog_scene_id,
		scene_version, default_variant_id, added_at
		FROM project_scene_references WHERE project_id = ? ORDER BY added_at DESC,
		project_scene_id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("查询 Project 场景引用失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := []ProjectSceneReference{}
	for rows.Next() {
		var item ProjectSceneReference
		if err := rows.Scan(&item.ProjectSceneID, &item.ProjectID, &item.CatalogSceneID,
			&item.SceneVersion, &item.DefaultVariantID,
			&item.AddedAt); err != nil {
			return nil, fmt.Errorf("扫描 Project 场景引用失败: %w", err)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// GetProjectSceneReference 防止通过另一个 Project 的引用启动场景。
func (s *Store) GetProjectSceneReference(projectID, projectSceneID string) (ProjectSceneReference, error) {
	var item ProjectSceneReference
	err := s.db.QueryRow(`SELECT project_scene_id, project_id, catalog_scene_id,
		scene_version, default_variant_id, added_at
		FROM project_scene_references WHERE project_id = ? AND project_scene_id = ?`,
		projectID, projectSceneID).Scan(&item.ProjectSceneID, &item.ProjectID,
		&item.CatalogSceneID, &item.SceneVersion, &item.DefaultVariantID, &item.AddedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectSceneReference{}, ErrNotFound
	}
	if err != nil {
		return ProjectSceneReference{}, fmt.Errorf("查询 Project 场景引用失败: %w", err)
	}
	return item, nil
}
