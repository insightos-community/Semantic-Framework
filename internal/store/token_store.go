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

// Token 是访问令牌记录，由 auth 模块签发与校验。
type Token struct {
	// Token 令牌串（64 字符十六进制），作为主键。
	Token string

	// UserID 令牌所属用户 ID。
	UserID string

	// ExpiresAt 过期时间，过期令牌在校验时剔除。
	ExpiresAt time.Time

	// CreatedAt 签发时间。
	CreatedAt time.Time
}

// CreateToken 写入一条令牌记录。
func (s *Store) CreateToken(t Token) error {
	if _, err := s.db.Exec(
		`INSERT INTO tokens (token, user_id, expires_at, created_at) VALUES (?, ?, ?, ?)`,
		t.Token, t.UserID, t.ExpiresAt, t.CreatedAt,
	); err != nil {
		return fmt.Errorf("写入 token 失败: %w", err)
	}
	return nil
}

// GetToken 查询令牌记录，不存在时返回 ErrNotFound。
func (s *Store) GetToken(token string) (*Token, error) {
	var t Token
	err := s.db.QueryRow(
		`SELECT token, user_id, expires_at, created_at FROM tokens WHERE token = ?`, token,
	).Scan(&t.Token, &t.UserID, &t.ExpiresAt, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("查询 token 失败: %w", err)
	}
	return &t, nil
}

// DeleteToken 删除指定令牌，令牌不存在时不视为错误（登出/换发幂等）。
func (s *Store) DeleteToken(token string) error {
	if _, err := s.db.Exec(`DELETE FROM tokens WHERE token = ?`, token); err != nil {
		return fmt.Errorf("删除 token 失败: %w", err)
	}
	return nil
}

// DeleteExpired 删除所有早于 now 过期的令牌，返回删除条数。
func (s *Store) DeleteExpired(now time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM tokens WHERE expires_at < ?`, now)
	if err != nil {
		return 0, fmt.Errorf("清理过期 token 失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清理条数失败: %w", err)
	}
	return n, nil
}
