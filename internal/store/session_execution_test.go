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
	"testing"
	"time"
)

// TestSessionExecutionPolicy 验证新会话默认 ask/false、显式策略可持久化，
// 删除会话后策略同步清理。
func TestSessionExecutionPolicy(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC()
	session := ChatSession{ID: "cs-execution", UserID: "usr-execution", Title: "执行测试",
		CreatedAt: now, UpdatedAt: now}
	if err := st.CreateChatSession(session); err != nil {
		t.Fatalf("CreateChatSession 失败: %v", err)
	}

	policy, err := st.GetSessionExecutionPolicy(session.ID)
	if err != nil || policy.Mode != ExecutionModeAsk || policy.HostExecutionEnabled {
		t.Fatalf("默认策略应为 ask/false: policy=%+v err=%v", policy, err)
	}
	policy, err = st.SetSessionExecutionPolicy(SessionExecutionPolicy{
		SessionID: session.ID, Mode: ExecutionModeFull, HostExecutionEnabled: true,
	})
	if err != nil || policy.Mode != ExecutionModeFull || !policy.HostExecutionEnabled || policy.UpdatedAt.IsZero() {
		t.Fatalf("保存策略失败: policy=%+v err=%v", policy, err)
	}
	loaded, err := st.GetSessionExecutionPolicy(session.ID)
	if err != nil || loaded.Mode != ExecutionModeFull || !loaded.HostExecutionEnabled {
		t.Fatalf("读取策略不一致: policy=%+v err=%v", loaded, err)
	}
	if _, err := st.SetSessionExecutionPolicy(SessionExecutionPolicy{
		SessionID: session.ID, Mode: "unsafe",
	}); err == nil {
		t.Fatal("非法执行模式应被拒绝")
	}

	if err := st.DeleteChatSession(session.ID); err != nil {
		t.Fatalf("DeleteChatSession 失败: %v", err)
	}
	var count int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM session_execution_policies WHERE session_id = ?`,
		session.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("会话删除后不应残留执行策略: count=%d err=%v", count, err)
	}
}
