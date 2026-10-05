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

const (
	// ExecutionModeAsk 表示执行工具每次都需要用户批准。
	ExecutionModeAsk = "ask"

	// ExecutionModeAuto 表示 Docker 执行可自动批准，宿主执行仍需批准。
	ExecutionModeAuto = "auto"

	// ExecutionModeFull 表示本会话跳过普通执行审批；宿主执行仍受服务端
	// 总开关和会话显式开关约束。
	ExecutionModeFull = "full"
)

// SessionExecutionPolicy 是单个会话的最小执行策略。该策略不承载复杂权限
// 指纹或版本授权，避免恢复已经废弃的 v0.1.3 权限系统。
type SessionExecutionPolicy struct {
	SessionID            string    `json:"session_id"`
	Mode                 string    `json:"mode"`
	HostExecutionEnabled bool      `json:"host_execution_enabled"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// GetSessionExecutionPolicy 读取会话策略；尚未显式配置时返回 ask/false，
// 不为了只读查询额外写数据库。
func (s *Store) GetSessionExecutionPolicy(sessionID string) (SessionExecutionPolicy, error) {
	policy := SessionExecutionPolicy{SessionID: sessionID, Mode: ExecutionModeAsk}
	var enabled int
	err := s.db.QueryRow(`SELECT mode, host_execution_enabled, updated_at
		FROM session_execution_policies WHERE session_id = ?`, sessionID).Scan(
		&policy.Mode, &enabled, &policy.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return policy, nil
	}
	if err != nil {
		return SessionExecutionPolicy{}, fmt.Errorf("查询会话 %q 执行策略失败: %w", sessionID, err)
	}
	policy.HostExecutionEnabled = enabled != 0
	return policy, nil
}

// SetSessionExecutionPolicy 原子写入当前会话策略。调用方必须先完成会话归属
// 和忙碌状态校验，存储层只校验稳定枚举。
func (s *Store) SetSessionExecutionPolicy(policy SessionExecutionPolicy) (SessionExecutionPolicy, error) {
	if policy.SessionID == "" {
		return SessionExecutionPolicy{}, fmt.Errorf("session_id 不能为空")
	}
	if !ValidExecutionMode(policy.Mode) {
		return SessionExecutionPolicy{}, fmt.Errorf("执行模式仅支持 ask/auto/full")
	}
	policy.UpdatedAt = time.Now().UTC()
	enabled := 0
	if policy.HostExecutionEnabled {
		enabled = 1
	}
	if _, err := s.db.Exec(`INSERT INTO session_execution_policies
		(session_id, mode, host_execution_enabled, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET mode = excluded.mode,
		host_execution_enabled = excluded.host_execution_enabled,
		updated_at = excluded.updated_at`, policy.SessionID, policy.Mode, enabled,
		policy.UpdatedAt); err != nil {
		return SessionExecutionPolicy{}, fmt.Errorf("保存会话 %q 执行策略失败: %w", policy.SessionID, err)
	}
	return policy, nil
}

// ValidExecutionMode 判断字符串是否属于当前支持的最小执行模式。
func ValidExecutionMode(mode string) bool {
	return mode == ExecutionModeAsk || mode == ExecutionModeAuto || mode == ExecutionModeFull
}
