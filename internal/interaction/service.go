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

package interaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/log"
)

// EventTypeInteractionRequest 是交互请求下行事件的类型
// （envelope.type，channel 固定为 interaction）。
const EventTypeInteractionRequest = "interaction.request"

// EventTypeInteractionResolved 表示用户已经提交有效应答。Studio 收到该事件
// 后再结束 submitting 状态，不能在上行发送成功时本地提前改成 answered。
const EventTypeInteractionResolved = "interaction.resolved"

// defaultTimeout 是交互请求的默认超时（架构文档 12 §8：timeout_default 300s）。
const defaultTimeout = 300 * time.Second

var (
	// ErrNotFound 表示交互不存在（统一不暴露存在性细节）。
	ErrNotFound = errors.New("交互不存在")

	// ErrNotPending 表示交互已终结（重复应答/已过期/已取消）。
	ErrNotPending = errors.New("交互已终结，无法应答")
)

// Request 是一次交互请求的参数（v1 仅 confirm 类型）。
type Request struct {
	// SessionID 目标会话 ID（交互路由到该会话的属主用户）。
	SessionID string

	// Agent 发起交互的 Agent 名。
	Agent string

	// Question 给用户的问题（confirm 的确认文案）。
	Question string

	// Risk 风险等级（随请求下行，前端据此决定呈现级别）。
	Risk string

	// RunID 关联的 run session ID。
	RunID string

	// CheckpointID 关联的内核断点 ID。
	CheckpointID string

	// Timeout 等待应答的超时；≤0 时用默认 300s。超时按拒绝处理（on_timeout=reject）。
	Timeout time.Duration
}

// RequestPayload 是 interaction.request 下行事件的负载
// （架构文档 12 §3 confirm schema：interaction_id/question/risk/timeout_ts）。
type RequestPayload struct {
	// InteractionID 交互唯一标识（应答时回传）。
	InteractionID string `json:"interaction_id"`

	// Type 交互类型（confirm）。
	Type string `json:"type"`

	// Question 给用户的问题。
	Question string `json:"question"`

	// Risk 风险等级。
	Risk string `json:"risk"`

	// TimeoutTS 应答截止时间（Unix 秒）。
	TimeoutTS int64 `json:"timeout_ts"`
}

// replyPayload 是应答落库的 JSON 结构。
type replyPayload struct {
	// Approved 用户是否批准。
	Approved bool `json:"approved"`
}

// waitResult 是应答等待通道的消息：应答（approved）或取消（cancelled）。
// 为什么通道只发不关：close 与 Reply 的非阻塞发送存在 send-on-closed 竞态，
// 缓冲容量 1 的通道保证发送方永不阻塞，注销后到达的消息随通道被 GC。
type waitResult struct {
	// approved 用户是否批准（cancelled 为 false 时有效）。
	approved bool

	// cancelled 交互已被批量取消（会话删除/任务取消）。
	cancelled bool
}

// AnswerRouter starts a new continuation Run after an answer is persisted.
// Implementations must be idempotent by Interaction ID.
type AnswerRouter interface {
	RouteInteractionAnswer(context.Context, store.Interaction) error
}

// Service 是交互服务：confirm 请求的发起、等待、应答、超时、取消。
// 等待方（Request）与应答方（Reply）可能分属不同 goroutine
// （run 执行流 vs WS 上行），经进程内 waiter 表衔接，状态以 store 为准。
type Service struct {
	// st 元数据存储（交互记录的唯一事实源）。
	st *store.Store

	// bus 事件总线（interaction.request 下行出口）。
	bus *event.Bus

	// logger 结构化日志器。
	logger *log.Logger

	// mu 保护 waiters。
	mu sync.Mutex

	// waiters 等待中的应答通道（interaction_id → chan），容量 1。
	waiters map[string]chan waitResult

	answerRouter AnswerRouter
}

// NewService 创建交互服务。
func NewService(st *store.Store, bus *event.Bus, logger *log.Logger) *Service {
	return &Service{st: st, bus: bus, logger: logger, waiters: make(map[string]chan waitResult)}
}

func (s *Service) SetAnswerRouter(router AnswerRouter) {
	s.mu.Lock()
	s.answerRouter = router
	s.mu.Unlock()
}

func (s *Service) routeInteractionAnswer(ctx context.Context, value store.Interaction) error {
	s.mu.Lock()
	router := s.answerRouter
	s.mu.Unlock()
	if router == nil {
		if value.WorkflowID == "" && value.TaskID == "" {
			return s.st.MarkInteractionHandled(value.ID, time.Now().UTC(), "")
		}
		return fmt.Errorf("Interaction AnswerRouter is not configured")
	}
	if err := router.RouteInteractionAnswer(ctx, value); err != nil {
		_ = s.st.RecordInteractionHandlingError(value.ID, err.Error())
		return err
	}
	return s.st.MarkInteractionHandled(value.ID, time.Now().UTC(), "")
}

// RecoverAnsweredInteractions routes answered records left incomplete by a restart.
func (s *Service) RecoverAnsweredInteractions(ctx context.Context) error {
	values, err := s.st.ListUnhandledAnsweredInteractions()
	if err != nil {
		return err
	}
	var first error
	for _, value := range values {
		if err := s.routeInteractionAnswer(ctx, value); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// AuthorizeReply 校验旧 /ws/chat 应答的连接范围。interaction_id 只负责
// 定位记录，不能替代 Conversation 与用户归属校验；任一归属不匹配都按
// 不存在处理，既阻止跨会话应答，也不向调用方暴露目标记录是否存在。
func (s *Service) AuthorizeReply(_ context.Context, userID, sessionID, interactionID string) error {
	interaction, err := s.st.GetInteraction(interactionID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if sessionID == "" || interaction.SessionID != sessionID {
		return ErrNotFound
	}
	session, err := s.st.GetChatSession(interaction.SessionID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if session.UserID != userID {
		return ErrNotFound
	}
	return nil
}

// Request 发起一次 confirm 请求并等待应答：
// 创建 Pending 记录 → 经事件总线下行 interaction.request → 等待应答/超时/取消。
// 返回用户是否批准；超时按拒绝处理（on_timeout=reject，状态迁移 Expired），
// ctx 取消时状态迁移 Cancelled 并返回 ctx 错误。
func (s *Service) Request(ctx context.Context, req Request) (bool, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	now := time.Now().UTC()
	deadline := now.Add(timeout)

	payload := RequestPayload{
		InteractionID: store.NewInteractionID(),
		Type:          store.InteractionTypeConfirm,
		Question:      req.Question,
		Risk:          req.Risk,
		TimeoutTS:     deadline.Unix(),
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("序列化交互负载失败: %w", err)
	}

	// 先落 Pending 记录再下行请求：应答可能随时到达，
	// Reply 以记录存在且 pending 为前提，先落库保证应答永不落空。
	if err := s.st.CreateInteraction(store.Interaction{
		ID: payload.InteractionID, SessionID: req.SessionID, Agent: req.Agent,
		Type: store.InteractionTypeConfirm, Status: store.InteractionStatusPending,
		Payload: string(payloadJSON), RunID: req.RunID, CheckpointID: req.CheckpointID,
		CreatedAt: now,
	}); err != nil {
		return false, err
	}
	interaction, err := s.st.GetInteraction(payload.InteractionID)
	if err != nil {
		return false, fmt.Errorf("读取刚创建的交互失败: %w", err)
	}
	ch := s.register(payload.InteractionID)
	defer s.unregister(payload.InteractionID)

	env := ws.NewEnvelope(req.SessionID, ws.ChannelInteraction, EventTypeInteractionRequest,
		ws.ImportanceCritical, payload)
	s.decorateEnvelope(&env, interaction)
	s.bus.Publish(event.TopicAgentEvents, env)
	s.logger.Info("交互请求已发起", "interaction_id", payload.InteractionID,
		"session_id", req.SessionID, "agent", req.Agent, "run_id", req.RunID,
		"risk", req.Risk, "timeout", timeout.String())

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-ch:
		if result.cancelled {
			s.logger.Info("交互请求已取消", "interaction_id", payload.InteractionID)
			return false, nil
		}
		s.logger.Info("交互请求已应答", "interaction_id", payload.InteractionID, "approved", result.approved)
		return result.approved, nil
	case <-timer.C:
		// 超时按拒绝处理：状态迁移 Expired（条件更新——应答可能恰好先到，
		// 此时以已落库的应答为准，重新读取返回真实结果）。
		if err := s.st.ExpireInteraction(payload.InteractionID, time.Now().UTC()); err != nil {
			if errors.Is(err, store.ErrNotPending) {
				return s.answeredResult(payload.InteractionID)
			}
			return false, err
		}
		s.logger.Info("交互请求超时，按拒绝处理", "interaction_id", payload.InteractionID,
			"timeout", timeout.String())
		return false, nil
	case <-ctx.Done():
		// 调用方取消（任务取消等）：状态迁移 Cancelled（已被应答则以应答为准）。
		if err := s.st.CancelInteraction(payload.InteractionID, time.Now().UTC()); err != nil && !errors.Is(err, store.ErrNotPending) {
			s.logger.WithError(err).Warn("取消交互失败", "interaction_id", payload.InteractionID)
		}
		s.logger.Info("交互请求已取消", "interaction_id", payload.InteractionID)
		return false, ctx.Err()
	}
}

// Reply 处理一次用户应答（WS 上行 interaction.reply 的入口）：
// 状态校验（存在且 pending）→ 应答落库 → 唤醒等待方。
// 为什么先落库再唤醒：等待方被唤醒后即恢复执行，
// 应答必须先成为持久事实（先落应答再恢复执行）。
func (s *Service) Reply(_ context.Context, interactionID string, approved bool) error {
	if _, err := s.st.GetInteraction(interactionID); errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	} else if err != nil {
		return err
	}

	reply, err := json.Marshal(replyPayload{Approved: approved})
	if err != nil {
		return fmt.Errorf("序列化应答失败: %w", err)
	}
	if err := s.st.AnswerInteraction(interactionID, string(reply), time.Now().UTC()); errors.Is(err, store.ErrNotPending) {
		return ErrNotPending
	} else if err != nil {
		return err
	}
	interaction, err := s.st.GetInteraction(interactionID)
	if err != nil {
		return err
	}
	resolved := ws.NewEnvelope(interaction.SessionID, ws.ChannelInteraction,
		EventTypeInteractionResolved, ws.ImportanceCritical, map[string]any{
			"interaction_id": interaction.ID,
			"status":         interaction.Status,
			"reply":          replyPayload{Approved: approved},
			"revision":       interaction.Revision,
		})
	s.decorateEnvelope(&resolved, interaction)
	s.bus.Publish(event.TopicAgentEvents, resolved)

	s.mu.Lock()
	ch, ok := s.waiters[interactionID]
	s.mu.Unlock()
	if ok {
		// 容量 1 的缓冲通道：等待方必然能收到（超时/取消路径已注销 waiter，
		// 注销后到达的应答只落库不唤醒——等待方以落库状态为准兜底）。
		select {
		case ch <- waitResult{approved: approved}:
		default:
		}
	}
	return nil
}

// decorateEnvelope 补齐 Project、资源版本和 Run/Trace 关联。Interaction 的
// 记录已经先落库，因此这些字段与 Snapshot 中的同一条记录保持一致。
func (s *Service) decorateEnvelope(envelope *ws.Envelope, interaction store.Interaction) {
	envelope.ProjectID = interaction.ProjectID
	envelope.ResourceType = "interaction"
	envelope.ResourceID = interaction.ID
	envelope.Revision = interaction.Revision
	envelope.Agent = ws.AgentRef{ID: interaction.Agent, Name: interaction.Agent}
	envelope.Parent = ws.ParentRef{RunID: interaction.RunID}
	if interaction.RunID == "" {
		return
	}
	if run, err := s.st.GetRunSession(interaction.RunID); err == nil {
		envelope.Parent.TraceID = run.TraceID
	}
}

// CancelSession 批量取消会话的全部待应答交互（任务取消/会话删除路径）：
// 先落库再唤醒——被唤醒的等待方以落库的终态为准。
func (s *Service) CancelSession(_ context.Context, sessionID string) error {
	pending, err := s.st.ListPendingInteractions(sessionID)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	if _, err := s.st.CancelPendingInteractions(sessionID, time.Now().UTC()); err != nil {
		return err
	}
	for _, it := range pending {
		s.mu.Lock()
		ch, ok := s.waiters[it.ID]
		s.mu.Unlock()
		if ok {
			select {
			case ch <- waitResult{cancelled: true}:
			default:
			}
		}
		s.logger.Info("交互请求已随会话取消", "interaction_id", it.ID, "session_id", sessionID)
	}
	return nil
}

// answeredResult 读取已落库的应答结果（超时与应答竞速时的兜底路径）。
func (s *Service) answeredResult(interactionID string) (bool, error) {
	it, err := s.st.GetInteraction(interactionID)
	if err != nil {
		return false, err
	}
	if it.Status != store.InteractionStatusAnswered {
		return false, nil
	}
	var reply replyPayload
	if err := json.Unmarshal([]byte(it.Reply), &reply); err != nil {
		return false, fmt.Errorf("解析交互 %q 的应答失败: %w", interactionID, err)
	}
	return reply.Approved, nil
}

// register 注册应答等待通道。
func (s *Service) register(interactionID string) chan waitResult {
	ch := make(chan waitResult, 1)
	s.mu.Lock()
	s.waiters[interactionID] = ch
	s.mu.Unlock()
	return ch
}

// unregister 注销应答等待通道（Request 返回前调用，防 waiter 表泄漏）。
func (s *Service) unregister(interactionID string) {
	s.mu.Lock()
	delete(s.waiters, interactionID)
	s.mu.Unlock()
}
