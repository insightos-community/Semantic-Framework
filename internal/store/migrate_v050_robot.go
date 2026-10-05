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

import "database/sql"

// migrateV22 保存 Server 视角的 Pilot、Robot Skill、Robot Execution 与
// Artifact 映射。二进制内容仍在各自文件空间，SQLite 只保存索引和状态。
func migrateV22(tx *sql.Tx) error {
	statements := []string{
		`CREATE TABLE robot_skill_packages (
			name TEXT NOT NULL, version TEXT NOT NULL, body BLOB NOT NULL,
			PRIMARY KEY(name, version))`,
		`CREATE TABLE robot_pilots (
			pilot_instance_id TEXT PRIMARY KEY, robot_id TEXT NOT NULL,
			status TEXT NOT NULL, last_seen_at TIMESTAMP NOT NULL, body BLOB NOT NULL)`,
		`CREATE INDEX idx_robot_pilots_robot ON robot_pilots(robot_id, last_seen_at)`,
		`CREATE TABLE robot_pilot_skills (
			pilot_instance_id TEXT NOT NULL, name TEXT NOT NULL, version TEXT NOT NULL,
			enabled INTEGER NOT NULL, body BLOB NOT NULL,
			PRIMARY KEY(pilot_instance_id, name, version))`,
		`CREATE TABLE robot_executions (
			id TEXT PRIMARY KEY, project_id TEXT NOT NULL, robot_id TEXT NOT NULL,
			request_key TEXT NOT NULL, status TEXT NOT NULL, revision INTEGER NOT NULL,
			updated_at TIMESTAMP NOT NULL, body BLOB NOT NULL,
			UNIQUE(project_id, request_key))`,
		`CREATE INDEX idx_robot_executions_robot ON robot_executions(robot_id, updated_at)`,
		`CREATE TABLE robot_execution_events (
			execution_id TEXT NOT NULL, sequence INTEGER NOT NULL, body BLOB NOT NULL,
			PRIMARY KEY(execution_id, sequence))`,
		`CREATE TABLE robot_artifact_mappings (
			pilot_instance_id TEXT NOT NULL, local_artifact_id TEXT NOT NULL,
			server_artifact_id TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, body BLOB NOT NULL,
			PRIMARY KEY(pilot_instance_id, local_artifact_id))`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}
