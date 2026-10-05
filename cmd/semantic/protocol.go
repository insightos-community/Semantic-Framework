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
	"fmt"
)

// 本文件是 WS 协议的客户端镜像（契约以 docs/api/ws.md 与服务端
// internal/server/ws 为准）。为什么 CLI 不复用服务端的类型：CLI 是协议
// 客户端，只解码自己关心的字段（delta/done/interaction.request/sync.done/
// error），本地镜像让协议编解码可脱离服务端依赖独立单测；契约文档与
// 集成测试兜底防漂移。

// 上行消息类型（与服务端 uplinkType* 常量一致）。
const (
	// uplinkChatMessage 用户对话消息。
	uplinkChatMessage = "chat.message"

	// uplinkInteractionReply 交互应答（批准/拒绝）。
	uplinkInteractionReply = "interaction.reply"

	// uplinkSync 断连续传请求。
	uplinkSync = "sync"
)

// uplinkMessage 是上行消息的协议结构（omitempty 保证每种类型只带自己的字段；
// Approved 用指针区分"显式拒绝"与"缺省"）。
type uplinkMessage struct {
	// Type 消息类型。
	Type string `json:"type"`

	// SessionID 目标会话 ID（chat.message）。
	SessionID string `json:"session_id,omitempty"`

	// Text 消息文本（chat.message）。
	Text string `json:"text,omitempty"`

	// InteractionID 目标交互 ID（interaction.reply）。
	InteractionID string `json:"interaction_id,omitempty"`

	// Approved 批准/拒绝（interaction.reply）。
	Approved *bool `json:"approved,omitempty"`

	// LastEventID 断连续传游标（sync）。
	LastEventID string `json:"last_event_id,omitempty"`
}

// encodeUplink 序列化一条上行消息。
func encodeUplink(msg uplinkMessage) ([]byte, error) {
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("序列化上行消息失败: %w", err)
	}
	return data, nil
}

// 下行频道与事件类型（CLI 关心的子集）。
const (
	// channelDialogue 主对话频道。
	channelDialogue = "dialogue"

	// channelInteraction 交互请求频道。
	channelInteraction = "interaction"

	// eventMessageDelta 模型文本增量。
	eventMessageDelta = "message.delta"

	// eventMessageDone 本轮回复完成。
	eventMessageDone = "message.done"

	// eventInteractionRequest 交互请求。
	eventInteractionRequest = "interaction.request"
)

// downEnvelope 是下行 envelope 的镜像（字段子集，未知字段忽略）。
type downEnvelope struct {
	// ID 事件唯一标识。
	ID string `json:"id"`

	// Channel 事件分发频道（协议应答消息为空，据此区分两类下行）。
	Channel string `json:"channel"`

	// Type 事件类型。
	Type string `json:"type"`

	// Importance 事件重要级别。
	Importance string `json:"importance"`

	// Payload 事件负载（按 type 再解码）。
	Payload json.RawMessage `json:"payload"`
}

// downReply 是协议应答消息（sync.done / error，非 envelope）。
type downReply struct {
	// Type 应答类型（sync.done / error）。
	Type string `json:"type"`

	// Count sync.done 的补发条数。
	Count int `json:"count"`

	// Code error 应答的错误码。
	Code string `json:"code"`

	// Message error 应答的描述。
	Message string `json:"message"`
}

// deltaPayload 是 message.delta 的负载。
type deltaPayload struct {
	// RunID 本次运行 ID。
	RunID string `json:"run_id"`

	// Text 文本增量。
	Text string `json:"text"`
}

// donePayload 是 message.done 的负载。
type donePayload struct {
	// RunID 本次运行 ID。
	RunID string `json:"run_id"`

	// Text 完整回复文本。
	Text string `json:"text"`

	// Turns 模型调用轮次。
	Turns int `json:"turns"`

	// Usage token 用量。
	Usage *struct {
		// TotalTokens 累计总 tokens。
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`

	// Error 运行失败的错误描述。
	Error string `json:"error"`
}

// interactionRequestPayload 是 interaction.request 的负载。
type interactionRequestPayload struct {
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

// decodeDownlink 解析一条下行消息：channel 非空为 envelope，否则为协议应答。
func decodeDownlink(data []byte) (downEnvelope, downReply, error) {
	var head struct {
		Channel string `json:"channel"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return downEnvelope{}, downReply{}, fmt.Errorf("下行消息不是合法 JSON: %w", err)
	}
	if head.Channel != "" {
		var env downEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			return downEnvelope{}, downReply{}, fmt.Errorf("下行 envelope 解析失败: %w", err)
		}
		return env, downReply{}, nil
	}
	var reply downReply
	if err := json.Unmarshal(data, &reply); err != nil {
		return downEnvelope{}, downReply{}, fmt.Errorf("下行协议应答解析失败: %w", err)
	}
	return downEnvelope{}, reply, nil
}
