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

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEncodeUplink 验证上行消息编码与线上协议一致（字段名/类型后缀，
// 契约见 docs/api/ws.md；omitempty 保证每种类型只带自己的字段）。
func TestEncodeUplink(t *testing.T) {
	// chat.message：只含 type/session_id/text。
	data, err := encodeUplink(uplinkMessage{
		Type: uplinkChatMessage, SessionID: "cs-1", Text: "你好",
	})
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("编码结果应为 JSON: %v", err)
	}
	if m["type"] != "chat.message" || m["session_id"] != "cs-1" || m["text"] != "你好" {
		t.Errorf("chat.message 编码不符: %s", data)
	}
	for _, absent := range []string{"interaction_id", "approved", "last_event_id"} {
		if _, ok := m[absent]; ok {
			t.Errorf("chat.message 不应携带 %s: %s", absent, data)
		}
	}

	// interaction.reply：显式拒绝（approved=false）必须出现在线上——
	// 指针 + omitempty 的语义就是"false 不等于缺省"。
	approved := false
	data, err = encodeUplink(uplinkMessage{
		Type: uplinkInteractionReply, InteractionID: "int-1", Approved: &approved,
	})
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if !strings.Contains(string(data), `"approved":false`) {
		t.Errorf("显式拒绝应编码 approved:false，实际: %s", data)
	}

	// sync：携带游标。
	data, err = encodeUplink(uplinkMessage{Type: uplinkSync, LastEventID: "evt-1"})
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if !strings.Contains(string(data), `"last_event_id":"evt-1"`) {
		t.Errorf("sync 编码不符: %s", data)
	}
}

// TestDecodeDownlink 验证下行消息解析：envelope（按 channel 区分）与
// 协议应答（sync.done/error）两条路径。
func TestDecodeDownlink(t *testing.T) {
	// ① envelope：dialogue/message.delta。
	raw := `{"id":"evt-1","session_id":"cs-1","ts":"2026-08-03T10:00:00Z",` +
		`"agent":{"id":"leader","role":"coordinator","name":"leader"},` +
		`"channel":"dialogue","type":"message.delta","importance":"normal",` +
		`"parent":{},"payload":{"run_id":"run-1","text":"你"}}`
	env, reply, err := decodeDownlink([]byte(raw))
	if err != nil {
		t.Fatalf("解析 envelope 失败: %v", err)
	}
	if env.Channel != channelDialogue || env.Type != eventMessageDelta || env.ID != "evt-1" {
		t.Errorf("envelope 头不符: %+v", env)
	}
	var delta deltaPayload
	if err := json.Unmarshal(env.Payload, &delta); err != nil || delta.Text != "你" || delta.RunID != "run-1" {
		t.Errorf("delta 负载不符: %v, %+v", err, delta)
	}

	// ② envelope：interaction.request 负载（审批链路）。
	raw = `{"id":"evt-2","channel":"interaction","type":"interaction.request",` +
		`"payload":{"interaction_id":"int-1","type":"confirm","question":"确认写入？","risk":"high","timeout_ts":100}}`
	env, _, err = decodeDownlink([]byte(raw))
	if err != nil {
		t.Fatalf("解析 interaction envelope 失败: %v", err)
	}
	var req interactionRequestPayload
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		t.Fatalf("interaction.request 负载解析失败: %v", err)
	}
	if req.InteractionID != "int-1" || req.Question != "确认写入？" || req.Risk != "high" || req.TimeoutTS != 100 {
		t.Errorf("interaction.request 负载不符: %+v", req)
	}

	// ③ 协议应答：sync.done。
	_, reply, err = decodeDownlink([]byte(`{"type":"sync.done","count":4}`))
	if err != nil || reply.Type != "sync.done" || reply.Count != 4 {
		t.Errorf("sync.done 解析不符: %v, %+v", err, reply)
	}

	// ④ 协议应答：error。
	_, reply, err = decodeDownlink([]byte(`{"type":"error","code":"WS_BAD_MESSAGE","message":"x"}`))
	if err != nil || reply.Type != "error" || reply.Code != "WS_BAD_MESSAGE" {
		t.Errorf("error 应答解析不符: %v, %+v", err, reply)
	}

	// ⑤ 非法 JSON 显性报错。
	if _, _, err = decodeDownlink([]byte(`not-json`)); err == nil {
		t.Error("非法 JSON 应返回错误")
	}
}

// TestDonePayload 验证 message.done 负载的 usage/error 解析。
func TestDonePayload(t *testing.T) {
	raw := `{"run_id":"run-1","text":"全文","turns":2,` +
		`"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`
	var p donePayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if p.Text != "全文" || p.Turns != 2 || p.Usage == nil || p.Usage.TotalTokens != 30 || p.Error != "" {
		t.Errorf("done 负载不符: %+v", p)
	}
}

// TestParseApproval 验证审批输入解析（REPL 的 y/n 语义）。
func TestParseApproval(t *testing.T) {
	cases := []struct {
		in       string
		approved bool
		valid    bool
	}{
		{"y", true, true}, {"Y", true, true}, {"yes", true, true},
		{"n", false, true}, {"N", false, true}, {"no", false, true},
		{"批准", false, false}, {"", false, false}, {"yb", false, false},
	}
	for _, tc := range cases {
		approved, valid := parseApproval(tc.in)
		if approved != tc.approved || valid != tc.valid {
			t.Errorf("parseApproval(%q) = (%v, %v)，期望 (%v, %v)",
				tc.in, approved, valid, tc.approved, tc.valid)
		}
	}
}
