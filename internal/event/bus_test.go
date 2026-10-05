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
	"io"
	"sync"
	"testing"
	"time"

	"insightos.cn/semantic-framework/pkg/log"
)

// newTestBus 创建带静默日志器的总线。
func newTestBus() *Bus {
	return NewBus(log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
}

// recvEvent 在超时内从通道接收一个事件。
func recvEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(time.Second):
		t.Fatal("1s 内未收到事件")
		return Event{}
	}
}

// TestPublishSubscribe 验证发布的事件携带 topic/payload/时间戳到达订阅者。
func TestPublishSubscribe(t *testing.T) {
	bus := newTestBus()
	ch := bus.Subscribe("t1")
	defer bus.Unsubscribe(ch)

	bus.Publish("t1", "hello")
	ev := recvEvent(t, ch)
	if ev.Topic != "t1" || ev.Payload != "hello" {
		t.Errorf("事件内容不符: %+v", ev)
	}
	if ev.Ts.IsZero() {
		t.Error("事件时间戳不应为零值")
	}

	// 未订阅的 topic 不应有事件到达。
	select {
	case ev := <-ch:
		t.Errorf("未订阅的 topic 不应投递事件: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestMultipleSubscribers 验证同一 topic 的多个订阅者都能收到事件。
func TestMultipleSubscribers(t *testing.T) {
	bus := newTestBus()
	ch1 := bus.Subscribe("t1")
	ch2 := bus.Subscribe("t1")
	defer bus.Unsubscribe(ch1)
	defer bus.Unsubscribe(ch2)

	bus.Publish("t1", 42)
	for i, ch := range []<-chan Event{ch1, ch2} {
		if ev := recvEvent(t, ch); ev.Payload != 42 {
			t.Errorf("订阅者 %d 收到的事件负载应为 42，实际: %v", i, ev.Payload)
		}
	}
}

// TestUnsubscribe 验证退订后不再收到事件。
func TestUnsubscribe(t *testing.T) {
	bus := newTestBus()
	ch := bus.Subscribe("t1")
	bus.Unsubscribe(ch)

	bus.Publish("t1", "x")
	select {
	case ev := <-ch:
		t.Errorf("退订后不应再收到事件: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestPublishReliableNonBlocking 验证订阅者暂时不消费时，可靠 Publish 既不
// 阻塞发布方，也不会因为超过公开通道容量而丢失业务事件。
func TestPublishReliableNonBlocking(t *testing.T) {
	bus := newTestBus()
	ch := bus.Subscribe("t1")
	defer bus.Unsubscribe(ch)

	const multiplier = 4
	total := subscribeBuffer * multiplier
	// 发布期间不消费，直接灌入超过缓冲容量的可靠事件。
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < total; i++ {
			bus.Publish("t1", i)
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("订阅者通道已满时可靠 Publish 发生阻塞")
	}

	// 开始消费后，异步邮箱必须按发布顺序交付全部事件。
	for want := 0; want < total; want++ {
		if event := recvEvent(t, ch); event.Payload != want {
			t.Fatalf("可靠事件 %d 丢失或乱序，实际负载: %v", want, event.Payload)
		}
	}
}

// TestPublishDroppableKeepsReliableTail 验证高频可丢事件在积压时会限流，
// 但随后发布的关键终态仍进入可靠队列并最终到达。
func TestPublishDroppableKeepsReliableTail(t *testing.T) {
	bus := newTestBus()
	ch := bus.Subscribe("t1")
	defer bus.Unsubscribe(ch)

	total := subscribeBuffer * 4
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < total; i++ {
			bus.PublishDroppable("t1", i)
		}
		bus.Publish("t1", "run.completed")
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("可丢事件积压时发布路径发生阻塞")
	}

	acceptedDroppable := 0
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-ch:
			if event.Payload == "run.completed" {
				if acceptedDroppable >= total {
					t.Fatalf("积压时应丢弃部分流式事件，实际全部接受 %d 条", acceptedDroppable)
				}
				return
			}
			acceptedDroppable++
		case <-deadline:
			t.Fatal("关键终态未越过可丢事件积压并到达订阅者")
		}
	}
}

// TestUnsubscribeStopsFullSubscriber 验证订阅者完全不消费且可靠队列已有积压
// 时，退订仍立即返回，并能打断正在等待满 out 的 worker。
func TestUnsubscribeStopsFullSubscriber(t *testing.T) {
	bus := newTestBus()
	ch := bus.Subscribe("t1")

	bus.mu.RLock()
	var subscriber *subscription
	for candidate := range bus.subs["t1"] {
		subscriber = candidate
		break
	}
	bus.mu.RUnlock()
	if subscriber == nil {
		t.Fatal("未找到刚创建的订阅者")
	}

	for i := 0; i < subscribeBuffer*4; i++ {
		bus.Publish("t1", i)
	}
	done := make(chan struct{})
	go func() {
		bus.Unsubscribe(ch)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("满订阅者退订发生阻塞")
	}
	select {
	case <-subscriber.exited:
	case <-time.After(time.Second):
		t.Fatal("退订后订阅 worker 未退出")
	}
}

// TestPublishConcurrentUnsubscribe 为 race 测试提供发布与退订的真实竞争窗口。
// 发布方可能已经取得订阅者快照；subscription.stop/enqueue 必须自行协调，
// 不能向已停止 worker 留下事件或发生数据竞争。
func TestPublishConcurrentUnsubscribe(t *testing.T) {
	for attempt := 0; attempt < 20; attempt++ {
		bus := newTestBus()
		ch := bus.Subscribe("t1")
		start := make(chan struct{})
		var workers sync.WaitGroup
		workers.Add(2)
		go func() {
			defer workers.Done()
			<-start
			for i := 0; i < subscribeBuffer*2; i++ {
				if i%2 == 0 {
					bus.Publish("t1", i)
				} else {
					bus.PublishDroppable("t1", i)
				}
			}
		}()
		go func() {
			defer workers.Done()
			<-start
			bus.Unsubscribe(ch)
		}()
		close(start)
		workers.Wait()
	}
}

// TestPublishNoSubscriber 验证无订阅者时 Publish 安全返回。
func TestPublishNoSubscriber(t *testing.T) {
	bus := newTestBus()
	bus.Publish("nobody", "x")
}
