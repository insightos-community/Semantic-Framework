package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/llm"
)

// TestFinishCompletedRunCancellationPriority 用两个确定顺序覆盖完成/取消竞速：
// cancelling 已经保存时，即使模型随后返回 Done 也必须落 cancelled；completed
// 已经保存时，迟到的取消不得覆盖终态。测试同时确认失败的第一次完成迁移
// 不会写入重复 Assistant 消息。
func TestFinishCompletedRunCancellationPriority(t *testing.T) {
	fx := newTestFixture(t)
	project, err := fx.st.EnsureDefaultProject("usr-race")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	session := store.ChatSession{ID: store.NewChatSessionID(), UserID: "usr-race",
		ProjectID: project.ID, Title: "完成取消竞速", CreatedAt: now, UpdatedAt: now}
	if err := fx.st.CreateChatSession(session); err != nil {
		t.Fatal(err)
	}
	prof, err := fx.loader.Load("leader")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(Deps{Store: fx.st, Bus: fx.bus, Logger: fx.logger})
	rt := &sessionRuntime{profile: prof}

	createRun := func(id string) store.RunSession {
		run := store.RunSession{ID: id, ProjectID: project.ID,
			ChatSessionID: session.ID, ContextID: "leader:" + session.ID,
			AgentID: "leader", AgentName: "leader", TraceID: "trace-" + id,
			Status: store.RunStatusRunning, StartedAt: now, UpdatedAt: now}
		if err := fx.st.CreateRunSession(run); err != nil {
			t.Fatal(err)
		}
		stored, err := fx.st.GetRunSession(id)
		if err != nil {
			t.Fatal(err)
		}
		return stored
	}

	cancelFirst := createRun("run-cancel-first")
	if _, err := fx.st.TransitionRunStatus(cancelFirst.ID,
		[]string{store.RunStatusRunning}, store.RunStatusCancelling, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	finished, err := svc.finishCompletedRun(rt, cancelFirst, "取消同时到达的回答",
		RunMetadata{Status: store.RunStatusCompleted})
	if err != nil || finished.Status != store.RunStatusCancelled || finished.EndedAt == nil {
		t.Fatalf("已保存取消必须优先收敛: run=%+v err=%v", finished, err)
	}
	messages, err := fx.st.ListChatMessages(session.ID, 0, 0)
	if err != nil || len(messages) != 1 {
		t.Fatalf("竞速只能写入一条 Assistant: messages=%+v err=%v", messages, err)
	}
	var metadata RunMetadata
	if err := json.Unmarshal([]byte(messages[0].Metadata), &metadata); err != nil ||
		metadata.Status != store.RunStatusCancelled {
		t.Fatalf("Assistant 元数据必须使用实际取消终态: metadata=%+v err=%v", metadata, err)
	}

	completeFirst := createRun("run-complete-first")
	finished, err = svc.finishCompletedRun(rt, completeFirst, "正常完成的回答",
		RunMetadata{Status: store.RunStatusCompleted})
	if err != nil || finished.Status != store.RunStatusCompleted {
		t.Fatalf("先完成的 Run 应保持 completed: run=%+v err=%v", finished, err)
	}
	if _, err := fx.st.TransitionRunStatus(completeFirst.ID,
		[]string{store.RunStatusRunning}, store.RunStatusCancelling, now.Add(2*time.Second)); !errors.Is(err, store.ErrInvalidState) {
		t.Fatalf("迟到取消不得覆盖 completed，实际: %v", err)
	}
}

// TestCancelRunByIDTransitionsAndPublishes 验证停止入口不会按 Conversation
// 猜测目标：只有精确活动 run_id 能进入 cancelling，并且三个状态事件使用
// 同一 Project、Run 和 Trace 关联。
func TestCancelRunByIDTransitionsAndPublishes(t *testing.T) {
	fx := newTestFixture(t)
	model := &cancelBlockingModel{MockChatModel: kernel.NewMockChatModel(), started: make(chan struct{})}
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
		BuildModel: func(context.Context, llm.Provider, string) (kernel.Model, error) {
			return model, nil
		},
	})

	done := make(chan error, 1)
	go func() {
		_, err := svc.HandleMessage(context.Background(), "usr-1", "", "等待精确停止")
		done <- err
	}()
	select {
	case <-model.started:
	case <-time.After(runtimeEventWait):
		t.Fatalf("Run 未在 %s 内进入模型", runtimeEventWait)
	}
	sessions, err := fx.st.ListChatSessionsByUser("usr-1")
	if err != nil || len(sessions) != 1 {
		t.Fatalf("Conversation 未创建: sessions=%+v err=%v", sessions, err)
	}
	runs, total, err := fx.st.ListRunSessions(store.RunFilter{
		ChatSessionID: sessions[0].ID,
	}, 0, 0)
	if err != nil || total != 1 {
		t.Fatalf("活动 Run 未保存: runs=%+v total=%d err=%v", runs, total, err)
	}
	run := runs[0]
	if run.Status != store.RunStatusRunning || run.TraceID == "" || run.ProjectID == "" {
		t.Fatalf("活动 Run 字段不完整: %+v", run)
	}
	if err := svc.CancelRunByID(context.Background(), "usr-1", "proj-wrong", run.ID); err == nil {
		t.Fatal("错误 Project 不得取消 Run")
	}
	unchanged, _ := fx.st.GetRunSession(run.ID)
	if unchanged.Status != store.RunStatusRunning {
		t.Fatalf("错误取消不得改变状态: %+v", unchanged)
	}
	if err := svc.CancelRunByID(context.Background(), "usr-1", run.ProjectID, run.ID); err != nil {
		t.Fatalf("精确取消失败: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("用户取消应正常收尾: %v", err)
		}
	case <-time.After(runtimeEventWait):
		t.Fatalf("取消后 Run 未在 %s 内收尾", runtimeEventWait)
	}
	cancelled, err := fx.st.GetRunSession(run.ID)
	if err != nil || cancelled.Status != store.RunStatusCancelled ||
		cancelled.EndedAt == nil || cancelled.Revision != 3 {
		t.Fatalf("取消终态不符: run=%+v err=%v", cancelled, err)
	}
	if err := svc.CancelRunByID(context.Background(), "usr-1", run.ProjectID, run.ID); !errors.Is(err, store.ErrInvalidState) {
		t.Fatalf("已结束 Run 再次精确取消应返回非法状态，实际: %v", err)
	}

	var stateEvents []ws.Envelope
	for _, item := range drainEvents(fx.events) {
		envelope, ok := item.Payload.(ws.Envelope)
		if !ok || envelope.ResourceType != "agent_run" {
			continue
		}
		stateEvents = append(stateEvents, envelope)
	}
	if len(stateEvents) != 3 {
		t.Fatalf("应有 started/cancelling/cancelled 三个状态事件: %+v", stateEvents)
	}
	wantTypes := []string{EventTypeRunStarted, EventTypeRunCancelling, EventTypeRunCancelled}
	for index, envelope := range stateEvents {
		if envelope.Type != wantTypes[index] || envelope.ResourceID != run.ID ||
			envelope.Revision != int64(index+1) || envelope.ProjectID != run.ProjectID ||
			envelope.Parent.RunID != run.ID || envelope.Parent.TraceID != run.TraceID {
			t.Errorf("状态事件 %d 不符: %+v", index, envelope)
		}
	}
}

// TestRecoverInterruptedRunsDoesNotReplay 验证 Server 重启只收敛状态并发布
// 终态，不恢复旧模型调用或工具；已经完成的 Run 保持原样。
func TestRecoverInterruptedRunsDoesNotReplay(t *testing.T) {
	fx := newTestFixture(t)
	project, err := fx.st.EnsureDefaultProject("usr-restart")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	session := store.ChatSession{ID: store.NewChatSessionID(), UserID: "usr-restart",
		ProjectID: project.ID, Title: "恢复", CreatedAt: now, UpdatedAt: now}
	if err := fx.st.CreateChatSession(session); err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{
		"run-restart-running":    store.RunStatusRunning,
		"run-restart-waiting":    store.RunStatusWaitingInput,
		"run-restart-cancelling": store.RunStatusCancelling,
		"run-restart-completed":  store.RunStatusCompleted,
	}
	for id, status := range statuses {
		run := store.RunSession{ID: id, ProjectID: project.ID,
			ChatSessionID: session.ID, AgentID: "leader", AgentName: "leader",
			TraceID: "trace-" + id, Status: status, StartedAt: now, UpdatedAt: now}
		if status == store.RunStatusCompleted {
			ended := now
			run.EndedAt = &ended
		}
		if err := fx.st.CreateRunSession(run); err != nil {
			t.Fatal(err)
		}
	}
	svc := NewService(Deps{Store: fx.st, Bus: fx.bus, Logger: fx.logger})
	if err := svc.RecoverInterruptedRuns(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"run-restart-running":    store.RunStatusFailed,
		"run-restart-waiting":    store.RunStatusFailed,
		"run-restart-cancelling": store.RunStatusCancelled,
		"run-restart-completed":  store.RunStatusCompleted,
	}
	for id, status := range want {
		run, err := fx.st.GetRunSession(id)
		if err != nil || run.Status != status {
			t.Errorf("Run %s 重启终态不符: run=%+v err=%v", id, run, err)
		}
		if status == store.RunStatusFailed && run.Error == "" {
			t.Errorf("中断失败 Run %s 应保存明确错误", id)
		}
	}
	events := drainEvents(fx.events)
	if len(events) != 3 {
		t.Fatalf("只应为三个旧非终态 Run 发布事件，实际: %d", len(events))
	}
	for _, item := range events {
		envelope, ok := item.Payload.(ws.Envelope)
		if !ok || envelope.ResourceType != "agent_run" || envelope.Revision != 2 ||
			envelope.Parent.RunID != envelope.ResourceID || envelope.Parent.TraceID == "" {
			t.Errorf("重启 Run 事件不符: %+v", item.Payload)
		}
		if envelope.Type != EventTypeRunFailed && envelope.Type != EventTypeRunCancelled {
			t.Errorf("重启事件类型不符: %s", envelope.Type)
		}
	}
}
