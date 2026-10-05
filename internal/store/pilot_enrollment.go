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
	"time"
)

// PilotEnrollment 是用户创建的一次性设备加入凭据。加入码只用于首次 claim；
// 成功后 Pilot 改用绑定自身 ID 的长期 credential 重连。
type PilotEnrollment struct {
	ID              string     `json:"id"`
	Code            string     `json:"code,omitempty"`
	Status          string     `json:"status"`
	ExpiresAt       time.Time  `json:"expires_at"`
	CreatedBy       string     `json:"created_by"`
	PilotInstanceID string     `json:"pilot_instance_id,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	ClaimedAt       *time.Time `json:"claimed_at,omitempty"`
}

type PilotCredential struct {
	PilotInstanceID string
	Credential      string
	CreatedAt       time.Time
	RevokedAt       *time.Time
}

type RobotDesiredSkill struct {
	RobotID   string    `json:"robot_id"`
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	Enabled   bool      `json:"enabled"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Store) SavePilotEnrollment(item PilotEnrollment) error {
	_, err := s.db.Exec(`INSERT INTO pilot_enrollments
		(id,code,status,expires_at,created_by,pilot_instance_id,created_at,claimed_at)
		VALUES(?,?,?,?,?,?,?,?)`, item.ID, item.Code, item.Status, item.ExpiresAt,
		item.CreatedBy, item.PilotInstanceID, item.CreatedAt, item.ClaimedAt)
	return err
}

func (s *Store) GetPilotEnrollment(id string) (PilotEnrollment, error) {
	return scanPilotEnrollment(s.db.QueryRow(`SELECT id,code,status,expires_at,created_by,
		pilot_instance_id,created_at,claimed_at FROM pilot_enrollments WHERE id=?`, id))
}

func scanPilotEnrollment(row *sql.Row) (PilotEnrollment, error) {
	var item PilotEnrollment
	var claimed sql.NullTime
	if err := row.Scan(&item.ID, &item.Code, &item.Status, &item.ExpiresAt, &item.CreatedBy,
		&item.PilotInstanceID, &item.CreatedAt, &claimed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return item, ErrNotFound
		}
		return item, err
	}
	if claimed.Valid {
		item.ClaimedAt = &claimed.Time
	}
	return item, nil
}

// ClaimPilotEnrollment 在一个事务里消费加入码并保存专用 credential，确保并发
// claim 只能有一个成功，也不会出现加入码已消耗但凭据没有落库的半状态。
func (s *Store) ClaimPilotEnrollment(code, pilotID, credential string, now time.Time) (PilotEnrollment, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return PilotEnrollment{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var item PilotEnrollment
	var claimed sql.NullTime
	if err := tx.QueryRow(`SELECT id,code,status,expires_at,created_by,pilot_instance_id,
		created_at,claimed_at FROM pilot_enrollments WHERE code=?`, code).Scan(
		&item.ID, &item.Code, &item.Status, &item.ExpiresAt, &item.CreatedBy,
		&item.PilotInstanceID, &item.CreatedAt, &claimed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return item, ErrNotFound
		}
		return item, err
	}
	if item.Status != "pending" || !item.ExpiresAt.After(now) {
		return item, ErrRevisionConflict
	}
	result, err := tx.Exec(`UPDATE pilot_enrollments SET status="claimed",
		pilot_instance_id=?,claimed_at=? WHERE id=? AND status="pending"`, pilotID, now, item.ID)
	if err != nil {
		return item, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return item, ErrRevisionConflict
	}
	if _, err := tx.Exec(`INSERT INTO pilot_credentials
		(pilot_instance_id,credential,created_at,revoked_at) VALUES(?,?,?,NULL)
		ON CONFLICT(pilot_instance_id) DO UPDATE SET credential=excluded.credential,
		created_at=excluded.created_at,revoked_at=NULL`, pilotID, credential, now); err != nil {
		return item, err
	}
	if err := tx.Commit(); err != nil {
		return item, err
	}
	item.Status, item.PilotInstanceID, item.ClaimedAt = "claimed", pilotID, &now
	return item, nil
}

func (s *Store) RevokePilotEnrollment(id string) error {
	result, err := s.db.Exec(`UPDATE pilot_enrollments SET status="revoked"
		WHERE id=? AND status="pending"`, id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetPilotCredential(credential string) (PilotCredential, error) {
	var item PilotCredential
	var revoked sql.NullTime
	err := s.db.QueryRow(`SELECT pilot_instance_id,credential,created_at,revoked_at
		FROM pilot_credentials WHERE credential=?`, credential).Scan(
		&item.PilotInstanceID, &item.Credential, &item.CreatedAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return item, ErrNotFound
	}
	if err != nil {
		return item, err
	}
	if revoked.Valid {
		item.RevokedAt = &revoked.Time
	}
	return item, nil
}

func (s *Store) SaveRobotDesiredSkill(item RobotDesiredSkill) error {
	_, err := s.db.Exec(`INSERT INTO robot_desired_skills(robot_id,name,version,enabled,updated_at)
		VALUES(?,?,?,?,?) ON CONFLICT(robot_id,name) DO UPDATE SET
		version=excluded.version,enabled=excluded.enabled,updated_at=excluded.updated_at`,
		item.RobotID, item.Name, item.Version, item.Enabled, item.UpdatedAt)
	return err
}

func (s *Store) DeleteRobotDesiredSkill(robotID, name string) error {
	_, err := s.db.Exec(`DELETE FROM robot_desired_skills WHERE robot_id=? AND name=?`, robotID, name)
	return err
}

func (s *Store) ListRobotDesiredSkills(robotID string) ([]RobotDesiredSkill, error) {
	rows, err := s.db.Query(`SELECT robot_id,name,version,enabled,updated_at
		FROM robot_desired_skills WHERE robot_id=? ORDER BY name`, robotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]RobotDesiredSkill, 0)
	for rows.Next() {
		var item RobotDesiredSkill
		if err := rows.Scan(&item.RobotID, &item.Name, &item.Version, &item.Enabled, &item.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
