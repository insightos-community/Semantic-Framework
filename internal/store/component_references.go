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

func (s *Store) RemoveProjectSceneReference(projectID, referenceID string) error {
	_, err := s.db.Exec(`DELETE FROM project_scene_references WHERE project_id = ? AND project_scene_id = ?`, projectID, referenceID)
	return err
}

// ProjectsUsingCatalogScene 用于独立场景包卸载检查，历史执行记录不属于当前项目引用。
func (s *Store) ProjectsUsingCatalogScene(sceneID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT project_id FROM project_scene_references WHERE catalog_scene_id = ?`, sceneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
