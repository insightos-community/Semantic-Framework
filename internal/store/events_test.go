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
	"testing"
	"time"
)

// newTestEvent 构造一条测试事件（id 由调用方指定，其余字段带可区分默认值）。
func newTestEvent(id, sessionID, channel string) Event {
	return Event{
		ID: id, SessionID: sessionID, Channel: channel, Type: "test.type",
		Importance: "normal", AgentID: "leader", AgentRole: "coordinator",
		Parent: `{"run_id":"run-1"}`, Payload: `{"n":1}`,
		Ts: time.Now().UTC(),
	}
}

// TestEventInsertAndListAfter 验证事件落库与按游标分页查询：
// 字典序游标、会话隔离、limit 分页、trace 频道排除。
func TestEventInsertAndListAfter(t *testing.T) {
	st := openTestStore(t)

	// cs-1 五条事件（含一条 trace），cs-2 一条（验证会话隔离）。
	for _, ev := range []Event{
		newTestEvent("evt-0001-a", "cs-1", "dialogue"),
		newTestEvent("evt-0002-b", "cs-1", "dialogue"),
		newTestEvent("evt-0003-c", "cs-1", "trace"),
		newTestEvent("evt-0004-d", "cs-1", "interaction"),
		newTestEvent("evt-0005-e", "cs-1", "dialogue"),
		newTestEvent("evt-0006-f", "cs-2", "dialogue"),
	} {
		if err := st.InsertEvent(ev); err != nil {
			t.Fatalf("InsertEvent(%s) 失败: %v", ev.ID, err)
		}
	}

	// 全量回放（空游标）：trace 被排除，cs-2 不可见，按 id 升序。
	all, err := st.ListEventsAfter("cs-1", "", 0)
	if err != nil {
		t.Fatalf("ListEventsAfter 失败: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("应返回 4 条（trace 排除、cs-2 隔离），实际: %d（%+v）", len(all), all)
	}
	wantIDs := []string{"evt-0001-a", "evt-0002-b", "evt-0004-d", "evt-0005-e"}
	for i, ev := range all {
		if ev.ID != wantIDs[i] {
			t.Errorf("第 %d 条应为 %q，实际: %q", i, wantIDs[i], ev.ID)
		}
	}
	if all[0].Parent != `{"run_id":"run-1"}` || all[0].Payload != `{"n":1}` ||
		all[0].AgentID != "leader" || all[0].Ts.IsZero() {
		t.Errorf("事件字段往返不一致: %+v", all[0])
	}

	// 游标续传：只返回 id 更晚的事件。
	after, err := st.ListEventsAfter("cs-1", "evt-0002-b", 0)
	if err != nil {
		t.Fatalf("ListEventsAfter(游标) 失败: %v", err)
	}
	if len(after) != 2 || after[0].ID != "evt-0004-d" || after[1].ID != "evt-0005-e" {
		t.Errorf("游标续传结果不符: %+v", after)
	}

	// limit 分页：第一页 2 条，以页尾 id 为游标取第二页。
	page1, err := st.ListEventsAfter("cs-1", "", 2)
	if err != nil {
		t.Fatalf("ListEventsAfter(分页) 失败: %v", err)
	}
	if len(page1) != 2 || page1[1].ID != "evt-0002-b" {
		t.Fatalf("第一页不符: %+v", page1)
	}
	page2, err := st.ListEventsAfter("cs-1", page1[len(page1)-1].ID, 2)
	if err != nil {
		t.Fatalf("ListEventsAfter(第二页) 失败: %v", err)
	}
	if len(page2) != 2 || page2[0].ID != "evt-0004-d" {
		t.Errorf("第二页不符: %+v", page2)
	}

	// 末尾游标：无后续事件，返回空（重复 sync 的幂等基础）。
	tail, err := st.ListEventsAfter("cs-1", "evt-0005-e", 0)
	if err != nil {
		t.Fatalf("ListEventsAfter(末尾) 失败: %v", err)
	}
	if len(tail) != 0 {
		t.Errorf("末尾游标应返回空，实际: %+v", tail)
	}
}

// TestEventInsertDuplicate 验证重复 id 落库显性报错（发放单调性被破坏的信号）。
func TestEventInsertDuplicate(t *testing.T) {
	st := openTestStore(t)
	ev := newTestEvent("evt-dup-1", "cs-1", "dialogue")
	if err := st.InsertEvent(ev); err != nil {
		t.Fatalf("首次 InsertEvent 失败: %v", err)
	}
	if err := st.InsertEvent(ev); err == nil {
		t.Error("重复 id 落库应返回错误")
	}
}

// TestProjectEventWriteWhileExternalReaderIsOpen 复现真实模型联调暴露的边界：
// 外部诊断程序读取 semantic.db 时会持有独立读事务，流式模型事件仍必须能够
// 分配 Project sequence 并提交。该测试验证的是 WAL 的实际行为，而不是只检查
// PRAGMA 字符串，防止以后调整连接初始化时重新引入 SQLITE_BUSY。
func TestProjectEventWriteWhileExternalReaderIsOpen(t *testing.T) {
	st := openTestStore(t)
	project, err := st.CreateProject("usr-event-reader", "事件并发读取")
	if err != nil {
		t.Fatalf("CreateProject 失败: %v", err)
	}

	reader, err := sql.Open(driverSQLite, st.databasePath)
	if err != nil {
		t.Fatalf("打开外部只读连接失败: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	readTx, err := reader.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("开启外部读事务失败: %v", err)
	}
	defer func() { _ = readTx.Rollback() }()
	var projectCount int
	if err := readTx.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&projectCount); err != nil {
		t.Fatalf("建立外部读快照失败: %v", err)
	}

	event := newTestEvent("evt-external-reader", "", "dialogue")
	event.ProjectID = project.ID
	written, err := st.InsertProjectEvent(event)
	if err != nil {
		t.Fatalf("外部读事务存在时写入事件失败: %v", err)
	}
	if written.Sequence != 1 {
		t.Fatalf("首条可回放事件 sequence 应为 1，实际为 %d", written.Sequence)
	}
	if err := readTx.Commit(); err != nil {
		t.Fatalf("提交外部读事务失败: %v", err)
	}

	next := newTestEvent("evt-after-external-reader", "", "dialogue")
	next.ProjectID = project.ID
	written, err = st.InsertProjectEvent(next)
	if err != nil {
		t.Fatalf("外部读事务结束后继续写入失败: %v", err)
	}
	if written.Sequence != 2 {
		t.Fatalf("事件 sequence 应连续为 2，实际为 %d", written.Sequence)
	}
}
