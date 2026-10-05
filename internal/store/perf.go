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
	"database/sql"
	"fmt"
	"time"
)

// SpanKindStat 是按跨度类型聚合的一行（耗时三分解的数据源）。
type SpanKindStat struct {
	// Kind 跨度类型（组件类别，如 ChatModel / Tool）。
	Kind string

	// Spans 跨度条数。
	Spans int

	// DurationMs 耗时合计（毫秒，嵌套跨度不去重）。
	DurationMs int64
}

// SummarizeSpanKinds 按跨度类型聚合条数与耗时合计，按耗时降序（同耗时按类型名）返回。
func (s *Store) SummarizeSpanKinds() ([]SpanKindStat, error) {
	rows, err := s.db.Query(
		`SELECT kind, COUNT(*), COALESCE(SUM(duration_ms), 0)
		 FROM trace_spans GROUP BY kind ORDER BY SUM(duration_ms) DESC, kind`,
	)
	if err != nil {
		return nil, fmt.Errorf("按类型聚合跨度失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var stats []SpanKindStat
	for rows.Next() {
		var st SpanKindStat
		if err := rows.Scan(&st.Kind, &st.Spans, &st.DurationMs); err != nil {
			return nil, fmt.Errorf("扫描跨度类型聚合行失败: %w", err)
		}
		stats = append(stats, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历跨度类型聚合失败: %w", err)
	}
	return stats, nil
}

// PerfWindow 是性能基线的数据窗口：观测数据的时间范围与规模。
// 无对应记录时时间为零值、计数为 0。
type PerfWindow struct {
	// TraceEarliest 链路跨度的最早开始时间。
	TraceEarliest time.Time

	// TraceLatest 链路跨度的最近开始时间。
	TraceLatest time.Time

	// Traces 链路数（distinct trace_id）。
	Traces int

	// Spans 跨度总数。
	Spans int

	// MeteringEarliest 计量记录的最早创建时间。
	MeteringEarliest time.Time

	// MeteringLatest 计量记录的最近创建时间。
	MeteringLatest time.Time

	// MeteringCalls 计量记录总数。
	MeteringCalls int

	// Runs Agent 运行数（run_sessions 总数）。
	Runs int
}

// PerfDataWindow 汇总观测数据窗口。
// 为什么不用 MIN/MAX 聚合取时间边界：实测 modernc.org/sqlite 对聚合
// 表达式返回字符串而非 time.Time（见 ListTraces 的实现注记），排序后
// LIMIT 1 取边界行的 started_at/created_at 列才能直接扫进 time.Time。
func (s *Store) PerfDataWindow() (PerfWindow, error) {
	var w PerfWindow

	if err := s.db.QueryRow(
		`SELECT COUNT(DISTINCT trace_id), COUNT(*) FROM trace_spans`,
	).Scan(&w.Traces, &w.Spans); err != nil {
		return PerfWindow{}, fmt.Errorf("统计链路规模失败: %w", err)
	}
	if err := s.scanTimeBoundary(`SELECT started_at FROM trace_spans ORDER BY started_at, id LIMIT 1`,
		&w.TraceEarliest); err != nil {
		return PerfWindow{}, fmt.Errorf("查询链路最早时间失败: %w", err)
	}
	if err := s.scanTimeBoundary(`SELECT started_at FROM trace_spans ORDER BY started_at DESC, id DESC LIMIT 1`,
		&w.TraceLatest); err != nil {
		return PerfWindow{}, fmt.Errorf("查询链路最近时间失败: %w", err)
	}

	if err := s.db.QueryRow(`SELECT COUNT(*) FROM metering`).Scan(&w.MeteringCalls); err != nil {
		return PerfWindow{}, fmt.Errorf("统计计量记录数失败: %w", err)
	}
	if err := s.scanTimeBoundary(`SELECT created_at FROM metering ORDER BY created_at, id LIMIT 1`,
		&w.MeteringEarliest); err != nil {
		return PerfWindow{}, fmt.Errorf("查询计量最早时间失败: %w", err)
	}
	if err := s.scanTimeBoundary(`SELECT created_at FROM metering ORDER BY created_at DESC, id DESC LIMIT 1`,
		&w.MeteringLatest); err != nil {
		return PerfWindow{}, fmt.Errorf("查询计量最近时间失败: %w", err)
	}

	if err := s.db.QueryRow(`SELECT COUNT(*) FROM run_sessions`).Scan(&w.Runs); err != nil {
		return PerfWindow{}, fmt.Errorf("统计 run 数失败: %w", err)
	}
	return w, nil
}

// scanTimeBoundary 执行取边界时间的查询并把结果扫进 target；
// 空集（sql.ErrNoRows）保持零值、不视为错误。
func (s *Store) scanTimeBoundary(query string, target *time.Time) error {
	err := s.db.QueryRow(query).Scan(target)
	if err == sql.ErrNoRows {
		return nil
	}
	return err
}
