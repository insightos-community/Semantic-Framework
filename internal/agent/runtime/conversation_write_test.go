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
	"errors"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/llm"
)

// TestConversationWritesShareWritableGuard 验证消息、模型覆盖和执行/工具策略
// 都复用同一门禁；模型、工具和执行策略的读取仍允许在非活动或归档状态下
// 复盘。旧 /ws/chat 与 Studio 最终都调用 HandleMessage，因此无需各自维护
// 一份容易漂移的 Project 状态判断。
func TestConversationWritesShareWritableGuard(t *testing.T) {
	fx := newTestFixture(t)
	model := kernel.NewMockChatModel()
	model.SetResponse("不应在只读 Conversation 中运行")
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus,
		Logger: fx.logger,
		BuildModel: func(context.Context, llm.Provider, string) (kernel.Model, error) {
			return model, nil
		},
	})

	// 普通 Project 便于分别验证 inactive 与 archived；第二个 Project 是切换
	// 和归档后的回退目标。
	work, err := fx.st.CreateProject("usr-write", "写入项目")
	if err != nil {
		t.Fatalf("创建工作 Project 失败: %v", err)
	}
	now := time.Now().UTC()
	if err := fx.st.CreateChatSession(store.ChatSession{
		ID: "cs-write-guard", UserID: "usr-write", ProjectID: work.ID,
		Title: "写入门禁", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("创建 Conversation 失败: %v", err)
	}
	if err := svc.InitializeSessionModels("cs-write-guard"); err != nil {
		t.Fatalf("初始化模型快照失败: %v", err)
	}
	fallback, err := fx.st.CreateProject("usr-write", "回退项目")
	if err != nil {
		t.Fatalf("创建回退 Project 失败: %v", err)
	}
	if _, err := fx.st.ActivateProject("usr-write", fallback.ID); err != nil {
		t.Fatalf("切换 Project 失败: %v", err)
	}

	assertConversationWritesRejected(t, svc, "cs-write-guard", store.ErrProjectInactive)
	assertConversationReadsAvailable(t, svc, "cs-write-guard")

	if _, err := fx.st.ActivateProject("usr-write", work.ID); err != nil {
		t.Fatalf("重新激活工作 Project 失败: %v", err)
	}
	if err := fx.st.ArchiveChatSession("usr-write", work.ID, "cs-write-guard",
		time.Now().UTC()); err != nil {
		t.Fatalf("归档 Conversation 失败: %v", err)
	}
	assertConversationWritesRejected(t, svc, "cs-write-guard", store.ErrConversationArchived)
	assertConversationReadsAvailable(t, svc, "cs-write-guard")

	if err := fx.st.CreateChatSession(store.ChatSession{
		ID: "cs-archived-project", UserID: "usr-write", ProjectID: work.ID,
		Title: "项目归档", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("创建 Project 归档测试 Conversation 失败: %v", err)
	}
	if err := svc.InitializeSessionModels("cs-archived-project"); err != nil {
		t.Fatalf("初始化 Project 归档测试模型失败: %v", err)
	}
	work, err = fx.st.GetProject(work.ID)
	if err != nil {
		t.Fatalf("读取工作 Project 失败: %v", err)
	}
	if _, err := fx.st.ArchiveProject("usr-write", work.ID, work.Revision); err != nil {
		t.Fatalf("归档无活动工作的 Project 失败: %v", err)
	}
	assertConversationWritesRejected(t, svc, "cs-archived-project", store.ErrProjectArchived)
	assertConversationReadsAvailable(t, svc, "cs-archived-project")

	if _, err := svc.HandleMessage(context.Background(), "other-user",
		"cs-archived-project", "越权写入"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("其他用户写入应隐藏为 ErrSessionNotFound，实际: %v", err)
	}
}

func assertConversationWritesRejected(t *testing.T, svc *Service, sessionID string,
	want error) {
	t.Helper()
	if _, err := svc.HandleMessage(context.Background(), "usr-write", sessionID,
		"不能写入"); !errors.Is(err, want) {
		t.Fatalf("消息写入应返回 %v，实际: %v", want, err)
	}
	if _, err := svc.SetSessionAgentModel("usr-write", sessionID, "leader",
		"mock", "auto"); !errors.Is(err, want) {
		t.Fatalf("模型配置写入应返回 %v，实际: %v", want, err)
	}
	if _, err := svc.SetSessionExecutionPolicy("usr-write", sessionID,
		store.ExecutionModeAsk, false); !errors.Is(err, want) {
		t.Fatalf("执行/工具策略写入应返回 %v，实际: %v", want, err)
	}
}

func assertConversationReadsAvailable(t *testing.T, svc *Service, sessionID string) {
	t.Helper()
	if _, err := svc.ListSessionAgentModels("usr-write", sessionID); err != nil {
		t.Fatalf("模型配置读取不应被写门禁误伤: %v", err)
	}
	if _, err := svc.GetSessionExecutionPolicy("usr-write", sessionID); err != nil {
		t.Fatalf("执行策略读取不应被写门禁误伤: %v", err)
	}
}
