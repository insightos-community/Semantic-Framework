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
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"insightos.cn/semantic-framework/internal/robotruntime"
)

func (s *Store) SaveRuntimeInstance(ctx context.Context, item robotruntime.RuntimeInstance) error {
	body, err := json.Marshal(item)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO robot_runtime_instances("+
		"instance_id,robot_id,status,revision,updated_at,body) VALUES(?,?,?,?,?,?) "+
		"ON CONFLICT(instance_id) DO UPDATE SET robot_id=excluded.robot_id,status=excluded.status,"+
		"revision=excluded.revision,updated_at=excluded.updated_at,body=excluded.body",
		item.InstanceID, item.RobotID, item.Status, item.Revision, item.UpdatedAt, body)
	return err
}

func (s *Store) GetRuntimeInstance(ctx context.Context, id string) (robotruntime.RuntimeInstance, error) {
	return scanRuntimeInstance(s.db.QueryRowContext(ctx,
		"SELECT body FROM robot_runtime_instances WHERE instance_id=?", id))
}

func (s *Store) GetActiveRuntimeByRobot(ctx context.Context, robotID string) (robotruntime.RuntimeInstance, error) {
	return scanRuntimeInstance(s.db.QueryRowContext(ctx,
		"SELECT body FROM robot_runtime_instances WHERE robot_id=? AND "+
			"status IN ('starting','ready','degraded','stopping','interrupted') "+
			"ORDER BY updated_at DESC LIMIT 1", robotID))
}

func (s *Store) GetLatestRuntimeByRobot(ctx context.Context, robotID string) (robotruntime.RuntimeInstance, error) {
	return scanRuntimeInstance(s.db.QueryRowContext(ctx,
		"SELECT body FROM robot_runtime_instances WHERE robot_id=? ORDER BY updated_at DESC LIMIT 1", robotID))
}

func scanRuntimeInstance(row *sql.Row) (robotruntime.RuntimeInstance, error) {
	var body []byte
	if err := row.Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return robotruntime.RuntimeInstance{}, robotruntime.ErrInstanceNotFound
		}
		return robotruntime.RuntimeInstance{}, err
	}
	var item robotruntime.RuntimeInstance
	if err := json.Unmarshal(body, &item); err != nil {
		return robotruntime.RuntimeInstance{}, err
	}
	return item, nil
}

func (s *Store) ListRuntimeInstances(ctx context.Context) ([]robotruntime.RuntimeInstance, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT body FROM robot_runtime_instances ORDER BY updated_at DESC,instance_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]robotruntime.RuntimeInstance, 0)
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var item robotruntime.RuntimeInstance
		if err := json.Unmarshal(body, &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) AcquireAbilityFrameworkPort(ctx context.Context, instanceID string, first, last int) (int, error) {
	if instanceID == "" || first <= 0 || last < first {
		return 0, robotruntime.ErrPortUnavailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var existing int
	if err := tx.QueryRowContext(ctx,
		"SELECT port FROM robot_runtime_port_leases WHERE instance_id=?", instanceID).Scan(&existing); err == nil {
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	for candidate := first; candidate <= last; candidate++ {
		var used int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(1) FROM robot_runtime_port_leases WHERE port=?", candidate).Scan(&used); err != nil {
			return 0, err
		}
		if used != 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO robot_runtime_port_leases(instance_id,port) VALUES(?,?)", instanceID, candidate); err != nil {
			return 0, err
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return candidate, nil
	}
	return 0, robotruntime.ErrPortUnavailable
}

func (s *Store) ReleaseAbilityFrameworkPort(ctx context.Context, instanceID string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM robot_runtime_port_leases WHERE instance_id=?", instanceID)
	return err
}
