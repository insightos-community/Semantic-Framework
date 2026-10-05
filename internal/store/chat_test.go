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
	"fmt"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
)

// newChatSession 构造测试用会话（时间固定便于断言）。
func newChatSession(id, userID, title string) ChatSession {
	ts := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	return ChatSession{ID: id, UserID: userID, Title: title, CreatedAt: ts, UpdatedAt: ts}
}

// TestChatSessionCRUD 验证会话的创建/查询/按用户列表/活动时间/删除。
func TestChatSessionCRUD(t *testing.T) {
	st := openTestStore(t)

	if err := st.CreateChatSession(newChatSession("cs-1", "usr-1", "第一个会话")); err != nil {
		t.Fatalf("CreateChatSession 失败: %v", err)
	}
	if err := st.CreateChatSession(newChatSession("cs-2", "usr-1", "第二个会话")); err != nil {
		t.Fatalf("CreateChatSession 失败: %v", err)
	}
	if err := st.CreateChatSession(newChatSession("cs-3", "usr-2", "别人的会话")); err != nil {
		t.Fatalf("CreateChatSession 失败: %v", err)
	}

	// Get：存在返回记录，不存在返回 ErrNotFound。
	sess, err := st.GetChatSession("cs-1")
	if err != nil {
		t.Fatalf("GetChatSession 失败: %v", err)
	}
	if sess.UserID != "usr-1" || sess.Title != "第一个会话" {
		t.Errorf("会话内容不符: %+v", sess)
	}
	if _, err := st.GetChatSession("cs-x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("查询不存在的会话应返回 ErrNotFound，实际: %v", err)
	}

	// ListByUser：只含本人会话；Touch 后该会话排在最前。
	later := time.Date(2026, 8, 3, 11, 0, 0, 0, time.UTC)
	if err := st.TouchChatSession("cs-1", later); err != nil {
		t.Fatalf("TouchChatSession 失败: %v", err)
	}
	sessions, err := st.ListChatSessionsByUser("usr-1")
	if err != nil {
		t.Fatalf("ListChatSessionsByUser 失败: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("usr-1 应有 2 个会话，实际: %d", len(sessions))
	}
	if sessions[0].ID != "cs-1" || !sessions[0].UpdatedAt.Equal(later) {
		t.Errorf("列表应按 updated_at 倒序，实际: %+v", sessions)
	}
	if err := st.UpdateChatSessionTitle("cs-1", "自动生成的标题"); err != nil {
		t.Fatalf("UpdateChatSessionTitle 失败: %v", err)
	}
	renamed, err := st.GetChatSession("cs-1")
	if err != nil || renamed.Title != "自动生成的标题" {
		t.Fatalf("标题更新后内容不符: %+v, %v", renamed, err)
	}

	// Delete：会话与消息一并删除，重复删除幂等。
	if err := st.AppendChatMessage(ChatMessage{
		ID: NewChatMessageID(), SessionID: "cs-1", Message: schema.UserMessage("你好"),
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("AppendChatMessage 失败: %v", err)
	}
	if err := st.DeleteChatSession("cs-1"); err != nil {
		t.Fatalf("DeleteChatSession 失败: %v", err)
	}
	if _, err := st.GetChatSession("cs-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除后查询应返回 ErrNotFound，实际: %v", err)
	}
	msgs, err := st.ListChatMessages("cs-1", 0, 0)
	if err != nil {
		t.Fatalf("ListChatMessages 失败: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("删除后消息应一并清除，实际: %d 条", len(msgs))
	}
	if err := st.DeleteChatSession("cs-1"); err != nil {
		t.Errorf("重复删除应幂等，实际: %v", err)
	}
}

// TestChatMessages 验证消息追加、按 id 排序查询、分页与计数。
func TestChatMessages(t *testing.T) {
	st := openTestStore(t)
	if err := st.CreateChatSession(newChatSession("cs-1", "usr-1", "会话")); err != nil {
		t.Fatalf("CreateChatSession 失败: %v", err)
	}

	contents := []string{"第一条", "第二条", "第三条"}
	for index, c := range contents {
		message := ChatMessage{
			ID: NewChatMessageID(), SessionID: "cs-1", Message: schema.UserMessage(c),
			CreatedAt: time.Now().UTC(),
		}
		if index == 0 {
			message.AgentID, message.RunID, message.TraceID = "query-1", "run-1", "trace-1"
			message.Provider, message.Endpoint, message.Model = "minimax", "minimax-m3", "MiniMax-M3"
			message.ArtifactRefs = []string{"art-image-1", "art-report-1"}
		}
		if err := st.AppendChatMessage(message); err != nil {
			t.Fatalf("AppendChatMessage 失败: %v", err)
		}
	}

	// 全量查询：按 id 升序（时间序），内容与写入顺序一致。
	msgs, err := st.ListChatMessages("cs-1", 0, 0)
	if err != nil {
		t.Fatalf("ListChatMessages 失败: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("应有 3 条消息，实际: %d", len(msgs))
	}
	for i, c := range contents {
		if msgs[i].Message == nil || msgs[i].Message.Content != c {
			t.Errorf("第 %d 条消息应为 %q，实际: %+v", i, c, msgs[i].Message)
		}
	}
	if first := msgs[0]; first.AgentID != "query-1" || first.RunID != "run-1" ||
		first.TraceID != "trace-1" || first.Provider != "minimax" ||
		first.Endpoint != "minimax-m3" || first.Model != "MiniMax-M3" ||
		len(first.ArtifactRefs) != 2 || first.ArtifactRefs[0] != "art-image-1" {
		t.Errorf("消息薄信封字段恢复不符: %+v", first)
	}
	if err := st.AppendChatMessage(ChatMessage{
		ID: NewChatMessageID(), SessionID: "cs-1", Message: schema.AssistantMessage("", nil),
		CreatedAt: time.Now().UTC(),
	}); err == nil {
		t.Error("空 Assistant 消息必须被拒绝")
	}

	// 分页查询：limit/offset 生效。
	page, err := st.ListChatMessages("cs-1", 2, 1)
	if err != nil {
		t.Fatalf("分页查询失败: %v", err)
	}
	if len(page) != 2 || page[0].Message.Content != "第二条" || page[1].Message.Content != "第三条" {
		t.Errorf("分页结果不符: %+v", page)
	}

	total, err := st.CountChatMessages("cs-1")
	if err != nil {
		t.Fatalf("CountChatMessages 失败: %v", err)
	}
	if total != 3 {
		t.Errorf("消息总数应为 3，实际: %d", total)
	}
}

func TestHistoricalAssistantRetainsRunStartAndWriteTime(t *testing.T) {
	st := openTestStore(t)
	if err := st.CreateChatSession(newChatSession("cs-timeline", "usr-1", "timeline")); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Add(-time.Minute)
	written := started.Add(50 * time.Second)
	if err := st.CreateRunSession(RunSession{ID: "run-timeline", ChatSessionID: "cs-timeline", AgentID: "leader", Status: RunStatusCompleted, StartedAt: started}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendChatMessage(ChatMessage{ID: NewChatMessageID(), SessionID: "cs-timeline", RunID: "run-timeline", Message: schema.AssistantMessage("answer", nil), CreatedAt: written}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListChatMessages("cs-timeline", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].StartedAt == nil || !rows[0].StartedAt.Equal(started) || !rows[0].CreatedAt.Equal(written) {
		t.Fatalf("lost timeline/audit timestamps: %+v", rows)
	}
}

// TestMigrateV28NormalizesWorkflowActivityRole 验证升级旧数据库时，只把
// workflow_started 产品活动改成 Assistant；其他 system 消息不被误改。
func TestMigrateV28NormalizesWorkflowActivityRole(t *testing.T) {
	st := openTestStore(t)
	if err := st.CreateChatSession(newChatSession("cs-v28", "usr-v28", "旧计划会话")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, message := range []ChatMessage{
		{ID: "msg-v28-workflow", SessionID: "cs-v28", Message: schema.SystemMessage("Workflow 已开始"),
			Metadata: `{"message_kind":"workflow_started"}`, CreatedAt: now},
		{ID: "msg-v28-other", SessionID: "cs-v28", Message: schema.SystemMessage("保留的系统消息"),
			Metadata: `{"message_kind":"other"}`, CreatedAt: now},
	} {
		if err := st.AppendChatMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := st.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateV28(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("执行 v28 迁移失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	messages, err := st.ListChatMessages("cs-v28", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	roles := make(map[string]schema.RoleType, len(messages))
	for _, message := range messages {
		roles[message.ID] = message.Message.Role
	}
	if roles["msg-v28-workflow"] != schema.Assistant || roles["msg-v28-other"] != schema.System {
		t.Fatalf("v28 消息角色转换范围错误: %+v", roles)
	}
}

// TestRunSessions 验证 run session 的创建、结束与状态迁移。
func TestRunSessions(t *testing.T) {
	st := openTestStore(t)

	started := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	if err := st.CreateRunSession(RunSession{
		ID: "run-1", AgentName: "leader", ChatSessionID: "cs-1",
		Status: RunStatusRunning, StartedAt: started,
	}); err != nil {
		t.Fatalf("CreateRunSession 失败: %v", err)
	}

	run, err := st.GetRunSession("run-1")
	if err != nil {
		t.Fatalf("GetRunSession 失败: %v", err)
	}
	if run.Status != RunStatusRunning || run.EndedAt != nil {
		t.Errorf("运行中状态不符: %+v", run)
	}

	ended := started.Add(2 * time.Second)
	if err := st.EndRunSession("run-1", RunStatusCompleted, ended); err != nil {
		t.Fatalf("EndRunSession 失败: %v", err)
	}
	run, err = st.GetRunSession("run-1")
	if err != nil {
		t.Fatalf("GetRunSession 失败: %v", err)
	}
	if run.Status != RunStatusCompleted || run.EndedAt == nil || !run.EndedAt.Equal(ended) {
		t.Errorf("结束状态不符: %+v", run)
	}

	// 结束不存在的 run：报错。
	if err := st.EndRunSession("run-x", RunStatusCompleted, ended); err == nil {
		t.Error("结束不存在的 run session 应报错")
	}
}

// TestLegacyRunStatusesRejected 验证旧状态只在 v14 数据库升级时转换；
// v0.2 运行时的创建、中间态迁移和结束接口都拒绝旧名称。
func TestLegacyRunStatusesRejected(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC()
	for index, status := range []string{"done", "awaiting_approval"} {
		err := st.CreateRunSession(RunSession{
			ID: fmt.Sprintf("run-legacy-%d", index), ChatSessionID: "cs-legacy",
			AgentName: "leader", Status: status, StartedAt: now,
		})
		if !errors.Is(err, ErrInvalidState) {
			t.Errorf("CreateRunSession 应拒绝旧状态 %q，实际: %v", status, err)
		}
	}

	if err := st.CreateRunSession(RunSession{
		ID: "run-current", ChatSessionID: "cs-current", AgentName: "leader",
		Status: RunStatusRunning, StartedAt: now,
	}); err != nil {
		t.Fatalf("准备当前 Run 失败: %v", err)
	}
	if _, err := st.TransitionRunStatus("run-current", []string{RunStatusRunning},
		"awaiting_approval", now); !errors.Is(err, ErrInvalidState) {
		t.Errorf("TransitionRunStatus 应拒绝旧目标状态，实际: %v", err)
	}
	if _, err := st.TransitionRunStatus("run-current", []string{"awaiting_approval"},
		RunStatusWaitingInput, now); !errors.Is(err, ErrInvalidState) {
		t.Errorf("TransitionRunStatus 应拒绝旧来源状态，实际: %v", err)
	}
	if _, err := st.FinishRunSession("run-current", []string{RunStatusRunning},
		"done", "", now); !errors.Is(err, ErrInvalidState) {
		t.Errorf("FinishRunSession 应拒绝旧终态，实际: %v", err)
	}
	if _, err := st.FinishRunSession("run-current", []string{"awaiting_approval"},
		RunStatusCompleted, "", now); !errors.Is(err, ErrInvalidState) {
		t.Errorf("FinishRunSession 应拒绝旧来源状态，实际: %v", err)
	}
}

// TestChatIDs 验证 ID 生成器：消息 ID 字典序与时间序一致，其余 ID 带前缀且不重复。
func TestChatIDs(t *testing.T) {
	prev := NewChatMessageID()
	for i := 0; i < 100; i++ {
		next := NewChatMessageID()
		if next <= prev {
			t.Fatalf("消息 ID 应随时间递增：%q 之后生成了 %q", prev, next)
		}
		prev = next
	}
	if id := NewChatSessionID(); len(id) < 4 || id[:3] != "cs-" {
		t.Errorf("会话 ID 应有 cs- 前缀，实际: %q", id)
	}
	if id := NewRunSessionID(); len(id) < 5 || id[:4] != "run-" {
		t.Errorf("run ID 应有 run- 前缀，实际: %q", id)
	}
	id1, id2 := NewChatSessionID(), NewChatSessionID()
	if id1 == id2 {
		t.Errorf("会话 ID 不应重复: %q", id1)
	}
}
