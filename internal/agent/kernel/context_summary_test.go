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

package kernel

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/log"
)

// TestPersistentSummaryRevisionConflictKeepsStoredVersion 验证并发 Run 使用旧
// revision 保存摘要时不会覆盖较新的边界，也不会让当前模型调用失败。
func TestPersistentSummaryRevisionConflictKeepsStoredVersion(t *testing.T) {
	st := openKernelTestStore(t)
	project, err := st.EnsureDefaultProject("summary-user")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	session := store.ChatSession{
		ID: store.NewChatSessionID(), UserID: "summary-user", ProjectID: project.ID,
		Title: "摘要冲突", CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateChatSession(session); err != nil {
		t.Fatal(err)
	}
	boundaryID := store.NewChatMessageID()
	if err := st.AppendChatMessage(store.ChatMessage{
		ID: boundaryID, SessionID: session.ID, Message: schema.UserMessage("第一轮"),
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := st.SaveContextSummary(store.ContextSummary{
		ContextID: "leader:" + session.ID, ProjectID: project.ID,
		SessionID: session.ID, Summary: "已经生效的摘要",
		CoveredThroughMessageID: boundaryID,
	}, 0)
	if err != nil || stored.Revision != 1 {
		t.Fatalf("准备已生效摘要失败: summary=%+v err=%v", stored, err)
	}

	ctx := WithSummaryPersistence(context.Background(), SummaryPersistence{
		ContextID: stored.ContextID, ProjectID: project.ID, SessionID: session.ID,
		CoveredThroughMessageID: boundaryID,
		ExpectedRevision:        0, // 模拟另一条 Run 在旧 revision 上生成完摘要。
	})
	finalize := persistentSummaryFinalizer(st,
		log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	finalMessages, err := finalize(ctx,
		[]*schema.Message{schema.UserMessage("第一轮")},
		schema.AssistantMessage("过期 Run 生成的摘要", nil))
	if err != nil || len(finalMessages) == 0 {
		t.Fatalf("revision 冲突不应中断本轮: messages=%d err=%v", len(finalMessages), err)
	}

	actual, err := st.GetContextSummary(stored.ContextID)
	if err != nil {
		t.Fatal(err)
	}
	if actual.Revision != stored.Revision || actual.Summary != stored.Summary ||
		actual.CoveredThroughMessageID != stored.CoveredThroughMessageID {
		t.Fatalf("旧 Run 不得覆盖已生效摘要: got=%+v want=%+v", actual, stored)
	}
}
