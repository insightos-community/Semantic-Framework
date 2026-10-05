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
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/log"
)

// testLogger 返回静默日志器，避免测试输出被日志淹没。
func testLogger() *log.Logger {
	return log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
}

// openTestStore 在临时目录打开一个已迁移的 Store，测试结束自动关闭。
func openTestStore(t *testing.T) *Store {
	t.Helper()
	return openMigratedStore(t)
}

// TestOpenUnsupportedDriver 验证非 sqlite 驱动直接报错（首版仅支持 sqlite）。
func TestOpenUnsupportedDriver(t *testing.T) {
	cfg := config.StoreConfig{Driver: "postgres", SQLitePath: "x.db"}
	if _, err := Open(cfg, testLogger()); err == nil {
		t.Error("不支持的驱动应导致 Open 返回错误")
	}
}

// TestMigrateIdempotent 验证迁移可重复执行且跳过已应用版本。
func TestMigrateIdempotent(t *testing.T) {
	st := openTestStore(t)
	// openTestStore 已执行一次迁移，再次执行不应报错。
	if err := st.Migrate(); err != nil {
		t.Errorf("重复 Migrate 应幂等成功，实际返回: %v", err)
	}
}

// TestMigrateV14ConvertsLegacyRunStatuses 验证旧 Run 状态的唯一兼容入口：
// v13 数据库升级到 v14 时，done 与 awaiting_approval 被一次性改写为
// completed 与 waiting_input，升级后的运行时代码无需继续理解旧名称。
func TestMigrateV14ConvertsLegacyRunStatuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-v13.db")
	st, err := Open(config.StoreConfig{Driver: driverSQLite, SQLitePath: path}, testLogger())
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	for _, item := range migrations {
		if item.version > 13 {
			break
		}
		tx, err := st.db.Begin()
		if err != nil {
			t.Fatalf("开启 v%d 迁移事务失败: %v", item.version, err)
		}
		if err := item.apply(tx); err != nil {
			_ = tx.Rollback()
			t.Fatalf("准备 v13 数据库时执行 v%d 失败: %v", item.version, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("提交 v%d 迁移失败: %v", item.version, err)
		}
	}

	now := time.Now().UTC().Truncate(time.Second)
	if _, err := st.db.Exec(`INSERT INTO projects
		(id, owner_id, name, workspace_root, is_default, created_at, updated_at)
		VALUES ('proj-legacy', 'usr-legacy', 'Legacy', '', 1, ?, ?)`, now, now); err != nil {
		t.Fatalf("准备旧 Project 失败: %v", err)
	}
	if _, err := st.db.Exec(`INSERT INTO chat_sessions
		(id, user_id, project_id, title, created_at, updated_at)
		VALUES ('cs-legacy', 'usr-legacy', 'proj-legacy', 'Legacy', ?, ?)`, now, now); err != nil {
		t.Fatalf("准备旧 Conversation 失败: %v", err)
	}
	for id, status := range map[string]string{
		"run-legacy-done":    "done",
		"run-legacy-waiting": "awaiting_approval",
	} {
		if _, err := st.db.Exec(`INSERT INTO run_sessions
			(id, agent_name, chat_session_id, status, started_at)
			VALUES (?, 'leader', 'cs-legacy', ?, ?)`, id, status, now); err != nil {
			t.Fatalf("准备旧 Run %s 失败: %v", id, err)
		}
	}

	tx, err := st.db.Begin()
	if err != nil {
		t.Fatalf("开启 v14 迁移事务失败: %v", err)
	}
	if err := migrateV14(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("执行 v14 迁移失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交 v14 迁移失败: %v", err)
	}

	for id, want := range map[string]string{
		"run-legacy-done":    RunStatusCompleted,
		"run-legacy-waiting": RunStatusWaitingInput,
	} {
		var got string
		if err := st.db.QueryRow(`SELECT status FROM run_sessions WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("读取迁移后 Run %s 失败: %v", id, err)
		}
		if got != want {
			t.Errorf("Run %s 状态转换不符: got=%q want=%q", id, got, want)
		}
	}
}

// TestMigrateDropsObsoleteBlackboard 验证最新数据库不再暴露已经退出运行
// 主链的 Blackboard 表。历史 v7 仍会按序执行，但 v15 必须完成清理。
func TestMigrateDropsObsoleteBlackboard(t *testing.T) {
	st := openTestStore(t)

	for _, table := range []string{"blackboard_entries", "work_sessions"} {
		var count int
		err := st.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
			WHERE type = 'table' AND name = ?`, table).Scan(&count)
		if err != nil {
			t.Fatalf("检查废弃表 %s 失败: %v", table, err)
		}
		if count != 0 {
			t.Errorf("废弃表 %s 在 v15 后仍存在", table)
		}
	}

	var name string
	if err := st.db.QueryRow(`SELECT name FROM schema_migrations WHERE version = 15`).Scan(&name); err != nil {
		t.Fatalf("读取 v15 迁移记录失败: %v", err)
	}
	if name != "drop_obsolete_blackboard" {
		t.Errorf("v15 迁移名称不符: %s", name)
	}
}

// TestMigrateNormalizesReplayableEventSequences 验证旧库中夹在业务事件之间
// 的 trace 序号会被释放，可恢复事件重新形成连续序列。
func TestMigrateNormalizesReplayableEventSequences(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC()
	for _, event := range []Event{
		{ID: "evt-1", ProjectID: "proj-sequence", Sequence: 1,
			Channel: "dialogue", Type: "run.started", Parent: "{}", Payload: "{}", Ts: now},
		{ID: "evt-2", ProjectID: "proj-sequence", Sequence: 2,
			Channel: ChannelTrace, Type: "trace.sample", Parent: "{}", Payload: "{}", Ts: now},
		{ID: "evt-3", ProjectID: "proj-sequence", Sequence: 3,
			Channel: "dialogue", Type: "run.completed", Parent: "{}", Payload: "{}", Ts: now},
	} {
		if _, err := st.db.Exec(`INSERT INTO events (id, project_id, session_id,
			resource_type, resource_id, revision, sequence, channel, type, importance,
			agent_id, agent_role, parent, payload, ts)
			VALUES (?, ?, '', '', '', 0, ?, ?, ?, 'normal', 'system', '', ?, ?, ?)`,
			event.ID, event.ProjectID, event.Sequence, event.Channel, event.Type,
			event.Parent, event.Payload, event.Ts); err != nil {
			t.Fatalf("准备旧事件 %s 失败: %v", event.ID, err)
		}
	}
	if _, err := st.db.Exec(`INSERT INTO project_event_sequences
		(project_id, next_sequence) VALUES ('proj-sequence', 4)`); err != nil {
		t.Fatalf("准备旧序号游标失败: %v", err)
	}
	tx, err := st.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateV16(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("重新整理事件序号失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]int64{"evt-1": 1, "evt-2": 0, "evt-3": 2} {
		var sequence int64
		if err := st.db.QueryRow(`SELECT sequence FROM events WHERE id = ?`, id).Scan(&sequence); err != nil {
			t.Fatal(err)
		}
		if sequence != want {
			t.Errorf("事件 %s 序号不符: got=%d want=%d", id, sequence, want)
		}
	}
	inserted, err := st.InsertProjectEvent(Event{ID: "evt-4", ProjectID: "proj-sequence",
		Channel: "dialogue", Type: "message.done", Parent: "{}", Payload: "{}", Ts: now})
	if err != nil || inserted.Sequence != 3 {
		t.Fatalf("整理后新事件应从 3 继续: event=%+v err=%v", inserted, err)
	}
}

// TestUserCRUD 验证用户的创建、按名查询与不存在场景。
func TestUserCRUD(t *testing.T) {
	st := openTestStore(t)

	u := User{
		ID:           "usr_test01",
		Username:     "alice",
		PasswordHash: "hash",
		CreatedAt:    time.Now().UTC().Truncate(time.Second),
	}
	if err := st.CreateUser(u); err != nil {
		t.Fatalf("CreateUser 失败: %v", err)
	}

	got, err := st.GetByUsername("alice")
	if err != nil {
		t.Fatalf("GetByUsername 失败: %v", err)
	}
	if got.ID != u.ID || got.Username != u.Username || got.PasswordHash != u.PasswordHash {
		t.Errorf("查询结果与写入不一致: %+v", got)
	}
	if !got.CreatedAt.Equal(u.CreatedAt) {
		t.Errorf("created_at 往返不一致: 写入 %s，读出 %s", u.CreatedAt, got.CreatedAt)
	}

	// username 唯一约束：重复创建必须报错。
	if err := st.CreateUser(u); err == nil {
		t.Error("重复 username 创建应返回错误")
	}

	// 不存在的用户返回 ErrNotFound。
	if _, err := st.GetByUsername("nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("查询不存在用户应返回 ErrNotFound，实际: %v", err)
	}
}

// TestTokenCRUD 验证 token 的创建、查询、删除与过期清理。
func TestTokenCRUD(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC()

	tok := Token{
		Token:     "tok-1",
		UserID:    "usr_test01",
		ExpiresAt: now.Add(time.Hour),
		CreatedAt: now,
	}
	if err := st.CreateToken(tok); err != nil {
		t.Fatalf("CreateToken 失败: %v", err)
	}

	got, err := st.GetToken("tok-1")
	if err != nil {
		t.Fatalf("GetToken 失败: %v", err)
	}
	if got.UserID != tok.UserID || !got.ExpiresAt.Equal(tok.ExpiresAt) {
		t.Errorf("查询结果与写入不一致: %+v", got)
	}

	// 不存在的 token 返回 ErrNotFound。
	if _, err := st.GetToken("tok-x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("查询不存在 token 应返回 ErrNotFound，实际: %v", err)
	}

	// DeleteToken 幂等：删除后再次删除不报错。
	if err := st.DeleteToken("tok-1"); err != nil {
		t.Fatalf("DeleteToken 失败: %v", err)
	}
	if err := st.DeleteToken("tok-1"); err != nil {
		t.Errorf("重复 DeleteToken 应幂等成功，实际: %v", err)
	}
	if _, err := st.GetToken("tok-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除后查询应返回 ErrNotFound，实际: %v", err)
	}
}

// TestDeleteExpired 验证 DeleteExpired 只清理已过期的 token。
func TestDeleteExpired(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC()

	for _, tok := range []Token{
		{Token: "expired-1", UserID: "u1", ExpiresAt: now.Add(-time.Hour), CreatedAt: now.Add(-2 * time.Hour)},
		{Token: "expired-2", UserID: "u2", ExpiresAt: now.Add(-time.Minute), CreatedAt: now.Add(-2 * time.Hour)},
		{Token: "alive-1", UserID: "u3", ExpiresAt: now.Add(time.Hour), CreatedAt: now},
	} {
		if err := st.CreateToken(tok); err != nil {
			t.Fatalf("CreateToken(%s) 失败: %v", tok.Token, err)
		}
	}

	n, err := st.DeleteExpired(now)
	if err != nil {
		t.Fatalf("DeleteExpired 失败: %v", err)
	}
	if n != 2 {
		t.Errorf("应清理 2 条过期 token，实际: %d", n)
	}
	if _, err := st.GetToken("alive-1"); err != nil {
		t.Errorf("未过期 token 应保留，实际查询返回: %v", err)
	}
}
