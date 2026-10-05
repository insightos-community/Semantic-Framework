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
	"testing"
)

// TestGenerateTraceIDLength 验证生成的 Trace ID 为 32 字符十六进制字符串。
func TestGenerateTraceIDLength(t *testing.T) {
	if id := GenerateTraceID(); len(id) != 32 {
		t.Errorf("TraceID 长度应为 32（16 字节十六进制编码），实际: %d", len(id))
	}
}

// TestGenerateTraceIDUniqueness 验证连续生成的 Trace ID 不重复。
func TestGenerateTraceIDUniqueness(t *testing.T) {
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		id := GenerateTraceID()
		if seen[id] {
			t.Fatalf("第 %d 次生成的 TraceID 与之前重复: %s", i, id)
		}
		seen[id] = true
	}
}

// TestGenerateTraceIDHexCharset 验证 Trace ID 仅包含合法的十六进制字符。
func TestGenerateTraceIDHexCharset(t *testing.T) {
	for _, c := range GenerateTraceID() {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !isHex {
			t.Errorf("非法字符 %c 出现在 TraceID 中", c)
		}
	}
}

// TestContextWithTraceIDRoundTrip 验证 Trace ID 可在 context 中存取。
func TestContextWithTraceIDRoundTrip(t *testing.T) {
	id := GenerateTraceID()
	ctx := ContextWithTraceID(context.Background(), id)
	if got := TraceIDFromContext(ctx); got != id {
		t.Errorf("从 context 提取的 TraceID = %q，期望 %q", got, id)
	}
}

// TestTraceIDFromContextEmpty 验证空 context 返回空字符串。
func TestTraceIDFromContextEmpty(t *testing.T) {
	if got := TraceIDFromContext(context.Background()); got != "" {
		t.Errorf("空 context 应返回空字符串，实际: %q", got)
	}
}
