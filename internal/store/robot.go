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
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"insightos.cn/semantic-framework/internal/robotruntime"
)

// RobotSkillPackage 是 Server Robot Skill Registry 的持久条目。PackagePath
// 指向独立 Package Store，不是 Project Artifact。
type RobotSkillPackage struct {
	Name             string           `json:"name"`
	Version          string           `json:"version"`
	Description      string           `json:"description"`
	Category         string           `json:"category"`
	PackagePath      string           `json:"package_path"`
	ApplicableModels []string         `json:"applicable_models,omitempty"`
	RequiredActions  []map[string]any `json:"required_actions"`
	StopActions      []map[string]any `json:"stop_actions"`
	PublishedAt      time.Time        `json:"published_at"`
}

type RobotPilot struct {
	PilotInstanceID        string                        `json:"pilot_instance_id"`
	RobotID                string                        `json:"robot_id"`
	DisplayName            string                        `json:"display_name,omitempty"`
	RobotModel             string                        `json:"robot_model"`
	Backend                string                        `json:"backend"`
	PilotVersion           string                        `json:"pilot_version"`
	Status                 string                        `json:"status"`
	RobotStatus            string                        `json:"robot_status"`
	AbilityFrameworkStatus string                        `json:"ability_framework_status"`
	SkillCatalogRevision   int64                         `json:"skill_catalog_revision"`
	AbilityCatalogRevision int64                         `json:"ability_catalog_revision"`
	CurrentExecutionID     string                        `json:"current_execution_id,omitempty"`
	Abilities              []map[string]any              `json:"abilities,omitempty"`
	Sensors                []map[string]any              `json:"sensors,omitempty"`
	Configuration          map[string]any                `json:"configuration,omitempty"`
	DesiredSkills          []RobotDesiredSkill           `json:"desired_skills,omitempty"`
	RuntimeInstance        *robotruntime.RuntimeInstance `json:"runtime_instance,omitempty"`
	Revision               int64                         `json:"revision"`
	LastSeenAt             time.Time                     `json:"last_seen_at"`
}

type RobotPilotSkill struct {
	PilotInstanceID string    `json:"pilot_instance_id"`
	Name            string    `json:"name"`
	Version         string    `json:"version"`
	Enabled         bool      `json:"enabled"`
	Status          string    `json:"status"`
	Error           string    `json:"error,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type RobotExecution struct {
	ID              string                 `json:"id"`
	ProjectID       string                 `json:"project_id"`
	WorkflowID      string                 `json:"workflow_id,omitempty"`
	TaskID          string                 `json:"task_id,omitempty"`
	SubtaskID       string                 `json:"subtask_id,omitempty"`
	RunID           string                 `json:"run_id,omitempty"`
	RobotID         string                 `json:"robot_id"`
	PilotInstanceID string                 `json:"pilot_instance_id"`
	SkillName       string                 `json:"skill_name"`
	SkillVersion    string                 `json:"skill_version"`
	RequestKey      string                 `json:"request_key"`
	RequestDigest   string                 `json:"request_digest,omitempty"`
	Status          string                 `json:"status"`
	Stage           string                 `json:"stage,omitempty"`
	Progress        *float64               `json:"progress,omitempty"`
	Input           map[string]any         `json:"input"`
	ArtifactRefs    []string               `json:"artifact_refs"`
	ArtifactSync    []RobotArtifactMapping `json:"artifact_sync,omitempty"`
	Result          map[string]any         `json:"result,omitempty"`
	Error           map[string]any         `json:"error,omitempty"`
	Revision        int64                  `json:"revision"`
	CreatedAt       time.Time              `json:"created_at"`
	UpdatedAt       time.Time              `json:"updated_at"`
}

type RobotExecutionEvent struct {
	ExecutionID string         `json:"execution_id"`
	Sequence    int64          `json:"sequence"`
	Type        string         `json:"type"`
	Payload     map[string]any `json:"payload"`
	CreatedAt   time.Time      `json:"created_at"`
}

type RobotArtifactMapping struct {
	PilotInstanceID  string    `json:"pilot_instance_id"`
	LocalArtifactID  string    `json:"local_artifact_id"`
	ServerArtifactID string    `json:"server_artifact_id,omitempty"`
	ExecutionID      string    `json:"execution_id"`
	ActionID         string    `json:"action_id,omitempty"`
	ObservationID    string    `json:"observation_id,omitempty"`
	MediaType        string    `json:"media_type"`
	Summary          string    `json:"summary"`
	SizeBytes        int64     `json:"size_bytes"`
	Status           string    `json:"status"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func (s *Store) RobotSkillPackageDir() string {
	return filepath.Join(filepath.Dir(s.databasePath), "robot-skills", "packages")
}

func scanRobotPilot(row *sql.Row) (RobotPilot, error) {
	var body []byte
	if err := row.Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RobotPilot{}, ErrNotFound
		}
		return RobotPilot{}, err
	}
	var item RobotPilot
	return item, json.Unmarshal(body, &item)
}

func scanRobotExecution(row interface{ Scan(...any) error }) (RobotExecution, error) {
	var body []byte
	if err := row.Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RobotExecution{}, ErrNotFound
		}
		return RobotExecution{}, err
	}
	var item RobotExecution
	return item, json.Unmarshal(body, &item)
}

func (s *Store) SaveRobotSkillPackage(item RobotSkillPackage) error {
	body, err := json.Marshal(item)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO robot_skill_packages(name,version,body) VALUES(?,?,?)
		ON CONFLICT(name,version) DO UPDATE SET body=excluded.body`, item.Name, item.Version, body)
	return err
}

func (s *Store) GetRobotSkillPackage(name, version string) (RobotSkillPackage, error) {
	var body []byte
	if err := s.db.QueryRow(`SELECT body FROM robot_skill_packages WHERE name=? AND version=?`, name, version).Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RobotSkillPackage{}, ErrNotFound
		}
		return RobotSkillPackage{}, err
	}
	var item RobotSkillPackage
	return item, json.Unmarshal(body, &item)
}

func (s *Store) ListRobotSkillPackages() ([]RobotSkillPackage, error) {
	rows, err := s.db.Query(`SELECT body FROM robot_skill_packages ORDER BY name,version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []RobotSkillPackage
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var item RobotSkillPackage
		if err := json.Unmarshal(body, &item); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) DeleteRobotSkillPackage(name, version string) error {
	result, err := s.db.Exec(`DELETE FROM robot_skill_packages WHERE name=? AND version=?`, name, version)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SaveRobotPilot(item RobotPilot) error {
	body, err := json.Marshal(item)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO robot_pilots(pilot_instance_id,robot_id,status,last_seen_at,body)
		VALUES(?,?,?,?,?) ON CONFLICT(pilot_instance_id) DO UPDATE SET
		robot_id=excluded.robot_id,status=excluded.status,last_seen_at=excluded.last_seen_at,body=excluded.body`,
		item.PilotInstanceID, item.RobotID, item.Status, item.LastSeenAt, body)
	return err
}

func (s *Store) GetRobotPilot(id string) (RobotPilot, error) {
	return scanRobotPilot(s.db.QueryRow(`SELECT body FROM robot_pilots WHERE pilot_instance_id=?`, id))
}

func (s *Store) GetActiveRobotPilot(robotID string) (RobotPilot, error) {
	return scanRobotPilot(s.db.QueryRow(`SELECT body FROM robot_pilots WHERE robot_id=? ORDER BY last_seen_at DESC LIMIT 1`, robotID))
}

func (s *Store) ListRobotPilots() ([]RobotPilot, error) {
	rows, err := s.db.Query(`SELECT body FROM robot_pilots ORDER BY robot_id,pilot_instance_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []RobotPilot
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var item RobotPilot
		if err := json.Unmarshal(body, &item); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) SaveRobotPilotSkill(item RobotPilotSkill) error {
	body, err := json.Marshal(item)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO robot_pilot_skills(pilot_instance_id,name,version,enabled,body)
		VALUES(?,?,?,?,?) ON CONFLICT(pilot_instance_id,name,version) DO UPDATE SET enabled=excluded.enabled,body=excluded.body`,
		item.PilotInstanceID, item.Name, item.Version, item.Enabled, body)
	return err
}

// ReplaceRobotPilotSkills 用 Pilot 本次连接实际扫描到的 active 目录替换旧快照。
// actual 是设备端事实，不能跨新的受管实例沿用；否则 Server 会误以为 Skill
// 已安装而跳过下发，直到 robot.run 才由新 Pilot 报 unknown skill。
func (s *Store) ReplaceRobotPilotSkills(pilotID string, items []RobotPilotSkill) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM robot_pilot_skills WHERE pilot_instance_id=?`, pilotID); err != nil {
		return err
	}
	for _, item := range items {
		item.PilotInstanceID = pilotID
		body, marshalErr := json.Marshal(item)
		if marshalErr != nil {
			return marshalErr
		}
		if _, err := tx.Exec(`INSERT INTO robot_pilot_skills(pilot_instance_id,name,version,enabled,body)
			VALUES(?,?,?,?,?)`, pilotID, item.Name, item.Version, item.Enabled, body); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) GetRobotPilotSkill(pilotID, name, version string) (RobotPilotSkill, error) {
	var body []byte
	if err := s.db.QueryRow(`SELECT body FROM robot_pilot_skills WHERE pilot_instance_id=? AND name=? AND version=?`, pilotID, name, version).Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RobotPilotSkill{}, ErrNotFound
		}
		return RobotPilotSkill{}, err
	}
	var item RobotPilotSkill
	return item, json.Unmarshal(body, &item)
}

func (s *Store) ListRobotPilotSkills(pilotID string) ([]RobotPilotSkill, error) {
	rows, err := s.db.Query(`SELECT body FROM robot_pilot_skills WHERE pilot_instance_id=? ORDER BY name,version`, pilotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []RobotPilotSkill
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var item RobotPilotSkill
		if err := json.Unmarshal(body, &item); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) SaveRobotExecution(item RobotExecution) error {
	body, err := json.Marshal(item)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO robot_executions(id,project_id,robot_id,request_key,status,revision,updated_at,body,subtask_id)
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET status=excluded.status,
		revision=excluded.revision,updated_at=excluded.updated_at,body=excluded.body,
		subtask_id=excluded.subtask_id`, item.ID, item.ProjectID, item.RobotID,
		item.RequestKey, item.Status, item.Revision, item.UpdatedAt, body, item.SubtaskID)
	return err
}

func (s *Store) GetRobotExecution(id string) (RobotExecution, error) {
	item, err := scanRobotExecution(s.db.QueryRow(`SELECT body FROM robot_executions WHERE id=?`, id))
	if err != nil {
		return RobotExecution{}, err
	}
	return s.withRobotExecutionArtifacts(item)
}

// withRobotExecutionArtifacts keeps upload state independent from lifecycle
// snapshots. Uploads write only the mapping table; every execution read derives
// its synced refs here, so a stale terminal write cannot discard a new image.
func (s *Store) withRobotExecutionArtifacts(item RobotExecution) (RobotExecution, error) {
	mappings, err := s.ListRobotArtifactMappings(item.ID)
	if err != nil {
		return RobotExecution{}, err
	}
	item.ArtifactSync = nil
	seen := make(map[string]bool, len(item.ArtifactRefs)+len(mappings))
	for _, ref := range item.ArtifactRefs {
		seen[ref] = true
	}
	for _, mapping := range mappings {
		if mapping.ExecutionID != item.ID || mapping.PilotInstanceID != item.PilotInstanceID {
			continue
		}
		item.ArtifactSync = append(item.ArtifactSync, mapping)
		if mapping.Status == "synced" && mapping.ServerArtifactID != "" {
			ref := "artifact://" + mapping.ServerArtifactID
			if !seen[ref] {
				item.ArtifactRefs = append(item.ArtifactRefs, ref)
				seen[ref] = true
			}
		}
	}
	return item, nil
}

func (s *Store) GetRobotExecutionByRequest(projectID, key string) (RobotExecution, error) {
	item, err := scanRobotExecution(s.db.QueryRow(`SELECT body FROM robot_executions WHERE project_id=? AND request_key=?`, projectID, key))
	if err != nil {
		return RobotExecution{}, err
	}
	return s.withRobotExecutionArtifacts(item)
}

func (s *Store) GetRobotExecutionBySubTask(subtaskID string) (RobotExecution, error) {
	item, err := scanRobotExecution(s.db.QueryRow(`SELECT body FROM robot_executions
		WHERE subtask_id=? ORDER BY updated_at DESC LIMIT 1`, subtaskID))
	if err != nil {
		return RobotExecution{}, err
	}
	return s.withRobotExecutionArtifacts(item)
}

func (s *Store) ListRobotExecutions(projectID, robotID string, limit int) ([]RobotExecution, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT body FROM robot_executions
		WHERE (?='' OR project_id=?) AND (?='' OR robot_id=?)
		ORDER BY updated_at DESC LIMIT ?`, projectID, projectID, robotID, robotID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]RobotExecution, 0)
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var item RobotExecution
		if err := json.Unmarshal(body, &item); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// Release the single SQLite connection before loading mapping projections.
	for index := range result {
		result[index], err = s.withRobotExecutionArtifacts(result[index])
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *Store) AppendRobotExecutionEvent(event RobotExecutionEvent) error {
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO robot_execution_events(execution_id,sequence,body)
		VALUES(?,?,?) ON CONFLICT(execution_id,sequence) DO NOTHING`, event.ExecutionID, event.Sequence, body)
	return err
}

func (s *Store) ListRobotExecutionEvents(executionID string, after int64, limit int) ([]RobotExecutionEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := s.db.Query(`SELECT body FROM robot_execution_events
		WHERE execution_id=? AND sequence>? ORDER BY sequence LIMIT ?`, executionID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]RobotExecutionEvent, 0)
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var item RobotExecutionEvent
		if err := json.Unmarshal(body, &item); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) NextRobotExecutionEventSequence(executionID string) (int64, error) {
	var next int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(sequence),0)+1 FROM robot_execution_events WHERE execution_id=?`, executionID).Scan(&next); err != nil {
		return 0, fmt.Errorf("next robot event sequence: %w", err)
	}
	return next, nil
}

func (s *Store) SaveRobotArtifactMapping(item RobotArtifactMapping) error {
	body, err := json.Marshal(item)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO robot_artifact_mappings(pilot_instance_id,local_artifact_id,server_artifact_id,status,body)
		VALUES(?,?,?,?,?) ON CONFLICT(pilot_instance_id,local_artifact_id) DO UPDATE SET
		server_artifact_id=excluded.server_artifact_id,status=excluded.status,body=excluded.body`,
		item.PilotInstanceID, item.LocalArtifactID, item.ServerArtifactID, item.Status, body)
	return err
}

func (s *Store) GetRobotArtifactMapping(pilotID, localID string) (RobotArtifactMapping, error) {
	var body []byte
	if err := s.db.QueryRow(`SELECT body FROM robot_artifact_mappings WHERE pilot_instance_id=? AND local_artifact_id=?`, pilotID, localID).Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RobotArtifactMapping{}, ErrNotFound
		}
		return RobotArtifactMapping{}, err
	}
	var item RobotArtifactMapping
	return item, json.Unmarshal(body, &item)
}

func (s *Store) ListRobotArtifactMappings(executionID string) ([]RobotArtifactMapping, error) {
	rows, err := s.db.Query(
		`SELECT body FROM robot_artifact_mappings WHERE json_extract(body,'$.execution_id')=? ORDER BY rowid`,
		executionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]RobotArtifactMapping, 0)
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var item RobotArtifactMapping
		if err := json.Unmarshal(body, &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
