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

// MessageAttachment 是消息元数据中的图片引用（内容在 artifact store）。
type MessageAttachment struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	MediaType  string `json:"media_type"`
	Size       int64  `json:"size"`
	ContentURL string `json:"content_url"`
}

// 对话下行事件的类型取值（envelope.type，channel 固定为 dialogue）。
const (
	// Run 状态事件由持久化状态变化触发，payload.run 始终是更新后的完整记录。
	EventTypeRunStarted      = "run.started"
	EventTypeRunWaitingInput = "run.waiting_input"
	EventTypeRunRunning      = "run.running"
	EventTypeRunCancelling   = "run.cancelling"
	EventTypeRunCompleted    = "run.completed"
	EventTypeRunFailed       = "run.failed"
	EventTypeRunCancelled    = "run.cancelled"

	// EventTypeMessageDelta 模型文本增量事件。
	EventTypeMessageDelta = "message.delta"

	// EventTypeReasoningDelta 模型服务显式返回的推理内容增量。
	EventTypeReasoningDelta = "reasoning.delta"

	// EventTypeMessageDone 本轮回复完成事件（成功或失败均以此收尾）。
	EventTypeMessageDone = "message.done"

	// EventTypeToolResult 普通工具执行完成事件（归并到同一 run 的助手活动）。
	EventTypeToolResult = "tool.result"

	// EventTypeToolCall 模型发起工具调用（携带 call_id 与原始参数）。
	EventTypeToolCall = "tool.call"

	// EventTypeSubAgentDelta SubAgent 文本增量事件（委派执行过程的冒泡下行，
	// envelope.agent 归因到成员实例，如 query-1）。
	EventTypeSubAgentDelta = "subagent.delta"

	// EventTypeSubAgentResult SubAgent 委派完成事件（携带任务与结果全文）。
	EventTypeSubAgentResult = "subagent.result"
)

// UsagePayload 是 message.done 携带的本轮 token 用量。
type UsagePayload struct {
	// PromptTokens 输入侧累计 tokens。
	PromptTokens int `json:"prompt_tokens"`

	// CompletionTokens 输出侧累计 tokens。
	CompletionTokens int `json:"completion_tokens"`

	// TotalTokens 累计总 tokens。
	TotalTokens int `json:"total_tokens"`
}

// ModelResolution 描述角色配置端点与本轮主 Runner 实际采用模型的解析结果。
// 当前版本不允许跨模型自动回退，因此 Fallback 保留为兼容字段且恒为 false。
type ModelResolution struct {
	// RequestedEndpoint Agent profile 配置的端点名。
	RequestedEndpoint string `json:"requested_endpoint"`

	// ResolvedEndpoint 注册表最终采用的端点名。
	ResolvedEndpoint string `json:"resolved_endpoint"`

	// ResolvedModel 发送给模型服务的实际 model ID。
	ResolvedModel string `json:"resolved_model"`

	// Provider 是实际提供模型服务的服务 ID，用于界面展示和审计。
	Provider string `json:"provider"`

	// Source 表示模型来自系统 Default、Agent Profile 或当前会话覆盖。
	Source string `json:"source"`

	// DefaultInherited 表示该会话 Agent 在创建快照时继承了系统 Default。
	DefaultInherited bool `json:"default_inherited"`

	// Fallback 是旧客户端兼容字段；当前版本恒为 false。
	Fallback bool `json:"fallback"`
}

// MessageDeltaPayload 是 message.delta 事件的负载。
type MessageDeltaPayload struct {
	// Reset starts the public summary of a corrected structured output exchange.
	Reset bool `json:"reset,omitempty"`
	// RunID 本次运行 ID（关联同一次回复的全部增量）。
	RunID string `json:"run_id"`

	// Text 文本增量（按到达顺序拼接即完整回复）。
	Text string `json:"text"`
}

// ReasoningDeltaPayload 是 reasoning.delta 事件负载。
type ReasoningDeltaPayload struct {
	RunID string `json:"run_id"`
	Text  string `json:"text"`
	Turn  int    `json:"turn,omitempty"`
}

type ReasoningRound struct {
	Turn int    `json:"turn"`
	Text string `json:"text"`
}

// MessageDonePayload 是 message.done 事件的负载。
type MessageDonePayload struct {
	// RunID 本次运行 ID。
	RunID string `json:"run_id"`

	// TraceID 本轮主 Agent 的精确链路 ID。前端必须直接使用该字段下钻，
	// 不能按完成时间猜测最近 Trace，否则并发会话和 SubAgent 会造成错配。
	TraceID string `json:"trace_id,omitempty"`

	// Text 完整回复文本（失败时为空）。
	Text string `json:"text"`

	// Turns 本轮运行的模型调用轮次。
	Turns int `json:"turns"`

	// Usage 本轮运行的累计 token 用量。
	Usage *UsagePayload `json:"usage,omitempty"`

	// Error 运行失败的错误描述；成功时为空。
	Error string `json:"error,omitempty"`

	// Cancelled 表示本轮由用户主动中断，不应渲染为系统故障。
	Cancelled bool `json:"cancelled,omitempty"`

	// Model 本轮主 Runner 的实际模型解析结果。
	Model *ModelResolution `json:"model,omitempty"`
}

// ToolCallPayload 是 tool.call 事件负载。
type ToolCallPayload struct {
	RunID     string `json:"run_id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolResultPayload 是 tool.result 事件的负载。
type ToolResultPayload struct {
	// RunID 本次运行 ID。
	RunID string `json:"run_id"`

	// CallID 与 tool.call 配对；旧工具链无法提供时可为空。
	CallID string `json:"call_id,omitempty"`

	// Name 模型侧工具名（已净化，可与 Trace 中的工具跨度对应）。
	Name string `json:"name"`

	// Result 工具结果文本。前端默认折叠展示；超出实时传输上限时为截断值，
	// 完整执行细节仍以 Trace/产物引用为准。
	Result string `json:"result"`

	// Truncated 表示 Result 因实时传输限界被截断。
	Truncated bool `json:"truncated,omitempty"`
}

// ToolActivity 是助手消息中持久化的单次工具活动。
type ToolActivity struct {
	CallID    string `json:"call_id"`
	AgentID   string `json:"agent_id,omitempty"`
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
	Status    string `json:"status"`
	Result    string `json:"result,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// DelegationActivity 是一次 SubAgent 委派的持久化视图。
type DelegationActivity struct {
	AgentID   string `json:"agent_id"`
	Task      string `json:"task,omitempty"`
	Text      string `json:"text,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
	Status    string `json:"status"`
}

// RunMetadata 随助手消息归档，可由 REST 完整恢复运行活动。
type RunMetadata struct {
	StartedAt           string               `json:"started_at,omitempty"`
	Status              string               `json:"status"`
	Turns               int                  `json:"turns,omitempty"`
	Usage               *UsagePayload        `json:"usage,omitempty"`
	Error               string               `json:"error,omitempty"`
	Reasoning           string               `json:"reasoning,omitempty"`
	ReasoningRounds     []ReasoningRound     `json:"reasoning_rounds,omitempty"`
	ReasoningEffort     string               `json:"reasoning_effort,omitempty"`
	ReasoningVisibility string               `json:"reasoning_visibility,omitempty"`
	Model               *ModelResolution     `json:"model,omitempty"`
	Tools               []ToolActivity       `json:"tools,omitempty"`
	Delegations         []DelegationActivity `json:"delegations,omitempty"`
}

// SubAgentDeltaPayload 是 subagent.delta 事件的负载。
type SubAgentDeltaPayload struct {
	// RunID 触发委派的 leader 运行 ID（关联同一次委派的全部增量）。
	RunID string `json:"run_id"`

	// Text SubAgent 文本增量（按到达顺序拼接即委派过程的产出文本）。
	Text string `json:"text"`
}

// SubAgentResultPayload 是 subagent.result 事件的负载。
type SubAgentResultPayload struct {
	// RunID 触发委派的 leader 运行 ID。
	RunID string `json:"run_id"`

	// Task 委派给 SubAgent 的任务文本。
	Task string `json:"task"`

	// Text 委派结果全文（SubAgent 最终回复，即 leader 看到的工具结果）。
	Text string `json:"text"`
}
