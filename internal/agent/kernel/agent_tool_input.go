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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/store"
)

// runArtifactRefsKey 是当前用户消息已验证 ArtifactRef 的 context 键。使用
// 私有类型避免与 Eino 或业务调用方的 context value 冲突。
type runArtifactRefsKey struct{}

// WithRunArtifactRefs 把当前用户消息中已经完成归属校验的 ArtifactRef 传给
// AgentTool。子 Agent 只允许接收这一白名单中的引用，不能凭模型生成的 ID
// 越权读取其他用户 Artifact。
func WithRunArtifactRefs(ctx context.Context, refs []string) context.Context {
	copyRefs := append([]string(nil), refs...)
	return context.WithValue(ctx, runArtifactRefsKey{}, copyRefs)
}

// runArtifactRefs 读取白名单副本，调用方修改不会污染父运行上下文。
func runArtifactRefs(ctx context.Context) []string {
	refs, _ := ctx.Value(runArtifactRefsKey{}).([]string)
	return append([]string(nil), refs...)
}

// agentToolInputMiddleware 把 Eino AgentTool 自定义 schema 产生的原始 JSON
// 用户消息转换为干净任务文本，并按需读取图片 Artifact 形成多模态输入。
type agentToolInputMiddleware struct {
	adk.BaseChatModelAgentMiddleware
	store *store.Store
}

// BeforeModelRewriteState 只处理最后一条尚未转换的 AgentTool 用户消息。
// 后续模型轮次中该消息已经是文本或 MultiContent，因此天然幂等。
func (m *agentToolInputMiddleware) BeforeModelRewriteState(ctx context.Context,
	state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	if state == nil || len(state.Messages) == 0 {
		return ctx, state, nil
	}
	last := state.Messages[len(state.Messages)-1]
	if last == nil || last.Role != schema.User || last.Content == "" ||
		len(last.UserInputMultiContent) > 0 {
		return ctx, state, nil
	}
	var input delegationArgs
	if err := json.Unmarshal([]byte(last.Content), &input); err != nil {
		return ctx, state, nil
	}
	if strings.TrimSpace(input.Task) == "" {
		return ctx, state, fmt.Errorf("SubAgent 委派任务不能为空")
	}

	message := *last
	if len(input.ArtifactRefs) == 0 {
		message.Content = input.Task
	} else {
		if m.store == nil {
			return ctx, state, fmt.Errorf("SubAgent 收到 ArtifactRef，但未配置 Artifact Store")
		}
		parts := []schema.MessageInputPart{{Type: schema.ChatMessagePartTypeText, Text: input.Task}}
		for _, id := range input.ArtifactRefs {
			artifact, content, err := m.store.GetArtifact(id)
			if err != nil {
				return ctx, state, fmt.Errorf("读取委派 Artifact %q 失败: %w", id, err)
			}
			if !strings.HasPrefix(artifact.MediaType, "image/") {
				return ctx, state, fmt.Errorf("委派 Artifact %q 不是图片，当前只支持多模态图片转发", id)
			}
			encoded := base64.StdEncoding.EncodeToString(content)
			parts = append(parts, schema.MessageInputPart{
				Type: schema.ChatMessagePartTypeImageURL,
				Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{
					Base64Data: &encoded, MIMEType: artifact.MediaType,
				}},
			})
		}
		message.Content = ""
		message.UserInputMultiContent = parts
	}

	messages := append([]*schema.Message(nil), state.Messages...)
	messages[len(messages)-1] = &message
	next := *state
	next.Messages = messages
	return ctx, &next, nil
}
