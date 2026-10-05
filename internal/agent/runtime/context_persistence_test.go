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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/llm"
)

// TestContextSummaryPersistsBoundaryAndRestoresRecentMessages 验证真正跨 Run 的
// Context 压缩：Eino 生成摘要后保存边界，后续装配只读取摘要和边界后消息，
// 同时自动加入用户维护的 Project Memory。
func TestContextSummaryPersistsBoundaryAndRestoresRecentMessages(t *testing.T) {
	fx := newTestFixture(t)
	rolePath := filepath.Join(fx.profileRoot, "leader", "role.yaml")
	if err := os.WriteFile(rolePath, []byte(`name: leader
mode: coordinator
description: 团队指挥官
model: mock
limits:
  max_turns: 10
  context_tokens: 120
`), 0o600); err != nil {
		t.Fatal(err)
	}

	project, err := fx.st.EnsureDefaultProject("usr-1")
	if err != nil {
		t.Fatal(err)
	}
	memory, err := fx.st.SaveProjectMemory("usr-1", project.ID,
		"- 测试偏好：回答必须给出可验证结果。", 0)
	if err != nil || memory.Revision != 1 {
		t.Fatalf("保存 Project Memory 失败: memory=%+v err=%v", memory, err)
	}
	now := time.Now().UTC()
	session := store.ChatSession{ID: store.NewChatSessionID(), UserID: "usr-1",
		ProjectID: project.ID, Title: "长对话", CreatedAt: now, UpdatedAt: now}
	if err := fx.st.CreateChatSession(session); err != nil {
		t.Fatal(err)
	}
	oldUser := "早期用户原文-" + strings.Repeat("甲", 260)
	oldAssistant := "早期助手原文-" + strings.Repeat("乙", 260)
	for _, message := range []*schema.Message{
		schema.UserMessage(oldUser), schema.AssistantMessage(oldAssistant, nil),
	} {
		if err := fx.st.AppendChatMessage(store.ChatMessage{
			ID: store.NewChatMessageID(), SessionID: session.ID,
			Message: message, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}

	model := kernel.NewMockChatModel()
	model.SetScript(
		kernel.MockReply{Content: "已压缩的对话摘要：用户正在验证持久化上下文。"},
		kernel.MockReply{Content: "本轮已完成。"},
	)
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus,
		Logger: fx.logger,
		BuildModel: func(context.Context, llm.Provider, string) (kernel.Model, error) {
			return model, nil
		},
	})
	if err := svc.InitializeSessionModels(session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.HandleMessage(context.Background(), "usr-1", session.ID,
		"请继续，并压缩此前历史。"); err != nil {
		t.Fatalf("触发摘要的 Run 失败: %v", err)
	}

	summary, err := fx.st.GetContextSummaryBySession(session.ID)
	if err != nil {
		t.Fatalf("摘要未持久化: %v", err)
	}
	if summary.Revision != 1 || summary.ProjectID != project.ID ||
		strings.TrimSpace(summary.Summary) == "" {
		t.Fatalf("摘要记录不完整: %+v", summary)
	}
	transcript, err := fx.st.ListChatMessages(session.ID, 0, 0)
	if err != nil || len(transcript) != 4 {
		t.Fatalf("Transcript 应完整保留 4 条消息: len=%d err=%v", len(transcript), err)
	}
	currentUser := transcript[len(transcript)-2]
	if summary.CoveredThroughMessageID != currentUser.ID {
		t.Fatalf("摘要边界应覆盖本轮用户消息: got=%q want=%q",
			summary.CoveredThroughMessageID, currentUser.ID)
	}
	recent, err := fx.st.ListChatMessagesAfter(session.ID,
		summary.CoveredThroughMessageID, 0)
	if err != nil || len(recent) != 1 || recent[0].Message.Role != schema.Assistant {
		t.Fatalf("边界后应只剩本轮 Assistant: recent=%+v err=%v", recent, err)
	}

	// 关闭旧 Service，并用同一数据库和全新 Profile Loader/Runner 模拟
	// Server 进程重启。把阈值调高，确保下一轮输入来自 Store 恢复，而不是
	// 在测试过程中再次触发摘要后碰巧删掉旧消息。
	svc.Shutdown()
	if err := os.WriteFile(rolePath, []byte(`name: leader
mode: coordinator
description: 团队指挥官
model: mock
limits:
  max_turns: 10
  context_tokens: 120000
`), 0o600); err != nil {
		t.Fatal(err)
	}
	restartModel := kernel.NewMockChatModel()
	restartModel.SetScript(kernel.MockReply{Content: "重启后本轮完成。"})
	restarted := NewService(Deps{
		Profiles: profile.NewLoader(fx.profileRoot), LLM: fx.llmReg,
		Store: fx.st, Bus: fx.bus, Logger: fx.logger,
		BuildModel: func(context.Context, llm.Provider, string) (kernel.Model, error) {
			return restartModel, nil
		},
	})
	defer restarted.Shutdown()
	loaded, err := restarted.loadContextHistory(session.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 3 || loaded.Messages[0].Role != schema.System ||
		!strings.Contains(loaded.Messages[0].Content, "测试偏好") ||
		loaded.Messages[1].Role != schema.User || loaded.Messages[2].Role != schema.Assistant {
		t.Fatalf("恢复输入应为 Memory + Summary + Recent Assistant: %+v",
			messageRoles(loaded.Messages))
	}
	for _, message := range loaded.Messages {
		if strings.Contains(message.Content, oldUser) || strings.Contains(message.Content, oldAssistant) {
			t.Fatalf("摘要边界前原文不应重新装入模型: role=%s", message.Role)
		}
	}
	if _, err := restarted.HandleMessage(context.Background(), "usr-1", session.ID,
		"重启后的新问题"); err != nil {
		t.Fatalf("重启后继续 Conversation 失败: %v", err)
	}
	inputs := restartModel.CallInputs()
	if len(inputs) != 1 {
		t.Fatalf("高阈值下重启后的主模型应只调用一次，实际: %d", len(inputs))
	}
	var sawSummary bool
	for _, message := range inputs[0] {
		if strings.Contains(message.Content, "已压缩的对话摘要") {
			sawSummary = true
		}
		if strings.Contains(message.Content, oldUser) || strings.Contains(message.Content, oldAssistant) {
			t.Fatalf("重启后的真实模型输入包含摘要边界前原文: role=%s", message.Role)
		}
	}
	if !sawSummary {
		t.Fatalf("重启后的真实模型输入未包含已保存摘要: %+v", messageRoles(inputs[0]))
	}
}
