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
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestArtifactPutGetList 验证产物的写入-读取-列表闭环：
// 内容本体落文件、元数据落库、URI 引用一致。
func TestArtifactPutGetList(t *testing.T) {
	st := openTestStore(t)

	content := []byte("# 季度报告\n营收 100 万。")
	a, err := st.PutArtifact("text/markdown", "Q3 营收报告", `{"author":"leader"}`, content)
	if err != nil {
		t.Fatalf("PutArtifact 失败: %v", err)
	}
	if a.ID == "" || a.URI != "artifact://"+a.ID || a.Size != int64(len(content)) {
		t.Errorf("产物元数据不符: %+v", a)
	}

	// 内容文件真实存在于产物目录。
	if _, err := os.Stat(filepath.Join(st.ArtifactDir(), a.ID)); err != nil {
		t.Fatalf("产物内容文件应存在: %v", err)
	}

	got, gotContent, err := st.GetArtifact(a.ID)
	if err != nil {
		t.Fatalf("GetArtifact 失败: %v", err)
	}
	if got.Summary != "Q3 营收报告" || got.MediaType != "text/markdown" || got.Metadata != `{"author":"leader"}` {
		t.Errorf("读取的产物元数据不符: %+v", got)
	}
	if string(gotContent) != string(content) {
		t.Errorf("产物内容不符: %q", gotContent)
	}

	list, err := st.ListArtifacts(0, 0)
	if err != nil || len(list) != 1 || list[0].ID != a.ID {
		t.Fatalf("ListArtifacts 应返回 1 条，实际: %v, %d", err, len(list))
	}
}

// TestArtifactNotFound 验证缺失产物的 ErrNotFound 语义。
func TestArtifactNotFound(t *testing.T) {
	st := openTestStore(t)
	if _, _, err := st.GetArtifact("art-missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("缺失产物应返回 ErrNotFound，实际: %v", err)
	}
	if _, err := st.GetArtifactMeta("art-missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("缺失产物元数据应返回 ErrNotFound，实际: %v", err)
	}
}

// TestArtifactDelete 验证产物删除：元数据行与内容本体都移除，
// 重复删除返回 ErrNotFound。
func TestArtifactDelete(t *testing.T) {
	st := openTestStore(t)

	a, err := st.PutArtifact("text/plain", "演示产物", "{}", []byte("演示内容"))
	if err != nil {
		t.Fatalf("PutArtifact 失败: %v", err)
	}
	if err := st.DeleteArtifact(a.ID); err != nil {
		t.Fatalf("DeleteArtifact 失败: %v", err)
	}
	if _, err := st.GetArtifactMeta(a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除后产物元数据应返回 ErrNotFound，实际: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.ArtifactDir(), a.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("删除后产物内容文件应不存在，实际: %v", err)
	}
	if err := st.DeleteArtifact(a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复删除应返回 ErrNotFound，实际: %v", err)
	}
}

// TestInteractionLifecycle 验证交互状态机：pending → answered，
// 以及防重复应答（条件更新原子保证）。
func TestInteractionLifecycle(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC()

	it := Interaction{
		ID: NewInteractionID(), SessionID: "cs-1", Agent: "leader",
		Type: InteractionTypeConfirm, Status: InteractionStatusPending,
		Payload: `{"question":"是否批准？"}`, RunID: "run-1", CheckpointID: "run-1",
		CreatedAt: now,
	}
	if err := st.CreateInteraction(it); err != nil {
		t.Fatalf("CreateInteraction 失败: %v", err)
	}

	got, err := st.GetInteraction(it.ID)
	if err != nil {
		t.Fatalf("GetInteraction 失败: %v", err)
	}
	if got.Status != InteractionStatusPending || got.SessionID != "cs-1" || got.RunID != "run-1" {
		t.Errorf("交互初始状态不符: %+v", got)
	}

	pending, err := st.ListPendingInteractions("cs-1")
	if err != nil || len(pending) != 1 {
		t.Fatalf("应有 1 条待应答交互，实际: %v, %d", err, len(pending))
	}

	// 应答：pending → answered，应答负载与时间落库。
	answeredAt := now.Add(time.Second)
	if err := st.AnswerInteraction(it.ID, `{"approved":true}`, answeredAt); err != nil {
		t.Fatalf("AnswerInteraction 失败: %v", err)
	}
	got, _ = st.GetInteraction(it.ID)
	if got.Status != InteractionStatusAnswered || got.Reply != `{"approved":true}` ||
		got.AnsweredAt == nil || !got.AnsweredAt.Equal(answeredAt) {
		t.Errorf("应答后状态不符: %+v", got)
	}

	// 重复应答 / 应答后过期：都应返回 ErrNotPending。
	if err := st.AnswerInteraction(it.ID, `{"approved":false}`, now); !errors.Is(err, ErrNotPending) {
		t.Errorf("重复应答应返回 ErrNotPending，实际: %v", err)
	}
	if err := st.ExpireInteraction(it.ID, now); !errors.Is(err, ErrNotPending) {
		t.Errorf("已应答后过期应返回 ErrNotPending，实际: %v", err)
	}

	pending, _ = st.ListPendingInteractions("cs-1")
	if len(pending) != 0 {
		t.Errorf("应答后不应再有待应答交互，实际: %d", len(pending))
	}
}

// TestInteractionExpireAndCancel 验证超时与批量取消路径。
func TestInteractionExpireAndCancel(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC()

	mk := func(sessionID string) Interaction {
		it := Interaction{
			ID: NewInteractionID(), SessionID: sessionID, Agent: "leader",
			Type: InteractionTypeConfirm, Status: InteractionStatusPending,
			Payload: `{}`, CreatedAt: now,
		}
		if err := st.CreateInteraction(it); err != nil {
			t.Fatalf("CreateInteraction 失败: %v", err)
		}
		return it
	}

	// 超时：pending → expired，expired_at 落库；过期后再应答返回 ErrNotPending。
	exp := mk("cs-1")
	if err := st.ExpireInteraction(exp.ID, now.Add(time.Minute)); err != nil {
		t.Fatalf("ExpireInteraction 失败: %v", err)
	}
	got, _ := st.GetInteraction(exp.ID)
	if got.Status != InteractionStatusExpired || got.ExpiredAt == nil {
		t.Errorf("超时后状态不符: %+v", got)
	}
	if err := st.AnswerInteraction(exp.ID, `{"approved":true}`, now); !errors.Is(err, ErrNotPending) {
		t.Errorf("过期后应答应返回 ErrNotPending，实际: %v", err)
	}

	// 批量取消：同会话 2 条 pending 全取消，其他会话不受影响。
	c1, c2 := mk("cs-2"), mk("cs-2")
	other := mk("cs-3")
	n, err := st.CancelPendingInteractions("cs-2", now)
	if err != nil || n != 2 {
		t.Fatalf("应取消 2 条，实际: %v, %d", err, n)
	}
	for _, id := range []string{c1.ID, c2.ID} {
		got, _ := st.GetInteraction(id)
		if got.Status != InteractionStatusCancelled {
			t.Errorf("交互 %q 应为 cancelled，实际: %+v", id, got)
		}
	}
	got, _ = st.GetInteraction(other.ID)
	if got.Status != InteractionStatusPending {
		t.Errorf("其他会话交互不应受影响，实际: %+v", got)
	}
	if pending, _ := st.ListPendingInteractions("cs-2"); len(pending) != 0 {
		t.Errorf("取消后不应再有待应答交互，实际: %d", len(pending))
	}
}

// TestRunCheckpointAndStatus 验证 run 断点的存取与 running ↔ waiting_input
// 状态迁移。
func TestRunCheckpointAndStatus(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC()

	runID := NewRunSessionID()
	if err := st.CreateRunSession(RunSession{
		ID: runID, AgentName: "leader", ChatSessionID: "cs-1",
		Status: RunStatusRunning, StartedAt: now,
	}); err != nil {
		t.Fatalf("CreateRunSession 失败: %v", err)
	}

	// 初始无断点。
	if _, ok, err := st.GetRunCheckpoint(runID); err != nil || ok {
		t.Fatalf("初始应无断点，实际: ok=%v, err=%v", ok, err)
	}

	// 状态迁移 running → waiting_input → running。
	if err := st.UpdateRunSessionStatus(runID, RunStatusWaitingInput); err != nil {
		t.Fatalf("UpdateRunSessionStatus 失败: %v", err)
	}
	run, _ := st.GetRunSession(runID)
	if run.Status != RunStatusWaitingInput || run.EndedAt != nil {
		t.Errorf("状态应迁移到 waiting_input 且无结束时间，实际: %+v", run)
	}
	if err := st.UpdateRunSessionStatus(runID, RunStatusRunning); err != nil {
		t.Fatalf("恢复 running 失败: %v", err)
	}

	// 断点写入-读取-覆盖。
	if err := st.SaveRunCheckpoint(runID, []byte("checkpoint-v1")); err != nil {
		t.Fatalf("SaveRunCheckpoint 失败: %v", err)
	}
	data, ok, err := st.GetRunCheckpoint(runID)
	if err != nil || !ok || string(data) != "checkpoint-v1" {
		t.Fatalf("断点读取不符: ok=%v, data=%q, err=%v", ok, data, err)
	}
	if err := st.SaveRunCheckpoint(runID, []byte("checkpoint-v2")); err != nil {
		t.Fatalf("覆盖断点失败: %v", err)
	}
	data, _, _ = st.GetRunCheckpoint(runID)
	if string(data) != "checkpoint-v2" {
		t.Errorf("断点应被覆盖，实际: %q", data)
	}

	// 不存在的 run：状态更新与断点保存报 ErrNotFound；断点读取返回不存在。
	if err := st.UpdateRunSessionStatus("run-x", RunStatusRunning); !errors.Is(err, ErrNotFound) {
		t.Errorf("更新缺失 run 应返回 ErrNotFound，实际: %v", err)
	}
	if err := st.SaveRunCheckpoint("run-x", []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Errorf("保存缺失 run 断点应返回 ErrNotFound，实际: %v", err)
	}
	if _, ok, _ := st.GetRunCheckpoint("run-x"); ok {
		t.Error("缺失 run 不应有断点")
	}
}

// TestListInteractions 验证交互记录的分页列表：session/status 过滤、
// 创建时间倒序与总数统计。
func TestListInteractions(t *testing.T) {
	st := openTestStore(t)
	base := time.Now().UTC().Truncate(time.Second)

	mk := func(sessionID string, createdAt time.Time) Interaction {
		return Interaction{
			ID: NewInteractionID(), SessionID: sessionID, Agent: "leader",
			Type: InteractionTypeConfirm, Status: InteractionStatusPending,
			Payload: `{"question":"是否批准？"}`, RunID: "run-1", CheckpointID: "run-1",
			CreatedAt: createdAt,
		}
	}
	it1 := mk("cs-1", base)
	it2 := mk("cs-1", base.Add(time.Second))
	it3 := mk("cs-2", base.Add(2*time.Second))
	for _, it := range []Interaction{it1, it2, it3} {
		if err := st.CreateInteraction(it); err != nil {
			t.Fatalf("CreateInteraction 失败: %v", err)
		}
	}
	// it1 应答，构造混合状态。
	if err := st.AnswerInteraction(it1.ID, `{"approved":true}`, base.Add(3*time.Second)); err != nil {
		t.Fatalf("AnswerInteraction 失败: %v", err)
	}

	// 无过滤：全部 3 条，按创建时间倒序（it3 → it2 → it1）。
	all, total, err := st.ListInteractions(InteractionFilter{}, 10, 0)
	if err != nil {
		t.Fatalf("ListInteractions 失败: %v", err)
	}
	if total != 3 || len(all) != 3 {
		t.Fatalf("应有 3 条（total=3），实际: total=%d len=%d", total, len(all))
	}
	if all[0].ID != it3.ID || all[1].ID != it2.ID || all[2].ID != it1.ID {
		t.Errorf("应按创建时间倒序，实际: %+v", all)
	}
	if all[2].Status != InteractionStatusAnswered || all[2].Reply != `{"approved":true}` || all[2].AnsweredAt == nil {
		t.Errorf("it1 应为已应答，实际: %+v", all[2])
	}

	// session 过滤。
	bySession, total, err := st.ListInteractions(InteractionFilter{SessionID: "cs-1"}, 10, 0)
	if err != nil {
		t.Fatalf("ListInteractions(session) 失败: %v", err)
	}
	if total != 2 || len(bySession) != 2 {
		t.Errorf("cs-1 应有 2 条，实际: total=%d len=%d", total, len(bySession))
	}

	// status 过滤：pending 剩 it2/it3；answered 只有 it1。
	pending, total, err := st.ListInteractions(InteractionFilter{Status: InteractionStatusPending}, 10, 0)
	if err != nil {
		t.Fatalf("ListInteractions(status) 失败: %v", err)
	}
	if total != 2 || len(pending) != 2 || pending[0].ID != it3.ID || pending[1].ID != it2.ID {
		t.Errorf("pending 应为 it3/it2 倒序，实际: total=%d %+v", total, pending)
	}
	answered, total, err := st.ListInteractions(InteractionFilter{Status: InteractionStatusAnswered}, 10, 0)
	if err != nil {
		t.Fatalf("ListInteractions(answered) 失败: %v", err)
	}
	if total != 1 || len(answered) != 1 || answered[0].ID != it1.ID {
		t.Errorf("answered 应只有 it1，实际: total=%d %+v", total, answered)
	}

	// 组合过滤 + 分页：cs-1 pending 只有 it2；page_size=1 第 2 页为空但 total 不变。
	combo, total, err := st.ListInteractions(
		InteractionFilter{SessionID: "cs-1", Status: InteractionStatusPending}, 10, 0)
	if err != nil {
		t.Fatalf("ListInteractions(combo) 失败: %v", err)
	}
	if total != 1 || len(combo) != 1 || combo[0].ID != it2.ID {
		t.Errorf("cs-1 pending 应只有 it2，实际: total=%d %+v", total, combo)
	}
	page2, total, err := st.ListInteractions(InteractionFilter{}, 2, 2)
	if err != nil {
		t.Fatalf("ListInteractions(分页) 失败: %v", err)
	}
	if total != 3 || len(page2) != 1 || page2[0].ID != it1.ID {
		t.Errorf("分页结果不符: total=%d %+v", total, page2)
	}
}
