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

package log

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

// traceIDKey 是 Trace ID 在 context 中的键类型，使用未导出结构体防止与其他包冲突。
type traceIDKey struct{}

// GenerateTraceID 生成一个 16 字节（32 字符十六进制）的随机 Trace ID。
// 长度与 OpenTelemetry TraceID 规范一致，便于后续对接分布式追踪系统；
// 使用 crypto/rand 保证随机性，避免并发下的 ID 冲突。
func GenerateTraceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ContextWithTraceID 将 Trace ID 注入到 context 中，供后续中间件和日志器提取。
func ContextWithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, traceID)
}

// TraceIDFromContext 从 context 中提取 Trace ID。
// 若 context 中不存在 Trace ID，返回空字符串。
func TraceIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(traceIDKey{}).(string); ok {
		return id
	}
	return ""
}
