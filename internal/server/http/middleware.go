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

package http

import (
	"net/http"
	"time"

	"insightos.cn/semantic-framework/pkg/log"
)

// requestIDHeader 是请求 ID 的 HTTP 头名，客户端可透传以实现全链路关联。
const requestIDHeader = "X-Request-Id"

// RequestID 返回请求 ID 中间件：透传客户端携带的 X-Request-Id，
// 缺失时生成新的 trace ID；ID 同时写入响应头与 request context
// （复用 pkg/log 的 trace_id 上下文键，日志链路自动关联）。
func RequestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(requestIDHeader)
			if id == "" {
				id = log.GenerateTraceID()
			}
			w.Header().Set(requestIDHeader, id)
			next.ServeHTTP(w, r.WithContext(log.ContextWithTraceID(r.Context(), id)))
		})
	}
}

// Recovery 返回 panic 恢复中间件：handler panic 时记录 ERROR 日志
// （含 trace_id），并向客户端返回 500 统一错误，避免进程崩溃。
func Recovery(logger *log.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.WithTraceID(log.TraceIDFromContext(r.Context())).
						Error("HTTP 处理发生 panic", "panic", rec, "path", r.URL.Path)
					WriteError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务内部错误")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Logging 返回访问日志中间件：请求完成后记录 INFO 日志，
// 含 method/path/status/耗时/trace_id 五个关键字段。
func Logging(logger *log.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			// handler 未显式调用 WriteHeader 时（隐式 200），status 保持零值。
			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			logger.WithTraceID(log.TraceIDFromContext(r.Context())).Info("HTTP 请求完成",
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"duration", time.Since(start).String(),
			)
		})
	}
}

// statusWriter 包装 ResponseWriter 以捕获状态码（WriteHeader 只认首次调用，
// 与 net/http 语义一致）。
type statusWriter struct {
	http.ResponseWriter
	status int
}

// WriteHeader 记录首个状态码后透传给底层 ResponseWriter。
func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
