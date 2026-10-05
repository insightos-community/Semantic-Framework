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
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// SpanIOMaxRunes 是 ChatModel 输入/输出每侧写入库的上限（按 rune）。
	SpanIOMaxRunes = 16 * 1024
)

// SpanIO 是一条跨度的模型输入输出正文，不写入 trace_spans.attrs。
type SpanIO struct {
	SpanID          int64
	Input           string
	Output          string
	InputTruncated  bool
	OutputTruncated bool
	InputSHA256     string
	OutputSHA256    string
}

var (
	redactAuthorization = regexp.MustCompile(`(?i)Authorization\s*[:=]\s*\S+(?:\s+\S+)?`)
	redactBearerToken   = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._\-+/=]+`)
	redactAPIKeyEq      = regexp.MustCompile(`(?i)(api[_-]?key\s*[:=]\s*)(\S+)`)
	redactSK            = regexp.MustCompile(`(?i)\bsk-[A-Za-z0-9]{8,}`)
)

// RedactSpanIO 去掉常见密钥形态，避免模型报文把 Token 带进观测库。
func RedactSpanIO(text string) string {
	if text == "" {
		return ""
	}
	out := redactAuthorization.ReplaceAllString(text, "Authorization: ***")
	out = redactBearerToken.ReplaceAllString(out, "Bearer ***")
	out = redactAPIKeyEq.ReplaceAllString(out, `${1}***`)
	out = redactSK.ReplaceAllString(out, "sk-***")
	return out
}

// TruncateSpanIO 按 rune 截断并返回是否发生截断。
func TruncateSpanIO(text string, maxRunes int) (string, bool) {
	if maxRunes <= 0 || utf8.RuneCountInString(text) <= maxRunes {
		return text, false
	}
	var b strings.Builder
	b.Grow(maxRunes)
	n := 0
	for _, r := range text {
		if n >= maxRunes {
			return b.String(), true
		}
		b.WriteRune(r)
		n++
	}
	return b.String(), false
}

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// PrepareSpanIO 脱敏、截断并计算哈希，供 ChatModel 跨度结束时写入。
func PrepareSpanIO(spanID int64, input, output string) SpanIO {
	in := RedactSpanIO(input)
	out := RedactSpanIO(output)
	inStored, inTrunc := TruncateSpanIO(in, SpanIOMaxRunes)
	outStored, outTrunc := TruncateSpanIO(out, SpanIOMaxRunes)
	return SpanIO{
		SpanID:          spanID,
		Input:           inStored,
		Output:          outStored,
		InputTruncated:  inTrunc,
		OutputTruncated: outTrunc,
		InputSHA256:     sha256Hex(in),
		OutputSHA256:    sha256Hex(out),
	}
}

// UpsertSpanIO 写入或覆盖一条跨度的模型输入输出。
func (s *Store) UpsertSpanIO(rec SpanIO) error {
	_, err := s.db.Exec(
		`INSERT INTO trace_span_io (
			span_id, input_text, output_text, input_truncated, output_truncated,
			input_sha256, output_sha256)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(span_id) DO UPDATE SET
			input_text=excluded.input_text,
			output_text=excluded.output_text,
			input_truncated=excluded.input_truncated,
			output_truncated=excluded.output_truncated,
			input_sha256=excluded.input_sha256,
			output_sha256=excluded.output_sha256`,
		rec.SpanID, rec.Input, rec.Output, boolToInt(rec.InputTruncated),
		boolToInt(rec.OutputTruncated), rec.InputSHA256, rec.OutputSHA256,
	)
	if err != nil {
		return fmt.Errorf("写入 trace 跨度 %d 输入输出失败: %w", rec.SpanID, err)
	}
	return nil
}

// SpanIDsWithIO 返回某条链路里已落库输入输出的跨度 ID。
func (s *Store) SpanIDsWithIO(traceID string) (map[int64]struct{}, error) {
	rows, err := s.db.Query(
		`SELECT i.span_id FROM trace_span_io i
		 JOIN trace_spans s ON s.id = i.span_id
		 WHERE s.trace_id = ?`, traceID,
	)
	if err != nil {
		return nil, fmt.Errorf("查询链路 %q 的跨度输入输出标记失败: %w", traceID, err)
	}
	defer func() { _ = rows.Close() }()

	ids := make(map[int64]struct{})
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("扫描链路 %q 的跨度输入输出标记失败: %w", traceID, err)
		}
		ids[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历链路 %q 的跨度输入输出标记失败: %w", traceID, err)
	}
	return ids, nil
}

// GetSpanIO 读取属于指定链路的跨度输入输出；跨度不属于该链路时返回 ErrNotFound。
func (s *Store) GetSpanIO(traceID string, spanID int64) (SpanIO, error) {
	var rec SpanIO
	var inTrunc, outTrunc int
	err := s.db.QueryRow(
		`SELECT i.span_id, i.input_text, i.output_text, i.input_truncated,
		        i.output_truncated, i.input_sha256, i.output_sha256
		 FROM trace_span_io i
		 JOIN trace_spans s ON s.id = i.span_id
		 WHERE s.trace_id = ? AND i.span_id = ?`,
		traceID, spanID,
	).Scan(&rec.SpanID, &rec.Input, &rec.Output, &inTrunc, &outTrunc,
		&rec.InputSHA256, &rec.OutputSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return SpanIO{}, fmt.Errorf("查询链路 %q 跨度 %d 输入输出失败: %w", traceID, spanID, ErrNotFound)
	}
	if err != nil {
		return SpanIO{}, fmt.Errorf("查询链路 %q 跨度 %d 输入输出失败: %w", traceID, spanID, err)
	}
	rec.InputTruncated = inTrunc != 0
	rec.OutputTruncated = outTrunc != 0
	return rec, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
