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
	"io"
	"sync"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/log"
)

// testLogger 返回静默日志器，避免测试输出被日志淹没。
func testLogger() *log.Logger {
	return log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
}

// TestClassifyRules 验证分级规则表 v1 的全部分支（纯函数表驱动测试）。
func TestClassifyRules(t *testing.T) {
	cases := []struct {
		name    string
		channel ws.Channel
		payload any
		want    ws.Importance
	}{
		// interaction 一律 critical（审批不可被折叠）。
		{"interaction 请求", ws.ChannelInteraction, map[string]any{"level": 1}, ws.ImportanceCritical},
		{"interaction 空负载", ws.ChannelInteraction, nil, ws.ImportanceCritical},
		// 静态表。
		{"dialogue", ws.ChannelDialogue, nil, ws.ImportanceNormal},
		{"artifact", ws.ChannelArtifact, nil, ws.ImportanceNormal},
		{"trace 恒 low", ws.ChannelTrace, map[string]any{"level": 5}, ws.ImportanceLow},
		{"未知频道兜底 normal", ws.Channel("workflow"), nil, ws.ImportanceNormal},
		// alert 数值级别：≥3 critical，1-2 low，0/缺失 normal。
		{"alert level 3", ws.ChannelAlert, map[string]any{"level": 3}, ws.ImportanceCritical},
		{"alert level 5", ws.ChannelAlert, map[string]any{"level": 5}, ws.ImportanceCritical},
		{"alert level 2", ws.ChannelAlert, map[string]any{"level": 2}, ws.ImportanceLow},
		{"alert level 1", ws.ChannelAlert, map[string]any{"level": 1}, ws.ImportanceLow},
		{"alert level 0", ws.ChannelAlert, map[string]any{"level": 0}, ws.ImportanceNormal},
		{"alert 缺 level", ws.ChannelAlert, map[string]any{"msg": "x"}, ws.ImportanceNormal},
		{"alert JSON 数值", ws.ChannelAlert, json.RawMessage(`{"level":4}`), ws.ImportanceCritical},
		// alert 字符串级别映射。
		{"alert critical 串", ws.ChannelAlert, map[string]any{"level": "critical"}, ws.ImportanceCritical},
		{"alert HIGH 串（大小写）", ws.ChannelAlert, map[string]any{"level": "HIGH"}, ws.ImportanceCritical},
		{"alert warning 串", ws.ChannelAlert, map[string]any{"level": "warning"}, ws.ImportanceNormal},
		{"alert info 串", ws.ChannelAlert, map[string]any{"level": "info"}, ws.ImportanceLow},
		{"alert 未知串", ws.ChannelAlert, map[string]any{"level": "notice"}, ws.ImportanceNormal},
		// 非对象负载不崩溃，兜底 normal。
		{"alert 非标量负载", ws.ChannelAlert, "oops", ws.ImportanceNormal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.channel, tc.payload); got != tc.want {
				t.Errorf("Classify(%s, %v) = %q，期望 %q", tc.channel, tc.payload, got, tc.want)
			}
		})
	}
}

// TestSequencerMonotonic 验证事件 ID 的字典序严格等于发放序
// （断连续传"按 id 排序补发"的正确性基础）。
func TestSequencerMonotonic(t *testing.T) {
	var seq Sequencer
	prev := ""
	for i := 0; i < 1000; i++ {
		id := seq.Next()
		if prev != "" && id <= prev {
			t.Fatalf("第 %d 个 ID 未严格递增: %q 应大于 %q", i, id, prev)
		}
		prev = id
	}
}

// fakeSink 是记录型下行出口（ws.Hub 的测试替身）。
type fakeSink struct {
	// mu 保护 envs。
	mu sync.Mutex

	// envs 按投递序记录的事件。
	envs []ws.Envelope
}

// Publish 记录一条投递事件。
func (f *fakeSink) Publish(env ws.Envelope) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.envs = append(f.envs, env)
}

// snapshot 返回已记录事件的副本。
func (f *fakeSink) snapshot() []ws.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ws.Envelope, len(f.envs))
	copy(out, f.envs)
	return out
}

// openEventStore 在临时目录打开一个已迁移的 store.Store。
func openEventStore(t *testing.T) *store.Store {
	t.Helper()
	st := storetest.OpenMigrated(t, testLogger())
	return st
}

// startAggregator 装配聚合器（真实 bus + 真实 store + 记录型 sink）并启动。
func startAggregator(t *testing.T) (*Aggregator, *event.Bus, *fakeSink, *store.Store) {
	t.Helper()
	bus := event.NewBus(testLogger())
	sink := &fakeSink{}
	st := openEventStore(t)
	agg := NewAggregator(bus, sink, st, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go agg.Run(ctx)
	return agg, bus, sink, st
}

// waitSink 等待 sink 收到至少 n 条事件（轮询替代睡眠，规避时序不稳定）。
func waitSink(t *testing.T, sink *fakeSink, n int) []ws.Envelope {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		envs := sink.snapshot()
		if len(envs) >= n {
			return envs
		}
		if time.Now().After(deadline) {
			t.Fatalf("5s 内 sink 只收到 %d/%d 条事件", len(envs), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAggregatorEndToEnd 端到端：bus 发布 → hub(sink) 收到（归一化 +
// 分级生效、id 单调）→ store 有记录 → ReplayEvents 按游标回放。
func TestAggregatorEndToEnd(t *testing.T) {
	agg, bus, sink, st := startAggregator(t)

	// ① 三条事件：dialogue（producer 预设 id/importance，应被重发/重打）、
	// interaction（critical）、trace（low，落库但补发排除）。
	delta := ws.NewEnvelope("cs-1", ws.ChannelDialogue, "message.delta", ws.ImportanceNormal,
		map[string]any{"text": "你"})
	delta.Agent = ws.AgentRef{ID: "leader", Role: "coordinator", Name: "leader"}
	producerID := delta.ID
	request := ws.NewEnvelope("cs-1", ws.ChannelInteraction, "interaction.request", ws.ImportanceLow,
		map[string]any{"interaction_id": "int-1"})
	trace := ws.NewEnvelope("cs-1", ws.ChannelTrace, "tool.called", ws.ImportanceNormal,
		map[string]any{"tool": "system.time"})
	for _, env := range []ws.Envelope{delta, request, trace} {
		bus.Publish(event.TopicAgentEvents, env)
	}

	envs := waitSink(t, sink, 3)

	// ② 归一化：id 被聚合器重发（≠ 发布方预设）且严格递增；ts 非零。
	if envs[0].ID == producerID {
		t.Errorf("事件 id 应由聚合器重新发放，实际仍是发布方值: %q", envs[0].ID)
	}
	for i := 1; i < len(envs); i++ {
		if envs[i].ID <= envs[i-1].ID {
			t.Errorf("下行 id 未严格递增: %q 应大于 %q", envs[i].ID, envs[i-1].ID)
		}
	}
	if envs[0].Ts.IsZero() || envs[0].Agent.ID != "leader" {
		t.Errorf("归一化结果不符: %+v", envs[0])
	}

	// ③ 分级重打：interaction 被提为 critical（发布方给的 low 被覆盖），
	// trace 被降为 low（发布方给的 normal 被覆盖）。
	if envs[1].Importance != ws.ImportanceCritical {
		t.Errorf("interaction 应为 critical，实际: %q", envs[1].Importance)
	}
	if envs[2].Importance != ws.ImportanceLow {
		t.Errorf("trace 应为 low，实际: %q", envs[2].Importance)
	}

	// ④ 落库：三条都在（含 trace），字段往返一致。
	records, err := st.ListEventsAfter("cs-1", "", 0)
	if err != nil {
		t.Fatalf("ListEventsAfter 失败: %v", err)
	}
	if len(records) != 2 { // store 查询侧排除 trace
		t.Fatalf("store 应查出 2 条（trace 排除），实际: %d", len(records))
	}
	if records[0].ID != envs[0].ID || records[0].Channel != "dialogue" ||
		records[0].AgentID != "leader" || records[0].AgentRole != "coordinator" {
		t.Errorf("落库记录与下行不一致: %+v", records[0])
	}

	// ⑤ ReplayEvents：空游标回放两条（trace 排除），payload 原样；
	// 中间游标回放一条；末尾游标回放空（幂等）。
	replayed, err := agg.ReplayEvents("cs-1", "")
	if err != nil {
		t.Fatalf("ReplayEvents 失败: %v", err)
	}
	if len(replayed) != 2 || replayed[0].ID != envs[0].ID || replayed[1].ID != envs[1].ID {
		t.Fatalf("回放结果不符: %+v", replayed)
	}
	raw, ok := replayed[0].Payload.(json.RawMessage)
	if !ok {
		t.Fatalf("回放 payload 应为 json.RawMessage，实际: %T", replayed[0].Payload)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil || payload["text"] != "你" {
		t.Errorf("回放 payload 不符: %s, %v", raw, err)
	}
	rest, err := agg.ReplayEvents("cs-1", envs[0].ID)
	if err != nil || len(rest) != 1 || rest[0].ID != envs[1].ID {
		t.Errorf("中间游标回放不符: %v, %+v", err, rest)
	}
	tail, err := agg.ReplayEvents("cs-1", envs[1].ID)
	if err != nil || len(tail) != 0 {
		t.Errorf("末尾游标应回放空: %v, %+v", err, tail)
	}
}

// TestAggregatorNormalizeFallback 验证归一化兜底：零 ts、空 agent、空 id
// 全部补全；channel/type 缺失的协议违例事件被丢弃（不转发、不落库）。
func TestAggregatorNormalizeFallback(t *testing.T) {
	_, bus, sink, st := startAggregator(t)

	// 裸事件：只有 channel/type/session（零 ts、空 agent、空 id）。
	bus.Publish(event.TopicAgentEvents, ws.Envelope{
		SessionID: "cs-2", Channel: ws.ChannelArtifact, Type: "artifact.created",
		Payload: map[string]any{"artifact_id": "art-1"},
	})
	envs := waitSink(t, sink, 1)
	env := envs[0]
	if env.ID == "" || env.Ts.IsZero() {
		t.Errorf("id/ts 应被兜底补全: %+v", env)
	}
	if env.Agent.ID != defaultAgentID || env.Agent.Name != defaultAgentID {
		t.Errorf("agent 应兜底为 system，实际: %+v", env.Agent)
	}
	if env.Importance != ws.ImportanceNormal {
		t.Errorf("artifact 应归一化为 normal，实际: %q", env.Importance)
	}

	// 协议违例：缺 channel / 缺 type / 非 envelope 负载，全部丢弃。
	bus.Publish(event.TopicAgentEvents, ws.Envelope{SessionID: "cs-2", Type: "x"})
	bus.Publish(event.TopicAgentEvents, ws.Envelope{SessionID: "cs-2", Channel: ws.ChannelDialogue})
	bus.Publish(event.TopicAgentEvents, "not-an-envelope")
	time.Sleep(50 * time.Millisecond) // 消费循环是异步的，给丢弃路径一个执行窗口
	if got := len(sink.snapshot()); got != 1 {
		t.Errorf("协议违例事件不应转发，sink 应有 1 条，实际: %d", got)
	}
	records, err := st.ListEventsAfter("cs-2", "", 0)
	if err != nil || len(records) != 1 {
		t.Errorf("协议违例事件不应落库，应有 1 条，实际: %v, %d", err, len(records))
	}
}

// TestAggregatorProjectSequence 验证 Session 事件自动补齐 Project，并能按
// Project 连续 sequence 恢复；Studio 无需为每个 Conversation 单独连接。
func TestAggregatorProjectSequence(t *testing.T) {
	aggregator, bus, sink, st := startAggregator(t)
	now := time.Now().UTC()
	if err := st.CreateChatSession(store.ChatSession{
		ID: "cs-project-event", UserID: "usr-event", Title: "事件会话",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("准备 Conversation 失败: %v", err)
	}
	session, err := st.GetChatSession("cs-project-event")
	if err != nil {
		t.Fatalf("读取 Conversation 失败: %v", err)
	}

	envelope := ws.NewEnvelope(session.ID, ws.ChannelDialogue,
		"run.updated", ws.ImportanceNormal, map[string]any{"status": "running"})
	envelope.ResourceType = "run"
	envelope.ResourceID = "run-project-event"
	envelope.Revision = 2
	bus.Publish(event.TopicAgentEvents, envelope)
	trace := ws.NewEnvelope(session.ID, ws.ChannelTrace,
		"trace.sample", ws.ImportanceLow, map[string]any{"sample": true})
	bus.Publish(event.TopicAgentEvents, trace)
	completed := ws.NewEnvelope(session.ID, ws.ChannelDialogue,
		"run.completed", ws.ImportanceNormal, map[string]any{"status": "completed"})
	completed.ResourceType = "run"
	completed.ResourceID = "run-project-event"
	completed.Revision = 3
	bus.Publish(event.TopicAgentEvents, completed)

	received := waitSink(t, sink, 3)
	if received[0].ProjectID != session.ProjectID || received[0].Sequence != 1 ||
		received[0].ResourceType != "run" || received[0].Revision != 2 {
		t.Fatalf("Project 首事件字段不完整: %+v", received[0])
	}
	if received[1].Channel != ws.ChannelTrace || received[1].Sequence != 0 {
		t.Fatalf("trace 事件不应占用可恢复序号: %+v", received[1])
	}
	if received[2].Sequence != 2 || received[2].Type != "run.completed" {
		t.Fatalf("trace 后业务事件应连续编号为 2: %+v", received[2])
	}
	replayed, err := aggregator.ReplayProjectEvents(session.ProjectID, 0)
	if err != nil || len(replayed) != 2 || replayed[0].Sequence != 1 ||
		replayed[1].Sequence != 2 || replayed[0].ResourceID != "run-project-event" {
		t.Fatalf("Project 事件回放不符: %+v, %v", replayed, err)
	}
	replayed, err = aggregator.ReplayProjectEvents(session.ProjectID, 1)
	if err != nil || len(replayed) != 1 || replayed[0].Sequence != 2 {
		t.Fatalf("trace 不应在增量中制造空洞: %+v, %v", replayed, err)
	}
}

// TestAggregatorReliableStateSurvivesDroppableBurst 回归聚合器启动前总线缓冲
// 被流式/Trace 事件占满的场景。旧实现只有 64 槽非阻塞通道，随后到达的
// Run、Interaction、Memory 状态会在获得 Project sequence 前静默丢失，
// 因而 Snapshot 与重连补发都无法发现缺口。
func TestAggregatorReliableStateSurvivesDroppableBurst(t *testing.T) {
	bus := event.NewBus(testLogger())
	sink := &fakeSink{}
	st := openEventStore(t)
	project, err := st.EnsureDefaultProject("usr-reliable-event")
	if err != nil {
		t.Fatalf("准备 Project 失败: %v", err)
	}
	aggregator := NewAggregator(bus, sink, st, testLogger())

	// 构造聚合器已经订阅、但消费循环尚未启动的确定性积压窗口。Trace 明细
	// 超过原 64 槽容量后允许被丢弃，不能挤掉后面的业务终态。
	for i := 0; i < 256; i++ {
		envelope := ws.NewEnvelope("", ws.ChannelTrace, "trace.delta",
			ws.ImportanceLow, map[string]any{"index": i})
		envelope.ProjectID = project.ID
		bus.PublishDroppable(event.TopicAgentEvents, envelope)
	}

	critical := []ws.Envelope{
		{
			ProjectID: project.ID, ResourceType: "agent_run", ResourceID: "run-reliable",
			Revision: 4, Channel: ws.ChannelDialogue, Type: "run.completed",
			Importance: ws.ImportanceNormal, Payload: map[string]any{"status": "completed"},
		},
		{
			ProjectID: project.ID, ResourceType: "interaction", ResourceID: "int-reliable",
			Revision: 2, Channel: ws.ChannelInteraction, Type: "interaction.resolved",
			Importance: ws.ImportanceCritical, Payload: map[string]any{"status": "answered"},
		},
		{
			ProjectID: project.ID, ResourceType: "project_memory", ResourceID: project.ID,
			Revision: 3, Channel: ws.ChannelDialogue, Type: "memory.updated",
			Importance: ws.ImportanceNormal, Payload: map[string]any{"revision": 3},
		},
	}
	for _, envelope := range critical {
		bus.Publish(event.TopicAgentEvents, envelope)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go aggregator.Run(ctx)

	// 使用真实 Store 的 Project 回放作为完成条件；这同时证明关键事件已经
	// 获得连续 sequence，而不仅是到达易失的 WebSocket sink。
	deadline := time.Now().Add(30 * time.Second)
	var replayed []ws.Envelope
	for time.Now().Before(deadline) {
		replayed, err = aggregator.ReplayProjectEvents(project.ID, 0)
		if err != nil {
			t.Fatalf("回放 Project 事件失败: %v", err)
		}
		if len(replayed) == len(critical) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(replayed) != len(critical) {
		t.Fatalf("关键状态未全部持久化，回放 %d/%d 条: %+v",
			len(replayed), len(critical), replayed)
	}
	for i, envelope := range replayed {
		if envelope.Type != critical[i].Type || envelope.ResourceID != critical[i].ResourceID ||
			envelope.Sequence != int64(i+1) {
			t.Fatalf("关键状态 %d 的回放内容或 sequence 不符: %+v", i, envelope)
		}
	}

	realtime := sink.snapshot()
	seen := make(map[string]bool, len(critical))
	for _, envelope := range realtime {
		for _, expected := range critical {
			if envelope.Type == expected.Type && envelope.ResourceID == expected.ResourceID {
				seen[expected.Type] = true
			}
		}
	}
	for _, expected := range critical {
		if !seen[expected.Type] {
			t.Errorf("关键状态未实时下发: %s", expected.Type)
		}
	}
}
