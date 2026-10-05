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

package llm

import "time"

// Usage 是一次模型调用的 token 计量。
type Usage struct {
	// PromptTokens 输入侧 token 数。
	PromptTokens int

	// CompletionTokens 输出侧 token 数。
	CompletionTokens int

	// TotalTokens 总 token 数。
	TotalTokens int
}

// MeteringRecord 是一条计量记录：按 模型/角色/用途 归因（docs/architecture/13 §4），
// 成本按端点单价估算，仅供观测，不作为计费依据。
type MeteringRecord struct {
	// TraceID 关联的链路 ID。
	TraceID string

	// Agent 发起调用的 Agent 名。
	Agent string

	// Role Agent 的角色（角色 profile 在后续里程碑落地，当前可为空）。
	Role string

	// Model 实际调用的模型 ID。
	Model string

	// Purpose 调用用途（如 chat / task）。
	Purpose string

	// Usage token 计量。
	Usage Usage

	// CostEstimate 按端点单价估算的成本。
	CostEstimate float64

	// CreatedAt 记录创建时间。
	CreatedAt time.Time
}

// EstimateCost 按每 1K tokens 单价估算一次调用的成本。
func EstimateCost(price Price, usage Usage) float64 {
	return price.Prompt*float64(usage.PromptTokens)/1000 +
		price.Completion*float64(usage.CompletionTokens)/1000
}
