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

// migrateV17 增加 v0.3 的 Workflow/Task、结构化交互和 Semantic Map。
// 这些表仍属于同一个 SQLite Store；任务与地图不会启动独立服务。
func migrateV17(tx *sql.Tx) error {
	statements := []string{
		`CREATE TABLE workflows (
			id TEXT PRIMARY KEY, project_id TEXT NOT NULL, conversation_id TEXT NOT NULL,
			goal TEXT NOT NULL, constraints_json TEXT NOT NULL DEFAULT '[]',
			criteria_json TEXT NOT NULL DEFAULT '[]', map_scope_json TEXT NOT NULL DEFAULT '{}',
			status TEXT NOT NULL, revision INTEGER NOT NULL DEFAULT 1,
			confirmed_revision INTEGER NOT NULL DEFAULT 0, created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL, ended_at TIMESTAMP)`,
		`CREATE UNIQUE INDEX idx_workflows_one_open_per_project ON workflows (project_id)
			WHERE status IN ('pending','running','paused','stopping')`,
		`CREATE INDEX idx_workflows_project_updated ON workflows (project_id, updated_at DESC)`,
		`CREATE TABLE workflow_revisions (workflow_id TEXT NOT NULL, revision INTEGER NOT NULL,
			snapshot_json TEXT NOT NULL, created_at TIMESTAMP NOT NULL,
			PRIMARY KEY (workflow_id, revision))`,
		`CREATE TABLE tasks (
			id TEXT PRIMARY KEY, workflow_id TEXT NOT NULL, position INTEGER NOT NULL,
			agent_id TEXT NOT NULL, goal TEXT NOT NULL, input_json TEXT NOT NULL DEFAULT '{}',
			criteria_json TEXT NOT NULL DEFAULT '[]', status TEXT NOT NULL,
			waiting_reason TEXT NOT NULL DEFAULT '', result_summary TEXT NOT NULL DEFAULT '',
			evidence_json TEXT NOT NULL DEFAULT '[]', context_id TEXT NOT NULL UNIQUE,
			revision INTEGER NOT NULL DEFAULT 1, created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL, ended_at TIMESTAMP)`,
		`CREATE UNIQUE INDEX idx_tasks_workflow_position ON tasks (workflow_id, position)`,
		`CREATE INDEX idx_tasks_workflow_status ON tasks (workflow_id, status, position)`,
		`CREATE TABLE task_dependencies (workflow_id TEXT NOT NULL, task_id TEXT NOT NULL,
			depends_on_task_id TEXT NOT NULL, PRIMARY KEY (task_id, depends_on_task_id))`,
		`CREATE TABLE subtasks (id TEXT PRIMARY KEY, task_id TEXT NOT NULL,
			position INTEGER NOT NULL, goal TEXT NOT NULL, status TEXT NOT NULL,
			waiting_reason TEXT NOT NULL DEFAULT '', result_json TEXT NOT NULL DEFAULT '{}',
			revision INTEGER NOT NULL DEFAULT 1, created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL, ended_at TIMESTAMP)`,
		`CREATE UNIQUE INDEX idx_subtasks_task_position ON subtasks (task_id, position)`,
		`ALTER TABLE run_sessions ADD COLUMN task_id TEXT NOT NULL DEFAULT ''`,
		`CREATE UNIQUE INDEX idx_run_context_single_active ON run_sessions (context_id)
			WHERE task_id <> '' AND
			status IN ('queued','running','waiting_input','cancelling')`,
		`CREATE INDEX idx_run_sessions_task ON run_sessions (task_id, started_at DESC)`,
		`ALTER TABLE interactions ADD COLUMN workflow_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE interactions ADD COLUMN task_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE interactions ADD COLUMN ui_kind TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE interactions ADD COLUMN source_revision INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE interactions ADD COLUMN map_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE interactions ADD COLUMN map_generation INTEGER NOT NULL DEFAULT 0`,
		`CREATE TABLE semantic_maps (id TEXT PRIMARY KEY, project_id TEXT NOT NULL,
			slot TEXT NOT NULL, generation INTEGER NOT NULL DEFAULT 1,
			revision INTEGER NOT NULL DEFAULT 1, frame_id TEXT NOT NULL DEFAULT 'world',
			created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL,
			UNIQUE (project_id, slot))`,
		`CREATE TABLE map_generations (map_id TEXT NOT NULL, generation INTEGER NOT NULL,
			reason TEXT NOT NULL DEFAULT '', created_at TIMESTAMP NOT NULL,
			PRIMARY KEY (map_id, generation))`,
		`CREATE TABLE map_entities (map_id TEXT NOT NULL, generation INTEGER NOT NULL,
			id TEXT NOT NULL, type TEXT NOT NULL, name TEXT NOT NULL, status TEXT NOT NULL,
			frame_id TEXT NOT NULL, pose_json TEXT NOT NULL DEFAULT '{}',
			bounds_json TEXT NOT NULL DEFAULT '{}', properties_json TEXT NOT NULL DEFAULT '{}',
			source TEXT NOT NULL, source_ts TIMESTAMP, evidence_json TEXT NOT NULL DEFAULT '[]',
			manual_json TEXT NOT NULL DEFAULT '{}', revision INTEGER NOT NULL DEFAULT 1,
			updated_at TIMESTAMP NOT NULL, PRIMARY KEY (map_id, generation, id))`,
		`CREATE INDEX idx_map_entities_query ON map_entities (map_id, generation, type, status)`,
		`CREATE TABLE map_relations (map_id TEXT NOT NULL, generation INTEGER NOT NULL,
			id TEXT NOT NULL, subject_id TEXT NOT NULL, predicate TEXT NOT NULL,
			object_id TEXT NOT NULL, source TEXT NOT NULL, evidence_json TEXT NOT NULL DEFAULT '[]',
			manual INTEGER NOT NULL DEFAULT 0, revision INTEGER NOT NULL DEFAULT 1,
			updated_at TIMESTAMP NOT NULL, PRIMARY KEY (map_id, generation, id))`,
		`CREATE INDEX idx_map_relations_query ON map_relations
			(map_id, generation, subject_id, predicate, object_id)`,
		`CREATE TABLE map_source_mappings (map_id TEXT NOT NULL, generation INTEGER NOT NULL,
			source TEXT NOT NULL, source_id TEXT NOT NULL, entity_id TEXT NOT NULL,
			PRIMARY KEY (map_id, generation, source, source_id))`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}
