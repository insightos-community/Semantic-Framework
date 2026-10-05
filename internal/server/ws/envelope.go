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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Channel 是事件的分发通道，前端据此决定事件的呈现位置。
// 取值与架构文档 §3.3 的枚举一致，是线上协议的一部分，禁止改名。
type Channel string

const (
	// ChannelDialogue 主对话通道（Agent 消息、流式回复）。
	ChannelDialogue Channel = "dialogue"

	// ChannelAlert 告警通道。
	ChannelAlert Channel = "alert"

	// ChannelTrace 工具与推理明细通道，允许在背压时丢弃。
	ChannelTrace Channel = "trace"

	// ChannelArtifact 产物引用通道。
	ChannelArtifact Channel = "artifact"

	// ChannelInteraction 确认与表单请求通道。
	ChannelInteraction Channel = "interaction"

	// ChannelSimulation 场景、Runtime、Viewer 与虚拟 Robot 状态通道。
	ChannelSimulation Channel = "simulation"
)

// Importance 是事件的重要级别，前端据此决定进主流、任务卡片或折叠。
type Importance string

const (
	// ImportanceCritical 关键事件，必须进主流视图。
	ImportanceCritical Importance = "critical"

	// ImportanceNormal 常规事件。
	ImportanceNormal Importance = "normal"

	// ImportanceLow 低优事件，可折叠或以 badge 呈现。
	ImportanceLow Importance = "low"
)

// AgentRef 标识事件的来源 Agent。
type AgentRef struct {
	// ID Agent 唯一标识。
	ID string `json:"id"`

	// Role Agent 角色（leader/robot/query 等）。
	Role string `json:"role"`

	// Name Agent 显示名。
	Name string `json:"name"`
}

// ParentRef 标识事件关联的运行上下文，供运行检查器定位 Trace。
type ParentRef struct {
	// RunID 关联的 Agent Run ID。
	RunID string `json:"run_id,omitempty"`

	// TraceID 关联的链路 ID。
	TraceID string `json:"trace_id,omitempty"`
}

// Envelope 是 WS 下行事件的统一信封结构（架构文档 §3.3）。
// 所有下行事件必须封装为该结构，前端只解析这一种协议。
type Envelope struct {
	// ID 事件唯一标识（evt- 前缀 + 时间戳 + 随机数）。
	ID string `json:"id"`

	// ProjectID 目标 Project；Studio 连接按它接收整个工作区的变化。
	ProjectID string `json:"project_id,omitempty"`

	// SessionID 目标会话 ID；Project 级事件可以为空。
	SessionID string `json:"session_id,omitempty"`

	// ResourceType / ResourceID 标识变化资源，revision 用于忽略旧事件，
	// sequence 用于发现 Project 增量缺口。
	ResourceType string `json:"resource_type,omitempty"`
	ResourceID   string `json:"resource_id,omitempty"`
	Revision     int64  `json:"revision,omitempty"`
	Sequence     int64  `json:"sequence,omitempty"`

	// Ts 事件发生时间。
	Ts time.Time `json:"ts"`

	// Agent 事件来源 Agent。
	Agent AgentRef `json:"agent"`

	// Channel 事件分发通道。
	Channel Channel `json:"channel"`

	// Type 事件类型（如 agent.message、tool.completed）。
	Type string `json:"type"`

	// Importance 事件重要级别。
	Importance Importance `json:"importance"`

	// Parent 关联的运行上下文。
	Parent ParentRef `json:"parent"`

	// Payload 事件负载，schema 由 channel/type 的契约定义。
	Payload any `json:"payload"`
}

// NewEnvelope 构造事件信封：补全 ID 与时间戳，其余字段由调用方指定。
// ID 采用"时间戳 + crypto/rand 随机数"而非引入新依赖：同一毫秒的并发
// 事件靠随机段区分，且 ID 按时间近似有序，便于断连续传场景排序。
func NewEnvelope(sessionID string, channel Channel, typ string, importance Importance, payload any) Envelope {
	return Envelope{
		ID:         newEventID(),
		SessionID:  sessionID,
		Ts:         time.Now().UTC(),
		Channel:    channel,
		Type:       typ,
		Importance: importance,
		Payload:    payload,
	}
}

// newEventID 生成事件 ID：evt-<毫秒时间戳>-<8 字节随机数十六进制>。
func newEventID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("evt-%d-%s", time.Now().UnixMilli(), hex.EncodeToString(b))
}
