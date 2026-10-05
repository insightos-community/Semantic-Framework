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
