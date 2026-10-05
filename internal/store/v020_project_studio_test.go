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
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
)

func TestV020ProjectContextAndStudioSnapshot(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)

	// 旧入口创建 Default Project；普通 Project 创建后不抢占活动位置。
	if err := st.CreateChatSession(ChatSession{
		ID: "cs-default", UserID: "usr-v020", Title: "默认会话",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("创建默认会话失败: %v", err)
	}
	project, err := st.CreateProject("usr-v020", "拆码垛开发")
	if err != nil {
		t.Fatalf("CreateProject 失败: %v", err)
	}
	if project.IsActive {
		t.Fatal("已有活动 Project 时新项目不应自动抢占")
	}
	if err := st.CreateChatSession(ChatSession{
		ID: "cs-rejected", UserID: "usr-v020", ProjectID: project.ID,
		Title: "错误项目", CreatedAt: now, UpdatedAt: now,
	}); !errors.Is(err, ErrProjectInactive) {
		t.Fatalf("显式向非活动 Project 创建会话应被拒绝，实际: %v", err)
	}
	project, err = st.ActivateProject("usr-v020", project.ID)
	if err != nil || !project.IsActive {
		t.Fatalf("ActivateProject 失败: %+v, %v", project, err)
	}
	project, err = st.UpdateProjectName("usr-v020", project.ID,
		"拆码垛 Studio", project.Revision)
	if err != nil {
		t.Fatalf("UpdateProjectName 失败: %v", err)
	}
	if _, err := st.UpdateProjectName("usr-v020", project.ID,
		"旧页面覆盖", project.Revision-1); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("旧 revision 应冲突，实际: %v", err)
	}

	session := ChatSession{
		ID: "cs-v020", UserID: "usr-v020", ProjectID: project.ID,
		Title: "运行验证", CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateChatSession(session); err != nil {
		t.Fatalf("创建 Project Conversation 失败: %v", err)
	}
	var messageIDs []string
	for index, text := range []string{"第一轮", "第二轮", "第三轮"} {
		id := NewChatMessageID()
		messageIDs = append(messageIDs, id)
		if err := st.AppendChatMessage(ChatMessage{
			ID: id, SessionID: session.ID, Message: schema.UserMessage(text),
			CreatedAt: now.Add(time.Duration(index) * time.Second),
		}); err != nil {
			t.Fatalf("写入消息失败: %v", err)
		}
	}
	summary, err := st.SaveContextSummary(ContextSummary{
		SessionID: session.ID, ProjectID: project.ID,
		Summary:                 "用户正在验证拆码垛流程",
		CoveredThroughMessageID: messageIDs[1],
	}, 0)
	if err != nil {
		t.Fatalf("首次保存摘要失败: %v", err)
	}
	recent, err := st.ListChatMessagesAfter(session.ID,
		summary.CoveredThroughMessageID, 0)
	if err != nil || len(recent) != 1 || recent[0].ID != messageIDs[2] {
		t.Fatalf("摘要边界后的消息不符: %+v, %v", recent, err)
	}
	if _, err := st.SaveContextSummary(ContextSummary{
		ContextID: summary.ContextID, SessionID: session.ID,
		ProjectID: project.ID, Summary: "过期摘要",
		CoveredThroughMessageID: messageIDs[2],
	}, summary.Revision-1); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("旧摘要 revision 应冲突，实际: %v", err)
	}

	memory, err := st.SaveProjectMemory("usr-v020", project.ID,
		"# Project Memory\n\n- 箱体从 A 搬到 B", 0)
	if err != nil || memory.Revision != 1 {
		t.Fatalf("首次保存 Memory 失败: %+v, %v", memory, err)
	}
	if _, err := st.SaveProjectMemory("usr-v020", project.ID,
		"旧内容", 0); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("重复首次保存 Memory 应冲突，实际: %v", err)
	}

	run := RunSession{
		ID: "run-v020", ChatSessionID: session.ID, AgentName: "leader",
		TraceID: "trace-v020", Status: RunStatusRunning, StartedAt: now,
	}
	if err := st.CreateRunSession(run); err != nil {
		t.Fatalf("创建 Run 失败: %v", err)
	}
	run, err = st.TransitionRunStatus(run.ID,
		[]string{RunStatusRunning}, RunStatusWaitingInput, now.Add(time.Second))
	if err != nil || run.Status != RunStatusWaitingInput ||
		run.ProjectID != project.ID || run.TraceID != "trace-v020" {
		t.Fatalf("Run 中间态不符: %+v, %v", run, err)
	}
	interaction := Interaction{
		ID: NewInteractionID(), SessionID: session.ID, RunID: run.ID,
		Agent: "leader", Type: InteractionTypeConfirm,
		Status: InteractionStatusPending, Payload: `{"question":"继续？"}`,
		CreatedAt: now.Add(2 * time.Second),
	}
	if err := st.CreateInteraction(interaction); err != nil {
		t.Fatalf("创建 Interaction 失败: %v", err)
	}

	event, err := st.InsertProjectEvent(Event{
		ID: "evt-v020-1", SessionID: session.ID, ResourceType: "run",
		ResourceID: run.ID, Revision: run.Revision, Channel: "dialogue",
		Type: "run.updated", Importance: "normal", AgentID: "leader",
		Parent:  `{"run_id":"run-v020","trace_id":"trace-v020"}`,
		Payload: `{"status":"waiting_input"}`, Ts: now,
	})
	if err != nil || event.ProjectID != project.ID || event.Sequence != 1 {
		t.Fatalf("Project 事件写入不符: %+v, %v", event, err)
	}

	snapshot, err := st.BuildStudioSnapshot("usr-v020", project.ID, 20)
	if err != nil {
		t.Fatalf("BuildStudioSnapshot 失败: %v", err)
	}
	if snapshot.Project.ID != project.ID || snapshot.MemoryRevision != 1 ||
		snapshot.EventSequence != 1 || len(snapshot.Conversations) != 1 ||
		len(snapshot.Runs) != 1 || len(snapshot.PendingInteractions) != 1 {
		t.Fatalf("Studio Snapshot 不完整: %+v", snapshot)
	}
	events, err := st.ListProjectEventsAfter(project.ID, 0, 10)
	if err != nil || len(events) != 1 || events[0].Sequence != 1 {
		t.Fatalf("Project 事件回放不符: %+v, %v", events, err)
	}
}

func TestV020RunRecoveryAndProjectArchive(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC()
	if err := st.CreateChatSession(ChatSession{
		ID: "cs-owner", UserID: "usr-owner", Title: "默认",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("创建默认会话失败: %v", err)
	}
	project, err := st.CreateProject("usr-owner", "临时项目")
	if err != nil {
		t.Fatalf("创建 Project 失败: %v", err)
	}
	project, err = st.ActivateProject("usr-owner", project.ID)
	if err != nil {
		t.Fatalf("激活 Project 失败: %v", err)
	}
	if err := st.CreateChatSession(ChatSession{
		ID: "cs-run", UserID: "usr-owner", ProjectID: project.ID,
		Title: "运行", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	for _, value := range []RunSession{
		{ID: "run-running", ChatSessionID: "cs-run", AgentName: "leader",
			Status: RunStatusRunning, StartedAt: now},
		{ID: "run-cancelling", ChatSessionID: "cs-run", AgentName: "leader",
			Status: RunStatusCancelling, StartedAt: now},
	} {
		if err := st.CreateRunSession(value); err != nil {
			t.Fatalf("创建恢复测试 Run 失败: %v", err)
		}
	}
	failed, cancelled, err := st.MarkInterruptedRuns(now.Add(time.Minute))
	if err != nil || failed != 1 || cancelled != 1 {
		t.Fatalf("MarkInterruptedRuns 结果不符: failed=%d cancelled=%d err=%v",
			failed, cancelled, err)
	}
	running, _ := st.GetRunSession("run-running")
	cancelling, _ := st.GetRunSession("run-cancelling")
	if running.Status != RunStatusFailed || running.Error == "" ||
		cancelling.Status != RunStatusCancelled {
		t.Fatalf("恢复后 Run 状态不符: %+v / %+v", running, cancelling)
	}

	project, err = st.ArchiveProject("usr-owner", project.ID, project.Revision)
	if err != nil || project.ArchivedAt == nil || project.IsActive {
		t.Fatalf("归档 Project 失败: %+v, %v", project, err)
	}
	active, err := st.GetActiveProject("usr-owner")
	if err != nil || !active.IsDefault {
		t.Fatalf("归档活动项目后应回到 Default Project: %+v, %v", active, err)
	}
}

func TestArchiveProjectRequiresRunsAndInteractionsToFinish(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC()
	// Default Project 作为归档后的回退目标；被测普通 Project 单独承载运行。
	if err := st.CreateChatSession(ChatSession{
		ID: "cs-archive-default", UserID: "usr-archive", Title: "默认",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("创建默认 Conversation 失败: %v", err)
	}
	project, err := st.CreateProject("usr-archive", "待归档")
	if err != nil {
		t.Fatalf("创建 Project 失败: %v", err)
	}
	project, err = st.ActivateProject("usr-archive", project.ID)
	if err != nil {
		t.Fatalf("激活 Project 失败: %v", err)
	}
	if err := st.CreateChatSession(ChatSession{
		ID: "cs-archive-work", UserID: "usr-archive", ProjectID: project.ID,
		Title: "执行中", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("创建运行 Conversation 失败: %v", err)
	}
	if err := st.CreateRunSession(RunSession{
		ID: "run-archive-work", ChatSessionID: "cs-archive-work",
		AgentName: "leader", Status: RunStatusRunning, StartedAt: now,
	}); err != nil {
		t.Fatalf("创建 Run 失败: %v", err)
	}

	if _, err := st.ArchiveProject("usr-archive", project.ID,
		project.Revision); !errors.Is(err, ErrProjectHasActiveWork) {
		t.Fatalf("running Run 必须阻止归档，实际: %v", err)
	}
	if _, err := st.TransitionRunStatus("run-archive-work",
		[]string{RunStatusRunning}, RunStatusCancelling, now.Add(time.Second)); err != nil {
		t.Fatalf("归档冲突后 Run 应可进入 cancelling: %v", err)
	}
	if _, err := st.FinishRunSession("run-archive-work",
		[]string{RunStatusCancelling}, RunStatusCancelled, "用户取消",
		now.Add(2*time.Second)); err != nil {
		t.Fatalf("归档冲突后 Run 应可完成取消: %v", err)
	}
	if err := st.CreateInteraction(Interaction{
		ID: "int-archive-work", SessionID: "cs-archive-work",
		RunID: "run-archive-work", Agent: "leader", Type: InteractionTypeConfirm,
		Status: InteractionStatusPending, Payload: `{"question":"是否结束？"}`,
		CreatedAt: now.Add(3 * time.Second),
	}); err != nil {
		t.Fatalf("创建 pending Interaction 失败: %v", err)
	}
	if _, err := st.ArchiveProject("usr-archive", project.ID,
		project.Revision); !errors.Is(err, ErrProjectHasActiveWork) {
		t.Fatalf("pending Interaction 必须阻止归档，实际: %v", err)
	}
	if err := st.AnswerInteraction("int-archive-work", `{"approved":false}`,
		now.Add(4*time.Second)); err != nil {
		t.Fatalf("归档冲突后 Interaction 应可应答: %v", err)
	}
	archived, err := st.ArchiveProject("usr-archive", project.ID, project.Revision)
	if err != nil || archived.ArchivedAt == nil {
		t.Fatalf("工作收敛后应允许归档: %+v err=%v", archived, err)
	}
}

func TestArchiveConversationKeepsHistoryAndRequiresWorkToFinish(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC()
	if err := st.CreateChatSession(ChatSession{
		ID: "cs-conversation-default", UserID: "usr-conversation", Title: "默认",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("创建默认 Conversation 失败: %v", err)
	}
	project, err := st.CreateProject("usr-conversation", "Conversation 归档")
	if err != nil {
		t.Fatalf("创建 Project 失败: %v", err)
	}
	project, err = st.ActivateProject("usr-conversation", project.ID)
	if err != nil {
		t.Fatalf("激活 Project 失败: %v", err)
	}
	session := ChatSession{
		ID: "cs-conversation-archive", UserID: "usr-conversation", ProjectID: project.ID,
		Title: "待归档", CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateChatSession(session); err != nil {
		t.Fatalf("创建 Conversation 失败: %v", err)
	}
	if err := st.AppendChatMessage(ChatMessage{
		ID: "msg-conversation-archive", SessionID: session.ID,
		Message: schema.UserMessage("保留这条历史"), CreatedAt: now,
	}); err != nil {
		t.Fatalf("写入历史消息失败: %v", err)
	}
	if err := st.CreateRunSession(RunSession{
		ID: "run-conversation-archive", ChatSessionID: session.ID,
		AgentName: "leader", Status: RunStatusRunning, StartedAt: now,
	}); err != nil {
		t.Fatalf("创建 Run 失败: %v", err)
	}
	if _, err := st.ArchiveConversation("usr-conversation", project.ID, session.ID,
		now.Add(time.Second)); !errors.Is(err, ErrConversationHasActiveWork) {
		t.Fatalf("running Run 必须阻止归档 Conversation，实际: %v", err)
	}
	if _, err := st.FinishRunSession("run-conversation-archive",
		[]string{RunStatusRunning}, RunStatusCompleted, "", now.Add(2*time.Second)); err != nil {
		t.Fatalf("结束 Run 失败: %v", err)
	}
	if err := st.CreateInteraction(Interaction{
		ID: "int-conversation-archive", SessionID: session.ID,
		RunID: "run-conversation-archive", Agent: "leader", Type: InteractionTypeConfirm,
		Status: InteractionStatusPending, Payload: `{"question":"继续？"}`,
		CreatedAt: now.Add(3 * time.Second),
	}); err != nil {
		t.Fatalf("创建 Interaction 失败: %v", err)
	}
	if _, err := st.ArchiveConversation("usr-conversation", project.ID, session.ID,
		now.Add(4*time.Second)); !errors.Is(err, ErrConversationHasActiveWork) {
		t.Fatalf("pending Interaction 必须阻止归档 Conversation，实际: %v", err)
	}
	if err := st.AnswerInteraction("int-conversation-archive", `{"approved":false}`,
		now.Add(5*time.Second)); err != nil {
		t.Fatalf("应答 Interaction 失败: %v", err)
	}
	archived, err := st.ArchiveConversation("usr-conversation", project.ID, session.ID,
		now.Add(6*time.Second))
	if err != nil || archived.ArchivedAt == nil || archived.Revision != 2 {
		t.Fatalf("工作结束后应成功归档: %+v err=%v", archived, err)
	}
	active, err := st.ListChatSessionsByProject(project.ID)
	if err != nil || len(active) != 0 {
		t.Fatalf("活动列表不应返回已归档 Conversation: %+v err=%v", active, err)
	}
	all, err := st.ListChatSessionsByProjectIncludingArchived(project.ID, true)
	if err != nil || len(all) != 1 || all[0].ArchivedAt == nil {
		t.Fatalf("归档列表应返回历史 Conversation: %+v err=%v", all, err)
	}
	messages, err := st.ListChatMessages(session.ID, 0, 0)
	if err != nil || len(messages) != 1 || messages[0].Message.Content != "保留这条历史" {
		t.Fatalf("归档不能删除历史消息: %+v err=%v", messages, err)
	}
}
