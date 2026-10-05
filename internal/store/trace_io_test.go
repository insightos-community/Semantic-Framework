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

package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPrepareSpanIORedactsAndTruncates(t *testing.T) {
	long := strings.Repeat("汉", SpanIOMaxRunes+8)
	rec := PrepareSpanIO(7, "Authorization: Bearer secret-token api_key=sk-abcdef1234567890", long)
	if rec.SpanID != 7 {
		t.Fatalf("span_id 不符: %d", rec.SpanID)
	}
	if strings.Contains(rec.Input, "secret-token") || strings.Contains(rec.Input, "sk-abcdef") ||
		strings.Contains(rec.Input, "Bearer secret") {
		t.Fatalf("输入应脱敏，实际: %q", rec.Input)
	}
	if !strings.Contains(rec.Input, "Authorization") || !strings.Contains(rec.Input, "***") {
		t.Fatalf("脱敏后应保留键名，实际: %q", rec.Input)
	}
	if !rec.OutputTruncated || rec.InputTruncated {
		t.Fatalf("只应截断超长输出，实际 in=%v out=%v", rec.InputTruncated, rec.OutputTruncated)
	}
	if rec.Output != strings.Repeat("汉", SpanIOMaxRunes) {
		t.Fatalf("应按 rune 截断输出，实际长度 %d", len([]rune(rec.Output)))
	}
	if rec.InputSHA256 == "" || rec.OutputSHA256 == "" {
		t.Fatal("应计算 sha256")
	}
}

func TestSpanIORoundTrip(t *testing.T) {
	st := openTestStore(t)
	id, err := st.InsertSpan(Span{
		TraceID: "tr-io", Name: "Mock", Kind: "ChatModel",
		StartedAt: time.Now().UTC(), DurationMs: 1, Attrs: `{}`,
	})
	if err != nil {
		t.Fatalf("InsertSpan 失败: %v", err)
	}
	other, err := st.InsertSpan(Span{
		TraceID: "tr-other", Name: "Mock", Kind: "ChatModel",
		StartedAt: time.Now().UTC(), DurationMs: 1, Attrs: `{}`,
	})
	if err != nil {
		t.Fatalf("InsertSpan other 失败: %v", err)
	}
	if err := st.UpsertSpanIO(PrepareSpanIO(id, "user: hello", "assistant: hi")); err != nil {
		t.Fatalf("UpsertSpanIO 失败: %v", err)
	}
	if err := st.UpsertSpanIO(PrepareSpanIO(other, "other", "other")); err != nil {
		t.Fatalf("UpsertSpanIO other 失败: %v", err)
	}

	ids, err := st.SpanIDsWithIO("tr-io")
	if err != nil {
		t.Fatalf("SpanIDsWithIO 失败: %v", err)
	}
	if _, ok := ids[id]; !ok || len(ids) != 1 {
		t.Fatalf("tr-io 应只有本链路的 has_io，实际: %v", ids)
	}

	rec, err := st.GetSpanIO("tr-io", id)
	if err != nil {
		t.Fatalf("GetSpanIO 失败: %v", err)
	}
	if rec.Input != "user: hello" || rec.Output != "assistant: hi" {
		t.Fatalf("正文往返不符: %+v", rec)
	}

	_, err = st.GetSpanIO("tr-io", other)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨链路读取应 404，实际: %v", err)
	}
	_, err = st.GetSpanIO("tr-io", id+99)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的跨度应 404，实际: %v", err)
	}
}
