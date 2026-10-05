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

// User 是本地账号记录，密码只存 bcrypt 哈希，绝不存明文。
type User struct {
	// ID 用户唯一标识（usr_ 前缀 + 随机十六进制）。
	ID string

	// Username 登录名，全库唯一。
	Username string

	// PasswordHash bcrypt 算法生成的密码哈希。
	PasswordHash string

	// CreatedAt 账号创建时间。
	CreatedAt time.Time
}

// CreateUser 写入一条用户记录；username 冲突时返回错误。
func (s *Store) CreateUser(u User) error {
	if _, err := s.db.Exec(
		`INSERT INTO users (id, username, password_hash, created_at) VALUES (?, ?, ?, ?)`,
		u.ID, u.Username, u.PasswordHash, u.CreatedAt,
	); err != nil {
		return fmt.Errorf("创建用户 %q 失败: %w", u.Username, err)
	}
	return nil
}

// GetByUsername 按登录名查询用户，不存在时返回 ErrNotFound。
func (s *Store) GetByUsername(username string) (*User, error) {
	var u User
	err := s.db.QueryRow(
		`SELECT id, username, password_hash, created_at FROM users WHERE username = ?`, username,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("查询用户 %q 失败: %w", username, err)
	}
	return &u, nil
}
