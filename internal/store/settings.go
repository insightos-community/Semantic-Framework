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
	"fmt"
	"time"
)

// SettingKey 是一条服务端托管密钥记录：Name 优先为模型服务 ID
// （兼容旧端点名）
// （settings_keys 表主键），Value 为密钥明文（只在库内与内存流转，
// REST 面一律掩码展示），UpdatedAt 为最近一次写入时间（UTC）。
type SettingKey struct {
	// Name 密钥名（优先对应 LLM 模型服务 ID）。
	Name string

	// Value 密钥明文。
	Value string

	// UpdatedAt 最近一次写入时间（UTC）。
	UpdatedAt time.Time
}

// AuditEntry 是一条设置变更审计记录（settings_audit 表）：
// 只记操作人、动作与变更键清单，绝不记密钥值。
type AuditEntry struct {
	// ID 自增主键（字典序即发放序）。
	ID int64

	// UserID 操作人（auth 中间件注入的用户 ID）。
	UserID string

	// Action 动作名：settings.patch / settings.key_set / settings.key_delete。
	Action string

	// Detail 变更细节（变更键清单等，不含任何值）。
	Detail string

	// CreatedAt 记录时间（UTC）。
	CreatedAt time.Time
}

// SetKey 写入（或覆盖）一条托管密钥，updated_at 刷新为当前时间（UTC）。
func (s *Store) SetKey(name, value string) error {
	if _, err := s.db.Exec(
		`INSERT INTO settings_keys (name, key_value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET key_value = excluded.key_value, updated_at = excluded.updated_at`,
		name, value, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("写入托管密钥 %q 失败: %w", name, err)
	}
	return nil
}

// GetKey 按名读取托管密钥，不存在时返回 ErrNotFound。
func (s *Store) GetKey(name string) (string, error) {
	var value string
	err := s.db.QueryRow(
		`SELECT key_value FROM settings_keys WHERE name = ?`, name,
	).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("读取托管密钥 %q 失败: %w", name, err)
	}
	return value, nil
}

// DeleteKey 删除一条托管密钥，不存在时返回 ErrNotFound。
func (s *Store) DeleteKey(name string) error {
	res, err := s.db.Exec(`DELETE FROM settings_keys WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("删除托管密钥 %q 失败: %w", name, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认托管密钥 %q 删除结果失败: %w", name, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ListKeys 按名称升序列出全部托管密钥（含值与更新时间；
// 展示层的掩码由调用方负责，本层不做截断）。
func (s *Store) ListKeys() ([]SettingKey, error) {
	rows, err := s.db.Query(
		`SELECT name, key_value, updated_at FROM settings_keys ORDER BY name`,
	)
	if err != nil {
		return nil, fmt.Errorf("查询托管密钥清单失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var keys []SettingKey
	for rows.Next() {
		var k SettingKey
		if err := rows.Scan(&k.Name, &k.Value, &k.UpdatedAt); err != nil {
			return nil, fmt.Errorf("扫描托管密钥记录失败: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历托管密钥记录失败: %w", err)
	}
	return keys, nil
}

// InsertAudit 写入一条设置变更审计，created_at 取当前时间（UTC）。
func (s *Store) InsertAudit(e AuditEntry) error {
	if _, err := s.db.Exec(
		`INSERT INTO settings_audit (user_id, action, detail, created_at) VALUES (?, ?, ?, ?)`,
		e.UserID, e.Action, e.Detail, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("写入设置审计失败: %w", err)
	}
	return nil
}

// ListAudit 按 id 倒序（最新在前）列出最近 limit 条审计记录。
func (s *Store) ListAudit(limit int) ([]AuditEntry, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, action, detail, created_at FROM settings_audit ORDER BY id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("查询设置审计失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.UserID, &e.Action, &e.Detail, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("扫描设置审计记录失败: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历设置审计记录失败: %w", err)
	}
	return entries, nil
}
