package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/store"
)

// ResumeWorkflowTerminal 在原 Conversation 中启动一次普通 Leader Run，汇总
// Workflow 的真实终态。它复用既有 conversation Run 和 Context，不增加新的
// RunKind；WorkflowID + 空 TaskID 是这次总结的幂等边界。
func (s *Service) ResumeWorkflowTerminal(ctx context.Context, view store.WorkflowView) error {
	if view.Workflow.Status != store.WorkflowStatusCompleted &&
		view.Workflow.Status != store.WorkflowStatusFailed &&
		view.Workflow.Status != store.WorkflowStatusStopped {
		return store.ErrInvalidState
	}
	if _, err := s.st.GetWorkflowSummaryRun(view.Workflow.ID); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	project, err := s.st.GetProject(view.Workflow.ProjectID)
	if err != nil {
		return err
	}
	conversation, err := s.st.GetChatSession(view.Workflow.ConversationID)
	if err != nil || conversation.ProjectID != project.ID {
		return store.ErrInvalidState
	}

	done := make(chan error, 1)
	go func() {
		done <- s.runWorkflowTerminalSummary(context.Background(), project, conversation, view)
	}()

	// 与 Interaction 恢复一致：调用方只等待 Run 落库，不阻塞 Workflow 收敛去
	// 等模型输出。Server 即使随后退出，也保留一条明确的 failed/running Run。
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case runErr := <-done:
			return runErr
		case <-ticker.C:
			if _, runErr := s.st.GetWorkflowSummaryRun(view.Workflow.ID); runErr == nil {
				return nil
			} else if !errors.Is(runErr, store.ErrNotFound) {
				return runErr
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *Service) runWorkflowTerminalSummary(ctx context.Context, project store.Project,
	conversation store.ChatSession, view store.WorkflowView) error {
	rt, err := s.buildPurposeRuntime(ctx, conversation.ID, s.leaderPlanningAgentID(),
		store.RunKindConversation, runtimePurposeWorkflowSummary)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	run := store.RunSession{ID: store.NewRunSessionID(), ProjectID: project.ID,
		ChatSessionID: conversation.ID, WorkflowID: view.Workflow.ID,
		Kind: store.RunKindConversation, ContextID: "workflow-summary:" + view.Workflow.ID,
		AgentID: s.leaderPlanningAgentID(), AgentName: rt.profile.Name,
		Provider: rt.modelResolution.Provider, Endpoint: rt.modelResolution.ResolvedEndpoint,
		Model: rt.modelResolution.ResolvedModel, TraceID: kernel.NewTraceID(),
		Status: store.RunStatusRunning, StartedAt: now, UpdatedAt: now}
	if err := s.st.CreateRunSession(run); err != nil {
		if _, existingErr := s.st.GetWorkflowSummaryRun(view.Workflow.ID); existingErr == nil {
			return nil
		}
		return err
	}
	release, runCtx, err := s.registerPurposeRun(ctx, run, rt)
	if err != nil {
		_, _ = s.st.FinishRunSession(run.ID, nil, store.RunStatusFailed, err.Error(), time.Now().UTC())
		return err
	}
	defer release()

	encoded, err := json.Marshal(view)
	if err != nil {
		return err
	}
	prompt := "下面是已经进入终态的 Workflow 结构化事实：\n" + string(encoded) +
		"\n请在原对话中向用户简洁汇总：目标是否完成、各 Task 的真实结果、失败或停止原因，以及已有证据引用。不要推测缺失信息。"
	history, err := s.loadContextHistory(conversation.ID, rt.supportsVision)
	if err != nil {
		return err
	}
	text, _, runErr := s.consumePurposeRun(runCtx, rt, run,
		append(history.Messages, schema.UserMessage(prompt)))
	checkStart := time.Now()
	if runErr == nil && strings.TrimSpace(text) == "" {
		runErr = fmt.Errorf("Workflow summary is empty; no factual summary was produced")
	}
	s.recordContractCheck(run, runtimePurposeWorkflowSummary, 1, checkStart, runErr)
	status, errorText := store.RunStatusCompleted, ""
	if runErr != nil {
		status, errorText = store.RunStatusFailed, runErr.Error()
		if errors.Is(runErr, context.Canceled) {
			status = store.RunStatusCancelled
		}
	}
	finished, finishErr := s.st.FinishRunSession(run.ID, nil, status, errorText, time.Now().UTC())
	if finishErr == nil {
		s.publishRunState(finished, rt, mapRunEventType(status))
	}
	if runErr != nil || finishErr != nil {
		return errors.Join(runErr, finishErr)
	}
	metadata, _ := json.Marshal(map[string]any{"message_kind": "workflow_summary",
		"workflow_id": view.Workflow.ID, "status": view.Workflow.Status})
	message := store.ChatMessage{ID: store.NewChatMessageID(),
		SessionID: conversation.ID, AgentID: run.AgentID, RunID: run.ID, TraceID: run.TraceID,
		Provider: run.Provider, Endpoint: run.Endpoint, Model: run.Model,
		Message: schema.AssistantMessage(text, nil), Metadata: string(metadata), CreatedAt: time.Now().UTC()}
	if err := s.st.AppendChatMessage(message); err != nil {
		return fmt.Errorf("保存 Workflow Leader 总结: %w", err)
	}
	if err := s.st.TouchChatSession(conversation.ID, time.Now().UTC()); err != nil {
		return err
	}
	// purpose Run 不走普通 Conversation run() 的收尾分支，因此需要在消息落库
	// 后显式发送一次 message.done。它是 Leader 的真实模型总结，不应被归类为
	// Workflow 系统活动；刷新后仍由同一条 chat_messages 记录恢复。
	s.publish(eventContextFromRun(run), rt, EventTypeMessageDone, MessageDonePayload{
		RunID: run.ID, TraceID: run.TraceID, Text: text,
	})
	return nil
}
