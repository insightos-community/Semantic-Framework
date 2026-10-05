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
	"time"

	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
)

// runEventContext 是一次 Run 产生的消息、工具和 SubAgent 事件的公共关联。
// 它只保存不会随状态迁移改变的字段，资源 revision 由 Run 状态事件单独提供。
type runEventContext struct {
	ProjectID string
	SessionID string
	RunID     string
	TraceID   string
	AgentID   string
}

func eventContextFromRun(run store.RunSession) runEventContext {
	return runEventContext{
		ProjectID: run.ProjectID, SessionID: run.ChatSessionID,
		RunID: run.ID, TraceID: run.TraceID, AgentID: run.AgentID,
	}
}

// finishRun 先用条件更新确定唯一 Run 终态，再持久化可进入下一轮上下文的
// Assistant 消息。终态事件只会在本方法返回后发布，因此 Studio 看到终态时
// 对应消息已经可读；先更新状态还能避免条件迁移失败时重复写入 Assistant。
func (s *Service) finishRun(rt *sessionRuntime, run store.RunSession, status, assistantText string,
	metadata RunMetadata) (store.RunSession, error) {
	now := time.Now().UTC()
	from := []string{store.RunStatusRunning, store.RunStatusWaitingInput}
	switch status {
	case store.RunStatusCancelled, store.RunStatusFailed:
		from = append(from, store.RunStatusCancelling)
	}
	finished, err := s.st.FinishRunSession(run.ID, from, status, metadata.Error, now)
	if err != nil {
		return store.RunSession{}, err
	}
	if s.directRobot != nil && run.Kind == store.RunKindConversation {
		if err := s.directRobot.FinishDirectRun(context.Background(), run.ID); err != nil {
			s.logger.WithError(err).Warn("对话 Robot 执行仍待收敛", "run_id", run.ID)
		}
	}
	metadata.Status = finished.Status
	metadata.StartedAt = run.StartedAt.Format(time.RFC3339Nano)
	encoded, err := json.Marshal(metadata)
	if err != nil {
		s.logger.WithError(err).Error("运行活动序列化失败", "run_id", run.ID)
		encoded = []byte("{}")
	}
	// 只有满足模型协议的 Assistant 消息才进入历史。运行活动仍保存在 Run
	// 元数据和 Trace 中，不能为了展示工具活动写入空 Assistant。
	if assistantText != "" {
		var provider, endpoint, model string
		if metadata.Model != nil {
			provider = metadata.Model.Provider
			endpoint, model = metadata.Model.ResolvedEndpoint, metadata.Model.ResolvedModel
		}
		if err := s.st.AppendChatMessage(store.ChatMessage{
			ID: store.NewChatMessageID(), SessionID: run.ChatSessionID,
			AgentID: run.AgentID, RunID: run.ID, TraceID: run.TraceID,
			Provider: provider, Endpoint: endpoint, Model: model,
			Message:  schema.AssistantMessage(assistantText, nil),
			Metadata: string(encoded), CreatedAt: now,
		}); err != nil {
			s.logger.WithError(err).Error(
				"助手消息落库失败", "run_id", run.ID, "session_id", run.ChatSessionID)
		}
	}

	return finished, nil
}

// finishCompletedRun 处理“模型已经返回 Done，用户取消同时到达”的竞速。
// completed 只允许从 running/waiting_input 写入；若条件更新失败且 Store 已经
// 是 cancelling，说明取消请求先成功持久化，此时取消优先并立即收敛为
// cancelled。其他非法状态仍原样返回，避免覆盖真正的状态错误。
func (s *Service) finishCompletedRun(rt *sessionRuntime, run store.RunSession,
	assistantText string, metadata RunMetadata) (store.RunSession, error) {
	finished, err := s.finishRun(rt, run, store.RunStatusCompleted, assistantText, metadata)
	if !errors.Is(err, store.ErrInvalidState) {
		return finished, err
	}
	current, getErr := s.st.GetRunSession(run.ID)
	if getErr != nil {
		return store.RunSession{}, errors.Join(err, getErr)
	}
	if current.Status != store.RunStatusCancelling {
		return store.RunSession{}, err
	}
	metadata.Status = store.RunStatusCancelled
	metadata.Error = ""
	return s.finishRun(rt, current, store.RunStatusCancelled, assistantText, metadata)
}

func usagePayload(usage kernel.Usage) *UsagePayload {
	return &UsagePayload{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
	}
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return text
}

func boundedToolResult(text string) (string, bool) {
	runes := []rune(text)
	if len(runes) <= toolResultRuneLimit {
		return text, false
	}
	return string(runes[:toolResultRuneLimit]), true
}

// publishRunState 发布 Store 已确认的 Run 状态。sequence 不在 Runtime
// 分配，由 Aggregator 落库时按 Project 连续生成。
func (s *Service) publishRunState(run store.RunSession, rt *sessionRuntime, typ string) {
	env := ws.NewEnvelope(run.ChatSessionID, ws.ChannelDialogue, typ,
		ws.ImportanceNormal, map[string]any{"run": run})
	env.ProjectID = run.ProjectID
	env.ResourceType = "agent_run"
	env.ResourceID = run.ID
	env.Revision = run.Revision
	env.Parent = ws.ParentRef{RunID: run.ID, TraceID: run.TraceID}
	env.Agent = ws.AgentRef{ID: run.AgentID, Name: run.AgentName}
	if rt != nil && rt.profile != nil {
		// AgentID 是 Team 成员实例 ID，不能被角色 profile 名覆盖。同一角色
		// 可以在 Team 中声明多个实例，Studio 与 Trace 必须据此准确归属。
		env.Agent.Role = string(rt.profile.Mode)
		if env.Agent.Name == "" {
			env.Agent.Name = rt.profile.Name
		}
	}
	s.bus.Publish(event.TopicAgentEvents, env)
}

// publishRuntimeEnvelope 根据事件是否可由最终消息重建选择投递方式。
// 只有纯流式增量允许在订阅者积压时丢弃；Run/Interaction 终态、Tool
// 调用结果和 message.done 都必须进入可靠队列，供 Aggregator 落库与回放。
func (s *Service) publishRuntimeEnvelope(typ string, env ws.Envelope) {
	switch typ {
	case EventTypeMessageDelta, EventTypeReasoningDelta, EventTypeSubAgentDelta:
		s.bus.PublishDroppable(event.TopicAgentEvents, env)
	default:
		s.bus.Publish(event.TopicAgentEvents, env)
	}
}

func (s *Service) publish(ref runEventContext, rt *sessionRuntime, typ string, payload any) {
	env := ws.NewEnvelope(ref.SessionID, ws.ChannelDialogue, typ, ws.ImportanceNormal, payload)
	env.ProjectID = ref.ProjectID
	env.Parent = ws.ParentRef{RunID: ref.RunID, TraceID: ref.TraceID}
	agentID := ref.AgentID
	if agentID == "" {
		agentID = rt.profile.Name
	}
	env.Agent = ws.AgentRef{ID: agentID,
		Role: string(rt.profile.Mode), Name: rt.profile.Name}
	s.publishRuntimeEnvelope(typ, env)
}

func (s *Service) publishSubAgent(ref runEventContext, agentID, typ string, payload any) {
	name := agentID
	if s.subAgents != nil {
		if def, ok := s.subAgents.Get(agentID); ok {
			name = def.Role
		}
	}
	env := ws.NewEnvelope(ref.SessionID, ws.ChannelDialogue, typ, ws.ImportanceNormal, payload)
	env.ProjectID = ref.ProjectID
	env.Parent = ws.ParentRef{RunID: ref.RunID, TraceID: ref.TraceID}
	env.Agent = ws.AgentRef{ID: agentID, Role: string(profile.ModeService), Name: name}
	s.publishRuntimeEnvelope(typ, env)
}

func titleFrom(text string) string {
	runes := []rune(text)
	if len(runes) > titleRuneLimit {
		return string(runes[:titleRuneLimit])
	}
	return text
}
