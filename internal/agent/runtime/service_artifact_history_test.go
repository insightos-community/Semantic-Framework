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
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/store"
)

// TestLoadHistoryUsesArtifactReferences 验证非图片 Artifact 只以摘要和引用进入
// 上下文，强制删除后的引用变为缺失标记，不会让整个会话无法继续。
func TestLoadHistoryUsesArtifactReferences(t *testing.T) {
	fx := newTestFixture(t)
	svc := NewService(Deps{Store: fx.st, Logger: fx.logger})
	now := time.Now().UTC()
	if err := fx.st.CreateChatSession(store.ChatSession{ID: "cs-artifact-history", UserID: "usr-1",
		Title: "Artifact 历史", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	report, err := fx.st.PutUserArtifact("usr-1", "application/json", "三行数据报告", "{}", []byte(`{"rows":3}`))
	if err != nil {
		t.Fatalf("创建报告 Artifact 失败: %v", err)
	}
	missing, err := fx.st.PutUserArtifact("usr-1", "image/png", "待删除图片", "{}", []byte("png"))
	if err != nil {
		t.Fatalf("创建图片 Artifact 失败: %v", err)
	}
	visible, err := fx.st.PutUserArtifact("usr-1", "image/png", "保留图片", "{}", []byte("png-live"))
	if err != nil {
		t.Fatalf("创建保留图片 Artifact 失败: %v", err)
	}
	for index, item := range []struct {
		text string
		ref  string
	}{{"查看报告", report.ID}, {"查看图片", missing.ID}, {"继续查看图片", visible.ID}} {
		if err := fx.st.AppendChatMessage(store.ChatMessage{ID: store.NewChatMessageID(),
			SessionID: "cs-artifact-history", Message: schema.UserMessage(item.text),
			ArtifactRefs: []string{item.ref}, CreatedAt: now.Add(time.Duration(index) * time.Second)}); err != nil {
			t.Fatalf("写入历史消息失败: %v", err)
		}
	}
	if err := fx.st.DeleteArtifact(missing.ID); err != nil {
		t.Fatalf("删除测试 Artifact 失败: %v", err)
	}

	history, err := svc.loadHistory("cs-artifact-history", true)
	if err != nil || len(history) != 3 {
		t.Fatalf("历史加载应成功: len=%d err=%v", len(history), err)
	}
	firstText := history[0].UserInputMultiContent[1].Text
	if !strings.Contains(firstText, report.URI) || !strings.Contains(firstText, "三行数据报告") {
		t.Fatalf("非图片 Artifact 应只注入引用和摘要: %q", firstText)
	}
	missingText := history[1].UserInputMultiContent[1].Text
	if !strings.Contains(missingText, "已缺失") || !strings.Contains(missingText, missing.ID) {
		t.Fatalf("被删除 Artifact 应显示缺失标记: %q", missingText)
	}
	if history[2].UserInputMultiContent[1].Image == nil {
		t.Fatalf("视觉模型历史应恢复图片本体: %+v", history[2].UserInputMultiContent)
	}

	// 跨模型切到非视觉端点时，同一历史仍保留，但旧图片降为稳定引用，
	// 不能继续把多模态 part 发送给不支持图片的 Provider。
	textHistory, err := svc.loadHistory("cs-artifact-history", false)
	if err != nil || len(textHistory) != 3 {
		t.Fatalf("非视觉历史加载应成功: len=%d err=%v", len(textHistory), err)
	}
	imagePart := textHistory[2].UserInputMultiContent[1]
	if imagePart.Image != nil || !strings.Contains(imagePart.Text, visible.URI) ||
		!strings.Contains(imagePart.Text, "可委派给视觉 Agent") {
		t.Fatalf("非视觉历史图片应降为可委派引用: %+v", imagePart)
	}
}
