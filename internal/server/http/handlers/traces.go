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
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/log"
)

// TracesHandler 是链路追踪只读视图（架构文档 13 §8）的 REST 处理器：
// trace 聚合列表与 span 明细，供前端运行检查器读取。
type TracesHandler struct {
	// st 元数据存储。
	st *store.Store

	// logger 结构化日志器。
	logger *log.Logger
}

// NewTracesHandler 创建链路追踪处理器。
func NewTracesHandler(st *store.Store, logger *log.Logger) *TracesHandler {
	return &TracesHandler{st: st, logger: logger}
}

// traceView 是 trace 列表行的响应视图。
type traceView struct {
	// TraceID 链路 ID。
	TraceID string `json:"trace_id"`

	// Name 链路名（最早开始的跨度名）。
	Name string `json:"name"`

	// Kind 链路类型（最早开始跨度的组件类别）。
	Kind string `json:"kind"`

	// StartedAt 链路开始时间。
	StartedAt time.Time `json:"started_at"`

	// DurationMs 全部跨度耗时合计（毫秒）。
	DurationMs int64 `json:"duration_ms"`

	// SpanCount 跨度数。
	SpanCount int `json:"span_count"`
}

// spanView 是 span 明细行的响应视图（attrs 以 JSON 对象下发）。
type spanView struct {
	// ID 跨度自增 ID。
	ID int64 `json:"id"`

	// ParentID 父跨度 ID（前端组树用），根跨度为空串。
	ParentID string `json:"parent_id"`

	// Name 跨度名。
	Name string `json:"name"`

	// Kind 跨度类型（组件类别）。
	Kind string `json:"kind"`

	// StartedAt 跨度开始时间。
	StartedAt time.Time `json:"started_at"`

	// DurationMs 跨度耗时（毫秒）。
	DurationMs int64 `json:"duration_ms"`

	// Attrs 附加属性（JSON 对象）。
	Attrs json.RawMessage `json:"attrs"`

	// HasIO 是否另有模型输入输出正文（正文不在本列表）。
	HasIO bool `json:"has_io"`
}

// HandleListTraces 处理 GET /api/v1/traces：分页查询 trace 聚合列表。
// trace_id 精确匹配链路 ID；结果按开始时间倒序返回。
func (h *TracesHandler) HandleListTraces(w http.ResponseWriter, r *http.Request) {
	page, pageSize := parsePageLimit(r, maxListPageSize)

	traceID := r.URL.Query().Get("trace_id")
	projectID := r.URL.Query().Get("project_id")
	if projectID != "" {
		if err := h.st.ProjectOwnedByUser(auth.UserIDFromContext(r.Context()),
			projectID); err != nil {
			writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "Project 不存在")
			return
		}
	}

	summaries, total, err := h.st.ListTraces(
		store.TraceFilter{ProjectID: projectID, TraceID: traceID},
		pageSize, (page-1)*pageSize)
	if err != nil {
		h.logger.WithError(err).Error("查询 trace 列表失败")
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	views := make([]traceView, 0, len(summaries))
	for _, ts := range summaries {
		views = append(views, traceView{
			TraceID: ts.TraceID, Name: ts.Name, Kind: ts.Kind,
			StartedAt: ts.StartedAt, DurationMs: ts.DurationMs, SpanCount: ts.SpanCount,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"traces": views, "page": page, "page_size": pageSize, "total": total,
	})
}

// HandleGetSpans 处理 GET /api/v1/traces/{trace_id}/spans：返回链路的全部
// span（按开始时间升序，含 parent_id 供前端组树；链路不存在时为空列表）。
func (h *TracesHandler) HandleGetSpans(w http.ResponseWriter, r *http.Request) {
	traceID := chi.URLParam(r, "trace_id")
	if projectID := r.URL.Query().Get("project_id"); projectID != "" {
		run, err := h.st.GetRunSessionByTraceID(traceID)
		if errors.Is(err, store.ErrNotFound) || err == nil && run.ProjectID != projectID ||
			h.st.ProjectOwnedByUser(auth.UserIDFromContext(r.Context()), projectID) != nil {
			writeError(w, http.StatusNotFound, "TRACE_NOT_FOUND", "Trace 不存在")
			return
		}
		if err != nil {
			h.logger.WithError(err).Error("校验 Trace Project 归属失败", "trace_id", traceID)
			writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
			return
		}
	}
	spans, err := h.st.GetSpans(traceID)
	if err != nil {
		h.logger.WithError(err).Error("查询 span 明细失败", "trace_id", traceID)
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	hasIO, err := h.st.SpanIDsWithIO(traceID)
	if err != nil {
		h.logger.WithError(err).Error("查询 span 输入输出标记失败", "trace_id", traceID)
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	views := make([]spanView, 0, len(spans))
	for _, sp := range spans {
		_, ok := hasIO[sp.ID]
		views = append(views, spanView{
			ID: sp.ID, ParentID: sp.ParentID, Name: sp.Name, Kind: sp.Kind,
			StartedAt: sp.StartedAt, DurationMs: sp.DurationMs,
			Attrs: rawJSON(sp.Attrs, "{}"), HasIO: ok,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"spans": views})
}

// HandleGetSpanIO 处理 GET /api/v1/traces/{trace_id}/spans/{span_id}/io：
// 按需返回 ChatModel 输入输出正文。鉴权与 span 列表明细相同。
func (h *TracesHandler) HandleGetSpanIO(w http.ResponseWriter, r *http.Request) {
	traceID := chi.URLParam(r, "trace_id")
	spanID, err := strconv.ParseInt(chi.URLParam(r, "span_id"), 10, 64)
	if err != nil || spanID <= 0 {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "span_id 不是合法整数")
		return
	}
	if projectID := r.URL.Query().Get("project_id"); projectID != "" {
		run, runErr := h.st.GetRunSessionByTraceID(traceID)
		if errors.Is(runErr, store.ErrNotFound) || runErr == nil && run.ProjectID != projectID ||
			h.st.ProjectOwnedByUser(auth.UserIDFromContext(r.Context()), projectID) != nil {
			writeError(w, http.StatusNotFound, "TRACE_NOT_FOUND", "Trace 不存在")
			return
		}
		if runErr != nil {
			h.logger.WithError(runErr).Error("校验 Trace Project 归属失败", "trace_id", traceID)
			writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
			return
		}
	}
	rec, err := h.st.GetSpanIO(traceID, spanID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "SPAN_IO_NOT_FOUND", "跨度输入输出不存在")
			return
		}
		h.logger.WithError(err).Error("查询 span 输入输出失败", "trace_id", traceID, "span_id", spanID)
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"span_id":          rec.SpanID,
		"input":            rec.Input,
		"output":           rec.Output,
		"input_truncated":  rec.InputTruncated,
		"output_truncated": rec.OutputTruncated,
		"input_sha256":     rec.InputSHA256,
		"output_sha256":    rec.OutputSHA256,
	})
}
