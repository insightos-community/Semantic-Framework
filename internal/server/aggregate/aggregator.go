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

package aggregate

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/log"
)

// seqWidth 是事件 ID 序列段的十进制宽度（零填充保证字典序 = 数值序）。
// 宽度 9 位：单进程 10 亿事件内字典序严格单调，超出后序列段进位变宽、
// 字典序保证失效——对 v1 的进程生命周期足够，扩容时整体加宽。
const seqWidth = 9

// defaultAgentID 是事件无来源 Agent 时的兜底标识（系统/服务自身事件）。
const defaultAgentID = "system"

// Sequencer 是事件 ID 的发放器：聚合器单点使用，保证 ID 字典序严格等于
// 发放序（断连续传"按 id 排序补发"的正确性基础）。
//
// 为什么用"时间戳+序列"而不是 ULID：
//   - ULID 同毫秒内的随机段不保证单调（单调扩展依赖实现的熵递增细节），
//     而本系统的补发契约要求"同会话内 id 字典序 = 事件到达序"严格成立；
//   - 引入进程内原子序列后，同纳秒/时钟回拨都由序列段兜底，保证不依赖
//     任何时钟假设；
//   - 不引入新依赖（与 store 消息 ID 的选型理由一致）。
//
// 格式：evt-<19 位纳秒时间戳>-<9 位序列>。跨进程重启靠纳秒时间戳前进
// 保持单调（时钟大幅回拨是已知边界，见 changelog TODO）。
type Sequencer struct {
	// seq 进程内原子序列（发放顺序的唯一权威）。
	seq atomic.Uint64
}

// Next 发放下一个事件 ID。
func (s *Sequencer) Next() string {
	n := s.seq.Add(1)
	return fmt.Sprintf("evt-%019d-%0*d", time.Now().UnixNano(), seqWidth, n)
}

// Sink 是事件下行出口（ws.Hub 实现）。定义为接口使聚合器可脱离真实
// WS 连接测试（与 ws.MessageHandler 的接口倒置同一套路）。
type Sink interface {
	// Publish 投递事件给目标会话的在线连接（SessionID 为空时广播）。
	Publish(env ws.Envelope)
}

// EventStore 是事件持久化能力（store.Store 实现）：
// 落库（实时路径）与按游标查询（补发路径）。
type EventStore interface {
	// InsertEvent 写入一条事件。
	InsertEvent(ev store.Event) error

	// ListEventsAfter 查询会话中 id 晚于 lastEventID 的事件（升序，trace 除外）。
	ListEventsAfter(sessionID, lastEventID string, limit int) ([]store.Event, error)
}

// ProjectEventStore 是 v0.2 Store 的增强事件能力。保留 EventStore 的旧
// 接口使现有单元测试和外部 fake 不必同步改造。
type ProjectEventStore interface {
	InsertProjectEvent(ev store.Event) (store.Event, error)
	ListProjectEventsAfter(projectID string, afterSequence int64, limit int) ([]store.Event, error)
}

// Aggregator 是会话聚合器 v1：订阅事件总线，归一化 → 分级 → 落库 → 转发。
// 单 goroutine 消费循环是事件顺序的唯一权威：同一进程内所有下行事件
// 经此处串行处理，ID 发放序 = 落库序 = 转发序。
type Aggregator struct {
	// bus 事件总线（agent.events topic 的订阅源）。
	bus *event.Bus

	// hub 下行出口。
	hub Sink

	// events 事件持久化。
	events EventStore

	// seq 事件 ID 发放器。
	seq Sequencer

	// logger 结构化日志器。
	logger *log.Logger

	// inbox 订阅通道（构造时订阅，见 NewAggregator）。
	inbox <-chan event.Event
}

// NewAggregator 创建聚合器。
// 为什么在构造时而不是 Run 时订阅：bus 的 Publish 只投递给已订阅者，
// Run 的 goroutine 启动存在调度窗口，窗口内发布的事件会静默丢失——
// 装配完成即订阅，事件面从 Wire 返回起就不再有丢事件的盲区。
func NewAggregator(bus *event.Bus, hub Sink, events EventStore, logger *log.Logger) *Aggregator {
	return &Aggregator{bus: bus, hub: hub, events: events, logger: logger,
		inbox: bus.Subscribe(event.TopicAgentEvents)}
}

// Run 启动消费循环并阻塞，直到 ctx 取消（订阅随之注销）。
// 与 App.Run 同生命周期：启动日志在此记录（聚合器是事件面关键路径）。
func (a *Aggregator) Run(ctx context.Context) {
	defer a.bus.Unsubscribe(a.inbox)
	a.logger.Info("聚合器已启动", "topic", event.TopicAgentEvents)
	for {
		select {
		case <-ctx.Done():
			a.logger.Info("聚合器已停止")
			return
		case ev := <-a.inbox:
			a.handle(ev)
		}
	}
}

// handle 处理一条总线事件：类型断言 → 归一化 → 分级 → 落库 → 转发。
func (a *Aggregator) handle(ev event.Event) {
	env, ok := ev.Payload.(ws.Envelope)
	if !ok {
		// 非 envelope 负载说明发布方未遵守 topic 契约，忽略但留下痕迹。
		a.logger.Debug("忽略非 envelope 负载的事件", "topic", ev.Topic)
		return
	}
	if env.Channel == "" || env.Type == "" {
		a.logger.Warn("事件缺少 channel/type，协议违例已丢弃",
			"channel", env.Channel, "type", env.Type)
		return
	}
	env = a.normalize(env)

	record, err := toRecord(env)
	if err != nil {
		a.logger.WithError(err).Error("事件序列化失败，已丢弃", "event_id", env.ID)
		return
	}
	// 先落库再转发：落库是断连续传与历史的事实源，转发是易失通知。
	// 反向顺序在"转发后落库前崩溃"的窗口会产生"客户端见过但补发不到"
	// 的事件；落库失败的事件不转发（宁可实时侧缺席，不可让 sync 契约撒谎）。
	if projectStore, ok := a.events.(ProjectEventStore); ok {
		record, err = projectStore.InsertProjectEvent(record)
		if err == nil {
			env.ProjectID = record.ProjectID
			env.ResourceType = record.ResourceType
			env.ResourceID = record.ResourceID
			env.Revision = record.Revision
			env.Sequence = record.Sequence
		}
	} else {
		err = a.events.InsertEvent(record)
	}
	if err != nil {
		a.logger.WithError(err).Error("事件落库失败，已丢弃（不转发）",
			"event_id", env.ID, "session_id", env.SessionID, "type", env.Type)
		return
	}
	a.hub.Publish(env)
}

// normalize 归一化 envelope：id 一律由聚合器重新发放（发布方预设的 id
// 只是占位——多 goroutine 并发生成的"毫秒+随机"id 在同毫秒内无字典序
// 保证，单点重发是补发契约的唯一可靠来源）；ts 零值兜底为当前时间；
// agent 信息兜底为 system；importance 按规则表重打（发布方取值仅作占位）。
// channel/type 缺失的事件无法路由与归档，属于协议违例，丢弃并 WARN。
func (a *Aggregator) normalize(env ws.Envelope) ws.Envelope {
	env.ID = a.seq.Next()
	if env.Ts.IsZero() {
		env.Ts = time.Now().UTC()
	}
	if env.Agent.ID == "" {
		env.Agent.ID = defaultAgentID
	}
	if env.Agent.Name == "" {
		env.Agent.Name = env.Agent.ID
	}
	env.Importance = Classify(env.Channel, env.Payload)
	return env
}

// ReplayEvents 实现 ws.EventReplayer：返回会话中 id 晚于 lastEventID 的
// 缺失事件（按 id 升序，store 查询侧已排除可丢不补的 trace 频道）。
// v1 一次性返回全部缺口（不分页）：缺口规模受会话长度限制，
// 分页协议留给事件量级真实出现时（见 changelog TODO）。
func (a *Aggregator) ReplayEvents(sessionID, lastEventID string) ([]ws.Envelope, error) {
	records, err := a.events.ListEventsAfter(sessionID, lastEventID, 0)
	if err != nil {
		return nil, err
	}
	envs := make([]ws.Envelope, 0, len(records))
	for _, rec := range records {
		env, err := fromRecord(rec)
		if err != nil {
			// 单条损坏不拖垮整个补发：跳过并记录（数据污染显性化）。
			a.logger.WithError(err).Error("补发事件反序列化失败，已跳过", "event_id", rec.ID)
			continue
		}
		envs = append(envs, env)
	}
	a.logger.Info("断连续传补发", "session_id", sessionID,
		"last_event_id", lastEventID, "count", len(envs))
	return envs, nil
}

// ReplayProjectEvents 返回 Project sequence 之后的可恢复事件。
func (a *Aggregator) ReplayProjectEvents(projectID string, afterSequence int64) ([]ws.Envelope, error) {
	projectStore, ok := a.events.(ProjectEventStore)
	if !ok {
		return nil, fmt.Errorf("Project 事件存储未装配")
	}
	records, err := projectStore.ListProjectEventsAfter(projectID, afterSequence, 0)
	if err != nil {
		return nil, err
	}
	result := make([]ws.Envelope, 0, len(records))
	for _, record := range records {
		envelope, err := fromRecord(record)
		if err != nil {
			a.logger.WithError(err).Error("Project 事件反序列化失败，已跳过",
				"event_id", record.ID)
			continue
		}
		result = append(result, envelope)
	}
	return result, nil
}

// toRecord 把 envelope 转为持久化记录（parent/payload 序列化为 JSON 文本）。
func toRecord(env ws.Envelope) (store.Event, error) {
	parent, err := json.Marshal(env.Parent)
	if err != nil {
		return store.Event{}, fmt.Errorf("序列化 parent 失败: %w", err)
	}
	payload, err := json.Marshal(env.Payload)
	if err != nil {
		return store.Event{}, fmt.Errorf("序列化 payload 失败: %w", err)
	}
	return store.Event{
		ID: env.ID, ProjectID: env.ProjectID, SessionID: env.SessionID,
		ResourceType: env.ResourceType, ResourceID: env.ResourceID,
		Revision: env.Revision, Sequence: env.Sequence,
		Channel: string(env.Channel), Type: env.Type,
		Importance: string(env.Importance), AgentID: env.Agent.ID,
		AgentRole: env.Agent.Role, Parent: string(parent),
		Payload: string(payload), Ts: env.Ts,
	}, nil
}

// fromRecord 把持久化记录还原为 envelope：payload 以 json.RawMessage
// 原样回灌（重发时逐字节一致，不经历 map 往返的字段序/数值精度漂移）。
func fromRecord(rec store.Event) (ws.Envelope, error) {
	var parent ws.ParentRef
	if err := json.Unmarshal([]byte(rec.Parent), &parent); err != nil {
		return ws.Envelope{}, fmt.Errorf("解析 parent 失败: %w", err)
	}
	payload := json.RawMessage(rec.Payload)
	return ws.Envelope{
		ID: rec.ID, ProjectID: rec.ProjectID, SessionID: rec.SessionID,
		ResourceType: rec.ResourceType, ResourceID: rec.ResourceID,
		Revision: rec.Revision, Sequence: rec.Sequence, Ts: rec.Ts,
		Agent:   ws.AgentRef{ID: rec.AgentID, Role: rec.AgentRole, Name: rec.AgentID},
		Channel: ws.Channel(rec.Channel), Type: rec.Type,
		Importance: ws.Importance(rec.Importance), Parent: parent,
		Payload: payload,
	}, nil
}
