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

package handlers

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/log"
)

// defaultMeteringWindow 是 summary 端点缺省的聚合时间窗。
const defaultMeteringWindow = 24 * time.Hour

// MeteringHandler 是模型调用计量只读视图（架构文档 13 §4）的 REST 处理器：
// 时间窗聚合（成本/用量归因）与单条链路计量明细。
type MeteringHandler struct {
	// st 元数据存储。
	st *store.Store

	// logger 结构化日志器。
	logger *log.Logger
}

// NewMeteringHandler 创建计量处理器。
func NewMeteringHandler(st *store.Store, logger *log.Logger) *MeteringHandler {
	return &MeteringHandler{st: st, logger: logger}
}

// meteringSummaryView 是 summary 聚合行的响应视图（按 模型/Agent/用途 分组）。
type meteringSummaryView struct {
	// Model 分组维度：模型 ID。
	Model string `json:"model"`

	// Agent 分组维度：Agent 名。
	Agent string `json:"agent"`

	// Purpose 分组维度：调用用途。
	Purpose string `json:"purpose"`

	// Calls 调用次数。
	Calls int `json:"calls"`

	// PromptTokens 输入侧 token 合计。
	PromptTokens int `json:"prompt_tokens"`

	// CompletionTokens 输出侧 token 合计。
	CompletionTokens int `json:"completion_tokens"`

	// TotalTokens 总 token 合计。
	TotalTokens int `json:"total_tokens"`

	// CostEstimate 成本估算合计。
	CostEstimate float64 `json:"cost_estimate"`
}

// meteringRecordView 是链路计量明细行的响应视图。
type meteringRecordView struct {
	// TraceID 关联的链路 ID。
	TraceID string `json:"trace_id"`

	// Agent 发起调用的 Agent 名。
	Agent string `json:"agent"`

	// Role Agent 的角色。
	Role string `json:"role"`

	// Model 实际调用的模型 ID。
	Model string `json:"model"`

	// Purpose 调用用途。
	Purpose string `json:"purpose"`

	// PromptTokens 输入侧 token 数。
	PromptTokens int `json:"prompt_tokens"`

	// CompletionTokens 输出侧 token 数。
	CompletionTokens int `json:"completion_tokens"`

	// TotalTokens 总 token 数。
	TotalTokens int `json:"total_tokens"`

	// CostEstimate 成本估算。
	CostEstimate float64 `json:"cost_estimate"`

	// CreatedAt 记录创建时间。
	CreatedAt time.Time `json:"created_at"`
}

// HandleSummary 处理 GET /api/v1/metering/summary?window=24h：聚合时间窗内
// 的计量记录（按 模型/Agent/用途 分组，按总 token 降序）。
func (h *MeteringHandler) HandleSummary(w http.ResponseWriter, r *http.Request) {
	window := defaultMeteringWindow
	if raw := r.URL.Query().Get("window"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, CodeBadRequest,
				"window 参数非法（Go duration，示例：1h、24h、168h）")
			return
		}
		window = parsed
	}

	since := time.Now().UTC().Add(-window)
	summaries, err := h.st.SummarizeMetering(since)
	if err != nil {
		h.logger.WithError(err).Error("聚合计量记录失败", "window", window)
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	views := make([]meteringSummaryView, 0, len(summaries))
	for _, ms := range summaries {
		views = append(views, meteringSummaryView{
			Model: ms.Model, Agent: ms.Agent, Purpose: ms.Purpose, Calls: ms.Calls,
			PromptTokens: ms.PromptTokens, CompletionTokens: ms.CompletionTokens,
			TotalTokens: ms.TotalTokens, CostEstimate: ms.CostEstimate,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"window": window.String(), "since": since, "rows": views,
	})
}

// HandleTraceMetering 处理 GET /api/v1/metering/traces/{id}：按链路 ID
// 分页查询模型调用计量明细。当前 Task 运行记录尚未接入该接口，因此不能把
// Trace 明细伪装成 Task。
func (h *MeteringHandler) HandleTraceMetering(w http.ResponseWriter, r *http.Request) {
	traceID := chi.URLParam(r, "id")
	page, pageSize := parsePageLimit(r, maxListPageSize)

	filter := store.MeteringFilter{TraceID: traceID, Limit: pageSize, Offset: (page - 1) * pageSize}
	records, err := h.st.QueryMetering(filter)
	if err != nil {
		h.logger.WithError(err).Error("查询链路计量明细失败", "trace_id", traceID)
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	total, err := h.st.CountMetering(store.MeteringFilter{TraceID: traceID})
	if err != nil {
		h.logger.WithError(err).Error("统计链路计量明细失败", "trace_id", traceID)
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}

	views := make([]meteringRecordView, 0, len(records))
	for _, m := range records {
		views = append(views, meteringRecordView{
			TraceID: m.TraceID, Agent: m.Agent, Role: m.Role, Model: m.Model, Purpose: m.Purpose,
			PromptTokens: m.PromptTokens, CompletionTokens: m.CompletionTokens,
			TotalTokens: m.TotalTokens, CostEstimate: m.CostEstimate, CreatedAt: m.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"records": views, "page": page, "page_size": pageSize, "total": total,
	})
}
