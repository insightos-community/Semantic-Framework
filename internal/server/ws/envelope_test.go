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

package ws

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEnvelopeSerialization 验证 envelope 序列化为协议规定的 snake_case 结构，
// 字段名是线上契约（docs/architecture/02 §3.3），改动必须显式可见。
func TestEnvelopeSerialization(t *testing.T) {
	env := NewEnvelope("cs-9f2", ChannelTrace, "tool.completed", ImportanceNormal,
		map[string]any{"tool": "system.time"})
	env.Agent = AgentRef{ID: "robot-a", Role: "robot", Name: "星海图 A 号"}
	env.Parent = ParentRef{RunID: "run-001", TraceID: "trace-007"}

	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("序列化结果应为 JSON 对象: %v", err)
	}

	// 协议字段必须全部存在且为 snake_case。
	for _, key := range []string{
		"id", "session_id", "ts", "agent", "channel", "type", "importance", "parent", "payload",
	} {
		if _, ok := m[key]; !ok {
			t.Errorf("envelope 缺少协议字段 %q: %s", key, data)
		}
	}

	if m["session_id"] != "cs-9f2" || m["channel"] != "trace" ||
		m["type"] != "tool.completed" || m["importance"] != "normal" {
		t.Errorf("envelope 标量字段不符: %s", data)
	}
	if id, _ := m["id"].(string); !strings.HasPrefix(id, "evt-") {
		t.Errorf("事件 ID 应以 evt- 开头，实际: %q", id)
	}
	if ts, _ := m["ts"].(string); ts == "" || !strings.Contains(ts, "T") {
		t.Errorf("ts 应为 RFC3339 时间串，实际: %q", ts)
	}

	agent, _ := m["agent"].(map[string]any)
	if agent["id"] != "robot-a" || agent["role"] != "robot" || agent["name"] != "星海图 A 号" {
		t.Errorf("agent 字段不符: %v", agent)
	}
	parent, _ := m["parent"].(map[string]any)
	if parent["run_id"] != "run-001" || parent["trace_id"] != "trace-007" {
		t.Errorf("parent 字段不符: %v", parent)
	}
	payload, _ := m["payload"].(map[string]any)
	if payload["tool"] != "system.time" {
		t.Errorf("payload 字段不符: %v", payload)
	}
}

// TestNewEnvelopeUniqueID 验证连续构造的事件 ID 不重复（同毫秒内靠随机段区分）。
func TestNewEnvelopeUniqueID(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 1000; i++ {
		env := NewEnvelope("", ChannelTrace, "t", ImportanceLow, nil)
		if _, dup := seen[env.ID]; dup {
			t.Fatalf("事件 ID 重复: %s", env.ID)
		}
		seen[env.ID] = struct{}{}
		if env.Ts.IsZero() {
			t.Fatal("事件时间戳不应为零值")
		}
	}
}
