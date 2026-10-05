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

// migrateV18 完成 Task Context、Run 归属、持久 Interaction 路由与 Map
// generation 元数据。v17 已经落地的表不改写，只追加兼容迁移。
func migrateV18(tx *sql.Tx) error {
	statements := []string{
		`ALTER TABLE workflows ADD COLUMN reason TEXT NOT NULL DEFAULT 'planning'`,
		`ALTER TABLE workflows ADD COLUMN started_at TIMESTAMP`,
		`ALTER TABLE run_sessions ADD COLUMN workflow_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE run_sessions ADD COLUMN kind TEXT NOT NULL DEFAULT 'conversation'`,
		`CREATE INDEX idx_run_sessions_workflow ON run_sessions (workflow_id, started_at DESC)`,
		`CREATE TABLE context_summaries_v030 (
			context_id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			task_id TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL DEFAULT '',
			covered_through_message_id TEXT NOT NULL DEFAULT '',
			revision INTEGER NOT NULL DEFAULT 1,
			updated_at TIMESTAMP NOT NULL
		)`,
		`INSERT INTO context_summaries_v030
			(context_id, project_id, session_id, summary, covered_through_message_id, revision, updated_at)
			SELECT context_id, project_id, session_id, summary,
			covered_through_message_id, revision, updated_at FROM context_summaries`,
		`DROP TABLE context_summaries`,
		`ALTER TABLE context_summaries_v030 RENAME TO context_summaries`,
		`CREATE INDEX idx_context_summaries_project ON context_summaries (project_id, updated_at)`,
		`CREATE INDEX idx_context_summaries_session ON context_summaries (session_id, updated_at)`,
		`CREATE TABLE context_messages (
			id TEXT PRIMARY KEY,
			context_id TEXT NOT NULL,
			project_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			task_id TEXT NOT NULL,
			agent_id TEXT NOT NULL DEFAULT '',
			run_id TEXT NOT NULL DEFAULT '',
			trace_id TEXT NOT NULL DEFAULT '',
			message_json TEXT NOT NULL,
			artifact_refs TEXT NOT NULL DEFAULT '[]',
			metadata TEXT NOT NULL DEFAULT '{}',
			created_at TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX idx_context_messages_context ON context_messages (context_id, id)`,
		`ALTER TABLE interactions ADD COLUMN target_agent_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE interactions ADD COLUMN response_schema TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE interactions ADD COLUMN expires_at TIMESTAMP`,
		`ALTER TABLE interactions ADD COLUMN cancelled_at TIMESTAMP`,
		`ALTER TABLE interactions ADD COLUMN handled_at TIMESTAMP`,
		`ALTER TABLE interactions ADD COLUMN handling_error TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE map_generations ADD COLUMN revision INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE map_generations ADD COLUMN closed_at TIMESTAMP`,
		`INSERT OR IGNORE INTO semantic_maps
			(id, project_id, slot, generation, revision, frame_id, created_at, updated_at)
			SELECT 'map-' || id || '-simulation', id, 'simulation_map', 1, 0, 'world', created_at, updated_at
			FROM projects`,
		`INSERT OR IGNORE INTO semantic_maps
			(id, project_id, slot, generation, revision, frame_id, created_at, updated_at)
			SELECT 'map-' || id || '-real', id, 'real_map', 1, 0, 'world', created_at, updated_at
			FROM projects`,
		`INSERT OR IGNORE INTO map_generations (map_id, generation, reason, created_at, revision)
			SELECT id, generation, 'initial', created_at, revision FROM semantic_maps`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}
