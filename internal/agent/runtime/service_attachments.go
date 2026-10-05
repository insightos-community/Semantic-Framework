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
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/store"
)

// loadedContext 是一次 Leader Run 装配出的模型输入历史及其摘要版本。
// Summary 的 revision 随 Run context 传给 Eino Finalize，防止慢摘要覆盖新值。
type loadedContext struct {
	Messages []*schema.Message
	Summary  store.ContextSummary
}

// loadHistory 保留原调用面，测试和只读诊断可直接取得模型输入历史。
func (s *Service) loadHistory(sessionID string, includeImageContent bool) ([]*schema.Message, error) {
	loaded, err := s.loadContextHistory(sessionID, includeImageContent)
	return loaded.Messages, err
}

// loadContextHistory 只装配 Project Memory、已保存摘要和摘要边界后的近期
// Transcript。Transcript 本身完整保留在 Store，不再每轮把边界前原文重新
// 读入模型，也不会让 Eino 对同一段历史反复生成摘要。
func (s *Service) loadContextHistory(sessionID string, includeImageContent bool, recipientIDs ...string) (loadedContext, error) {
	recipientID := ""
	if len(recipientIDs) > 0 {
		recipientID = recipientIDs[0]
	}
	session, err := s.st.GetChatSession(sessionID)
	if err != nil {
		return loadedContext{}, err
	}
	memory, err := s.st.GetProjectMemory(session.ProjectID)
	if err != nil {
		return loadedContext{}, err
	}
	summary, err := s.st.GetContextSummaryBySession(sessionID)
	if errors.Is(err, store.ErrNotFound) {
		summary = store.ContextSummary{}
	} else if err != nil {
		return loadedContext{}, err
	}

	records, err := s.st.ListChatMessagesAfter(sessionID,
		summary.CoveredThroughMessageID, 0)
	if err != nil {
		return loadedContext{}, err
	}
	history := make([]*schema.Message, 0, len(records)+2)
	if content := strings.TrimSpace(memory.Content); content != "" {
		history = append(history, schema.SystemMessage(
			"Project Memory（由用户在 Studio 中维护）：\n\n"+content))
	}
	if content := strings.TrimSpace(summary.Summary); content != "" {
		// Eino DefaultFinalize 产出的摘要本身是 User 消息；保持相同角色，
		// 下一次触发压缩时可以在已有摘要基础上继续累积。
		history = append(history, schema.UserMessage(content))
	}
	for _, rec := range records {
		if rec.Message == nil {
			return loadedContext{}, fmt.Errorf("会话 %q 的消息 %q 缺少 schema.Message", sessionID, rec.ID)
		}
		message := *rec.Message
		switch message.Role {
		case schema.Assistant:
			if strings.TrimSpace(message.Content) == "" && len(message.ToolCalls) == 0 {
				continue
			}
			if recipientID != "" && rec.AgentID != "" && rec.AgentID != recipientID {
				// 其他 Agent 的公开回复是协作资料，不导入它的工具协议或 Task 上下文。
				message = *schema.UserMessage("[Agent " + rec.AgentID + " 的公开回复]\n" + message.Content)
			}
		case schema.User:
			if len(rec.ArtifactRefs) > 0 {
				parts, getErr := s.artifactInputParts(message.Content,
					rec.ArtifactRefs, includeImageContent)
				if getErr != nil {
					return loadedContext{}, getErr
				}
				message.Content = ""
				message.UserInputMultiContent = parts
			}
		default:
			return loadedContext{}, fmt.Errorf(
				"会话 %q 的消息 %q 角色 %q 非法", sessionID, rec.ID, message.Role)
		}
		history = append(history, &message)
	}
	return loadedContext{Messages: history, Summary: summary}, nil
}

func hasCapability(capabilities []string, target string) bool {
	for _, capability := range capabilities {
		if capability == target {
			return true
		}
	}
	return false
}

// resolveAttachments 校验图片归属，返回最新用户 schema.Message 与前端展示
// 引用。消息中的 Base64 仅用于本轮模型调用，持久化时只保存 Artifact ID。
func (s *Service) resolveAttachments(userID string,
	text string, ids []string) (*schema.Message, []MessageAttachment, error) {
	if len(ids) > 4 {
		return nil, nil, errors.New("每条消息最多发送 4 张图片")
	}
	seen := make(map[string]struct{}, len(ids))
	views := make([]MessageAttachment, 0, len(ids))
	parts := make([]schema.MessageInputPart, 0, len(ids)+1)
	if text != "" {
		parts = append(parts, schema.MessageInputPart{Type: schema.ChatMessagePartTypeText, Text: text})
	}
	for _, id := range ids {
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		artifact, content, err := s.st.GetArtifact(id)
		if err != nil || artifact.OwnerID != userID ||
			!strings.HasPrefix(artifact.MediaType, "image/") {
			return nil, nil, fmt.Errorf("图片 %q 不存在或不属于当前用户", id)
		}
		encoded := base64.StdEncoding.EncodeToString(content)
		parts = append(parts, schema.MessageInputPart{
			Type: schema.ChatMessagePartTypeImageURL,
			Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{
				Base64Data: &encoded, MIMEType: artifact.MediaType,
			}},
		})
		views = append(views, MessageAttachment{
			ID: id, Name: artifact.Summary, MediaType: artifact.MediaType,
			Size: artifact.Size, ContentURL: "/api/v1/chat/attachments/" + id,
		})
	}
	if len(views) == 0 {
		return schema.UserMessage(text), views, nil
	}
	return &schema.Message{Role: schema.User, UserInputMultiContent: parts}, views, nil
}

// artifactInputParts 按 Artifact ID 读取图片并组装 Eino 多模态输入。
func (s *Service) artifactInputParts(text string, ids []string,
	includeImageContent bool) ([]schema.MessageInputPart, error) {
	parts := make([]schema.MessageInputPart, 0, len(ids)+1)
	if text != "" {
		parts = append(parts, schema.MessageInputPart{Type: schema.ChatMessagePartTypeText, Text: text})
	}
	for _, id := range ids {
		// 先只读取元数据。非图片或非视觉上下文不应为了生成引用而读取
		// 文件本体；只有确实要构造图片 part 时才按需加载二进制。
		artifact, err := s.st.GetArtifactMeta(id)
		if errors.Is(err, store.ErrNotFound) {
			// 强制删除只移除 Artifact 本体，历史消息仍可继续进入上下文；
			// 明确缺失引用比让整段历史加载失败更符合可恢复语义。
			parts = append(parts, schema.MessageInputPart{Type: schema.ChatMessagePartTypeText,
				Text: "[ArtifactRef 已缺失: artifact://" + id + "]"})
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("读取历史 Artifact %q 失败: %w", id, err)
		}
		if !strings.HasPrefix(artifact.MediaType, "image/") || !includeImageContent {
			// 非图片 Artifact 始终只放引用；切换到非视觉模型后，历史图片也
			// 降为引用，避免旧图片让新端点请求失败。Artifact 本体仍可委派
			// 给具备视觉能力的 SubAgent 按需读取。
			kind := "ArtifactRef"
			if strings.HasPrefix(artifact.MediaType, "image/") {
				kind = "图片 ArtifactRef；当前 Agent 不直接读取图片，可委派给视觉 Agent"
			}
			parts = append(parts, schema.MessageInputPart{Type: schema.ChatMessagePartTypeText,
				Text: fmt.Sprintf("[%s: %s, 类型: %s, 摘要: %s]",
					kind, artifact.URI, artifact.MediaType, artifact.Summary)})
			continue
		}
		_, content, err := s.st.GetArtifact(id)
		if errors.Is(err, store.ErrNotFound) {
			parts = append(parts, schema.MessageInputPart{Type: schema.ChatMessagePartTypeText,
				Text: "[ArtifactRef 已缺失: artifact://" + id + "]"})
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("读取历史 Artifact %q 失败: %w", id, err)
		}
		encoded := base64.StdEncoding.EncodeToString(content)
		parts = append(parts, schema.MessageInputPart{
			Type: schema.ChatMessagePartTypeImageURL,
			Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{
				Base64Data: &encoded, MIMEType: artifact.MediaType,
			}},
		})
	}
	return parts, nil
}

// attachmentReferenceMessage 为非视觉 Agent 构造只含文本引用的当前用户消息。
// 运行 context 仍持有经过归属校验的 ArtifactRef，因此 AgentTool 可以安全地
// 把图片本体交给目标视觉 Agent，而不会把 Base64 泄漏给当前文本模型。
func attachmentReferenceMessage(text string, attachments []MessageAttachment) *schema.Message {
	parts := make([]schema.MessageInputPart, 0, len(attachments)+1)
	if text != "" {
		parts = append(parts, schema.MessageInputPart{Type: schema.ChatMessagePartTypeText, Text: text})
	}
	for _, attachment := range attachments {
		parts = append(parts, schema.MessageInputPart{Type: schema.ChatMessagePartTypeText,
			Text: fmt.Sprintf("[图片 ArtifactRef；当前 Agent 不直接读取图片，可委派给视觉 Agent: artifact://%s, 类型: %s, 摘要: %s]",
				attachment.ID, attachment.MediaType, attachment.Name)})
	}
	return &schema.Message{Role: schema.User, UserInputMultiContent: parts}
}
