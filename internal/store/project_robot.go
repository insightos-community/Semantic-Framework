package store

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"
)

func migrateV37(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE project_robot_bindings (
      project_id TEXT NOT NULL REFERENCES projects(id), robot_id TEXT NOT NULL,
      created_at TIMESTAMP NOT NULL, PRIMARY KEY(project_id, robot_id))`)
	if err != nil {
		return err
	}
	// Preserve the real robot already explicitly selected as this project's
	// Pilot map source. New map selections do not implicitly add robots.
	_, err = tx.Exec(`INSERT OR IGNORE INTO project_robot_bindings
      SELECT m.project_id, p.robot_id, m.synced_at FROM project_pilot_maps m
      JOIN robot_pilots p ON p.pilot_instance_id=m.pilot_id
      JOIN projects j ON j.id=m.project_id
      JOIN pilot_enrollments e ON e.pilot_instance_id=p.pilot_instance_id
      WHERE e.created_by=j.owner_id AND e.status='claimed'`)
	return err
}

func (s *Store) SetProjectRobot(projectID, robotID string, added bool) error {
	project, err := s.GetProject(projectID)
	if err != nil {
		return err
	}
	if project.ArchivedAt != nil {
		return ErrProjectArchived
	}
	if strings.TrimSpace(robotID) == "" {
		return ErrInvalidState
	}
	if added {
		_, err = s.db.Exec(`INSERT OR IGNORE INTO project_robot_bindings(project_id,robot_id,created_at) VALUES(?,?,?)`, projectID, robotID, time.Now().UTC())
	} else {
		_, err = s.db.Exec(`DELETE FROM project_robot_bindings WHERE project_id=? AND robot_id=?`, projectID, robotID)
	}
	return err
}

// Managed runtimes already carry their project; do not create a competing copy.
func (s *Store) ListProjectRobotIDs(projectID string) ([]string, error) {
	if _, err := s.GetProject(projectID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT robot_id FROM project_robot_bindings WHERE project_id=? ORDER BY robot_id`, projectID)
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids[id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	instances, err := s.ListRuntimeInstances(context.Background())
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, item := range instances {
		if seen[item.RobotID] {
			continue
		}
		seen[item.RobotID] = true
		if item.ProjectID == projectID {
			ids[item.RobotID] = true
		}
	}
	result := make([]string, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	sort.Strings(result)
	return result, nil
}

func (s *Store) ProjectHasRobot(projectID, robotID string) (bool, error) {
	ids, err := s.ListProjectRobotIDs(projectID)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if id == robotID {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) RobotProjectIDs(robotID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT project_id FROM project_robot_bindings WHERE robot_id=? ORDER BY project_id`, robotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}
