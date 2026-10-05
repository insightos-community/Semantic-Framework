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

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"insightos.cn/semantic-framework/internal/interaction"
	"insightos.cn/semantic-framework/internal/store"
)

// ResumeConversationInteraction 为已回答的 Conversation Interaction 启动新的
// Leader Run。原 Run 已经结束，不能继续占用模型请求；来源 Interaction ID
// 同时写入新 Run，使 Server 在“Run 已建立、Interaction 尚未标记 handled”
// 的崩溃窗口恢复时能够识别原执行，而不是重复消费同一回答。
func (s *Service) ResumeConversationInteraction(ctx context.Context, value store.Interaction) error {
	if value.WorkflowID != "" || value.TaskID != "" || value.RunID == "" {
		return store.ErrInvalidState
	}
	if existing, err := s.st.GetRunSessionBySourceInteraction(value.ID); err == nil {
		if existing.Status == store.RunStatusQueued || existing.Status == store.RunStatusRunning ||
			existing.Status == store.RunStatusWaitingInput || existing.Status == store.RunStatusCompleted {
			return nil
		}
		return store.ErrContinuationInterrupted
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	sourceRun, err := s.st.GetRunSession(value.RunID)
	if err != nil || sourceRun.Kind != store.RunKindConversation ||
		sourceRun.ProjectID != value.ProjectID || sourceRun.ChatSessionID != value.SessionID {
		return store.ErrInvalidState
	}
	project, err := s.st.GetProject(value.ProjectID)
	if err != nil {
		return err
	}
	var payload interaction.StructuredPayload
	if err := json.Unmarshal([]byte(value.Payload), &payload); err != nil {
		return fmt.Errorf("读取 Interaction 问题失败: %w", err)
	}
	mode := payload.InteractionMode
	if mode == "" {
		mode = interactionModeCollaboration
	}
	outcome := "用户回答：" + value.Reply
	if value.Status == store.InteractionStatusCancelled {
		outcome = "用户取消了本次询问，没有提供答案"
	}
	prompt := fmt.Sprintf("你之前提出的结构化问题已经结束。\n问题：%s\n结果：%s\n请结合原Conversation上下文继续当前工作；取消不等于停止Workflow，也不能臆造答案。",
		payload.Prompt, outcome)

	done := make(chan error, 1)
	go func() {
		_, runErr := s.handleMessageWithMode(context.Background(), project.OwnerID,
			value.SessionID, prompt, nil, "", "", mode, value.ID, sourceRun.AgentID)
		done <- runErr
	}()

	// HTTP 回答只等待新 Run 真正持久化，不等待模型完成。这样既保持前端交互
	// 及时返回，也保证 AnswerRouter 标记 handled 前已经形成可恢复的 Run。
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case runErr := <-done:
			return runErr
		case <-ticker.C:
			if _, runErr := s.st.GetRunSessionBySourceInteraction(value.ID); runErr == nil {
				return nil
			} else if !errors.Is(runErr, store.ErrNotFound) {
				return runErr
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
