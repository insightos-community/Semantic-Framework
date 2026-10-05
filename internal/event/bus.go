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

package event

import (
	"sync"
	"time"

	"insightos.cn/semantic-framework/pkg/log"
)

// subscribeBuffer 是订阅通道和可丢事件待发送队列的缓冲容量。
// 业务状态事件不受此上限约束；它们先进入订阅者自己的可靠队列，避免
// 聚合器尚未开始消费或短暂写库变慢时丢失 Project 状态。
const subscribeBuffer = 64

// TopicAgentEvents 是 Agent 协同事件（下行 envelope）的总线 topic，
// 由 bootstrap 桥接到 WS hub 推送给前端。
const TopicAgentEvents = "agent.events"

// Event 是总线上流转的事件。
type Event struct {
	// Topic 事件所属主题。
	Topic string

	// Payload 事件负载，类型由 topic 的契约约定。
	Payload any

	// Ts 事件发布时间。
	Ts time.Time
}

// Bus 是进程内事件总线，维护 topic → 订阅通道的路由表。
// 所有方法并发安全。
type Bus struct {
	// logger 结构化日志器，用于丢弃事件等关键路径记录。
	logger *log.Logger

	// mu 保护 subs 的读写锁。
	mu sync.RWMutex

	// subs 维护 topic 到订阅者集合的映射。每个订阅者有独立发送协程，
	// 因而慢订阅者只会增加自己的待发送队列，不会阻塞发布者或其他订阅者。
	subs map[string]map[*subscription]struct{}
}

// subscription 是一个订阅者的异步邮箱。out 保持原有 Subscribe API；
// queue 保存尚未交给 out 的事件，worker 按进入队列的顺序逐条发送。
//
// 可靠事件的队列不设硬上限：状态变化一旦丢失，聚合器就无法分配 sequence，
// Studio 也无法通过事件缺口发现它。高频且可重建的流式/Trace 事件必须由
// 发布方明确调用 PublishDroppable，它们在 queue 积压时会被丢弃，从而避免
// 慢订阅者造成无界的流式数据堆积。
type subscription struct {
	out    chan Event
	wake   chan struct{}
	done   chan struct{}
	exited chan struct{}

	mu       sync.Mutex
	queue    []Event
	head     int
	inflight bool
	stopped  bool
}

func newSubscription() *subscription {
	return &subscription{
		out:    make(chan Event, subscribeBuffer),
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
		exited: make(chan struct{}),
	}
}

// enqueue 只持有订阅者自己的短锁，不等待 out 的消费者。droppable=true 时，
// 待发送队列达到上限就拒绝该事件；可靠事件始终追加并返回当前积压量。
func (s *subscription) enqueue(ev Event, droppable bool) (accepted bool, backlog int) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return false, 0
	}
	// 无待发送事件且 worker 未持有队首时，沿用旧实现的快速路径：直接把
	// 事件放入公开缓冲。这样低负载下 Publish 返回时订阅者已经可见事件；
	// out 满或已有积压时再进入可靠队列。发送是非阻塞 select，不会让慢
	// 订阅者占住发布线程。
	if !s.inflight && s.head >= len(s.queue) {
		select {
		case s.out <- ev:
			s.mu.Unlock()
			return true, 0
		default:
		}
	}
	backlog = len(s.queue) - s.head
	if droppable && backlog >= subscribeBuffer {
		s.mu.Unlock()
		return false, backlog
	}
	s.queue = append(s.queue, ev)
	backlog++
	s.mu.Unlock()

	// wake 容量为 1：worker 已经被唤醒时无需重复通知。这里绝不等待，
	// 可靠性由 queue 保证，而不是靠 wake 的通知数量保证。
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return true, backlog
}

// next 取出队首并在队列清空时释放底层数组，避免长时间保留事件负载。
func (s *subscription) next() (Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.head >= len(s.queue) {
		if s.head >= len(s.queue) {
			s.queue = nil
			s.head = 0
		}
		return Event{}, false
	}
	ev := s.queue[s.head]
	s.queue[s.head] = Event{}
	s.head++
	s.inflight = true
	if s.head == len(s.queue) {
		s.queue = nil
		s.head = 0
	}
	return ev, true
}

func (s *subscription) delivered() {
	s.mu.Lock()
	s.inflight = false
	s.mu.Unlock()
}

// run 串行发送该订阅者已接受的事件。out 满时只阻塞本订阅者的 worker；
// Publish 仍可把可靠状态追加到 queue，其他订阅者也不受影响。
func (s *subscription) run() {
	defer close(s.exited)
	for {
		if ev, ok := s.next(); ok {
			select {
			case s.out <- ev:
				s.delivered()
			case <-s.done:
				return
			}
			continue
		}
		select {
		case <-s.wake:
		case <-s.done:
			return
		}
	}
}

func (s *subscription) stop() {
	s.mu.Lock()
	if !s.stopped {
		s.stopped = true
		s.queue = nil
		s.head = 0
		s.inflight = false
		close(s.done)
	}
	s.mu.Unlock()
}

// NewBus 创建事件总线。
func NewBus(logger *log.Logger) *Bus {
	return &Bus{
		logger: logger,
		subs:   make(map[string]map[*subscription]struct{}),
	}
}

// Publish 向 topic 的全部订阅者可靠投递业务事件。方法只把事件追加到各
// 订阅者的异步邮箱，不等待订阅者消费；通道暂时已满不会造成状态事件丢失。
func (b *Bus) Publish(topic string, payload any) {
	b.publish(topic, payload, false)
}

// PublishDroppable 投递可以由 Snapshot、终态或完整 Trace 重建的高频事件。
// 只有发布方明确选择此方法时才允许丢弃；当前用于消息/推理流式增量与
// Trace 明细，Run 终态、Interaction、Memory 等业务状态必须使用 Publish。
func (b *Bus) PublishDroppable(topic string, payload any) {
	b.publish(topic, payload, true)
}

func (b *Bus) publish(topic string, payload any, droppable bool) {
	ev := Event{Topic: topic, Payload: payload, Ts: time.Now()}

	b.mu.RLock()
	subscribers := make([]*subscription, 0, len(b.subs[topic]))
	for subscriber := range b.subs[topic] {
		subscribers = append(subscribers, subscriber)
	}
	b.mu.RUnlock()

	for _, subscriber := range subscribers {
		accepted, backlog := subscriber.enqueue(ev, droppable)
		if droppable && !accepted {
			b.logger.Debug("可丢事件因订阅者积压被丢弃",
				"topic", topic, "backlog", backlog)
		} else if !droppable && backlog > 0 && backlog%1024 == 0 {
			// 可靠事件不能静默丢弃；异常积压通过日志显性化，便于定位失活的
			// 订阅者。只按 1024 的倍数记录，避免慢消费者触发日志风暴。
			b.logger.Warn("可靠事件订阅者积压过高",
				"topic", topic, "backlog", backlog)
		}
	}
}

// Subscribe 订阅 topic，返回带缓冲的接收通道。
// 不再使用时必须调用 Unsubscribe 释放，否则通道常驻路由表。
func (b *Bus) Subscribe(topic string) <-chan Event {
	subscriber := newSubscription()
	b.mu.Lock()
	if b.subs[topic] == nil {
		b.subs[topic] = make(map[*subscription]struct{})
	}
	b.subs[topic][subscriber] = struct{}{}
	b.mu.Unlock()
	go subscriber.run()
	return subscriber.out
}

// Unsubscribe 退订指定通道，从所有 topic 的路由表中移除。
// 为什么遍历全部 topic：订阅方只持有通道句柄，退订不应要求它回忆 topic。
func (b *Bus) Unsubscribe(ch <-chan Event) {
	b.mu.Lock()
	var stopped []*subscription
	for topic, set := range b.subs {
		for subscriber := range set {
			if (<-chan Event)(subscriber.out) == ch {
				delete(set, subscriber)
				stopped = append(stopped, subscriber)
			}
		}
		if len(set) == 0 {
			delete(b.subs, topic)
		}
	}
	b.mu.Unlock()
	for _, subscriber := range stopped {
		subscriber.stop()
	}
}
