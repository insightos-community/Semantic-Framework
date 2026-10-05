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

import "fmt"

// ListRunEventsAfter reads existing durable events for one exact Run. Project
// sequence remains the cursor; gaps are normal because other Runs share it.
// Trace-channel events have no durable sequence and retain their non-replayable
// semantics. Tool I/O is persisted on the dialogue channel.
func (s *Store) ListRunEventsAfter(projectID, runID string, afterSequence int64, limit int) ([]Event, error) {
	if projectID == "" || runID == "" || afterSequence < 0 || limit <= 0 || limit > 501 {
		return nil, ErrInvalidState
	}
	// Older envelopes may identify their Run only in parent; current tool events
	// also carry payload.run_id. Never infer ownership from conversation/trace or
	// a substring, and exclude records whose two explicit Run identities conflict.
	rows, err := s.db.Query(`WITH candidates AS (
		SELECT *,
		json_extract(CASE WHEN json_valid(payload) THEN payload ELSE '{}' END, '$.run_id') AS payload_run,
		json_extract(CASE WHEN json_valid(parent) THEN parent ELSE '{}' END, '$.run_id') AS parent_run
		FROM events WHERE project_id = ? AND sequence > ? AND channel != ?
	)
	SELECT id, project_id, session_id, resource_type, resource_id, revision, sequence,
		channel, type, importance, agent_id, agent_role, parent, payload, ts
	FROM candidates
	WHERE (payload_run = ? OR parent_run = ?)
		AND (payload_run IS NULL OR payload_run = '' OR payload_run = ?)
		AND (parent_run IS NULL OR parent_run = '' OR parent_run = ?)
	ORDER BY sequence LIMIT ?`, projectID, afterSequence, ChannelTrace,
		runID, runID, runID, runID, limit)
	if err != nil {
		return nil, fmt.Errorf("读取 Run 事件失败: %w", err)
	}
	defer rows.Close()
	result := make([]Event, 0)
	for rows.Next() {
		var item Event
		if err := rows.Scan(&item.ID, &item.ProjectID, &item.SessionID, &item.ResourceType,
			&item.ResourceID, &item.Revision, &item.Sequence, &item.Channel, &item.Type,
			&item.Importance, &item.AgentID, &item.AgentRole, &item.Parent, &item.Payload,
			&item.Ts); err != nil {
			return nil, fmt.Errorf("扫描 Run 事件失败: %w", err)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
