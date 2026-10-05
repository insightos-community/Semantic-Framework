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
	"strings"
	"time"
)

// Span 是一条链路跨度记录：内核（模型调用、工具执行等）的一次可观测事件。
type Span struct {
	// ID 自增主键，插入后回填。
	ID int64

	// TraceID 所属链路 ID，同一次 Agent 运行的全部跨度共享。
	TraceID string

	// ParentID 父跨度 ID，根跨度为空。
	ParentID string

	// Name 跨度名（组件实现标识，如 OpenAI / Mock）。
	Name string

	// Kind 跨度类型（组件类别，如 ChatModel / Tool）。
	Kind string

	// StartedAt 跨度开始时间。
	StartedAt time.Time

	// DurationMs 跨度耗时（毫秒）。
	DurationMs int64

	// Attrs 附加属性（JSON 对象字符串）。
	Attrs string
}

// InsertSpan 写入一条链路跨度，返回自增 ID。
func (s *Store) InsertSpan(sp Span) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO trace_spans (trace_id, parent_id, name, kind, started_at, duration_ms, attrs)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sp.TraceID, sp.ParentID, sp.Name, sp.Kind, sp.StartedAt, sp.DurationMs, sp.Attrs,
	)
	if err != nil {
		return 0, fmt.Errorf("写入 trace 跨度 %q 失败: %w", sp.Name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("读取 trace 跨度自增 ID 失败: %w", err)
	}
	return id, nil
}

// CompleteSpan 完成一个已经在 OnStart 写入的跨度。先写入再完成，使子跨度
// 可以在执行期间稳定引用父跨度 ID；更新失败由 TraceHandler 记录，不影响业务。
func (s *Store) CompleteSpan(id int64, durationMs int64, attrs string) error {
	res, err := s.db.Exec(`UPDATE trace_spans SET duration_ms = ?, attrs = ? WHERE id = ?`,
		durationMs, attrs, id)
	if err != nil {
		return fmt.Errorf("完成 trace 跨度 %d 失败: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取 trace 跨度 %d 更新结果失败: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("完成 trace 跨度 %d 失败: 记录不存在", id)
	}
	return nil
}

// QuerySpans 按链路 ID 查询全部跨度，按 ID 升序（即写入顺序）返回。
func (s *Store) QuerySpans(traceID string) ([]Span, error) {
	rows, err := s.db.Query(
		`SELECT id, trace_id, parent_id, name, kind, started_at, duration_ms, attrs
		 FROM trace_spans WHERE trace_id = ? ORDER BY id`, traceID,
	)
	if err != nil {
		return nil, fmt.Errorf("查询链路 %q 的跨度失败: %w", traceID, err)
	}
	defer func() { _ = rows.Close() }()

	spans, err := scanSpans(rows)
	if err != nil {
		return nil, fmt.Errorf("查询链路 %q 的跨度失败: %w", traceID, err)
	}
	return spans, nil
}

// TraceSummary 是一条链路的聚合视图（trace 列表端点的一行）。
type TraceSummary struct {
	// TraceID 链路 ID。
	TraceID string

	// Name 链路名（最早开始的跨度名；回调写入序中根跨度最先开始）。
	Name string

	// Kind 链路类型（最早开始跨度的组件类别）。
	Kind string

	// StartedAt 链路开始时间（最早跨度的开始时间）。
	StartedAt time.Time

	// DurationMs 全部跨度耗时合计（毫秒，嵌套跨度不去重）。
	DurationMs int64

	// SpanCount 链路包含的跨度数。
	SpanCount int
}

// TraceFilter 是 trace 列表查询过滤器，零值字段不参与过滤。
type TraceFilter struct {
	// ProjectID 只返回该 Project 的 Run 所关联链路。
	ProjectID string

	// TraceID 按链路 ID 精确过滤。
	TraceID string
}

// ListTraces 按过滤器分页查询链路聚合视图（按开始时间倒序），返回本页与总链路数。
// 实现分两步：先在 SQL 侧按 trace_id 聚合出分页（MIN(started_at) 只参与排序
// 不落结果集——聚合表达式经驱动返回字符串而非 time.Time），再按页内 trace_id
// 回查最早开始的跨度补 name/kind/started_at。
func (s *Store) ListTraces(f TraceFilter, limit, offset int) ([]TraceSummary, int, error) {
	conditions := make([]string, 0, 2)
	var args []any
	if f.ProjectID != "" {
		conditions = append(conditions, `EXISTS (SELECT 1 FROM run_sessions
			WHERE run_sessions.trace_id = trace_spans.trace_id
			  AND run_sessions.project_id = ?)`)
		args = append(args, f.ProjectID)
	}
	if f.TraceID != "" {
		conditions = append(conditions, "trace_id = ?")
		args = append(args, f.TraceID)
	}
	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}

	var total int
	if err := s.db.QueryRow(
		`SELECT COUNT(DISTINCT trace_id) FROM trace_spans`+where, args...,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计链路数失败: %w", err)
	}

	rows, err := s.db.Query(
		`SELECT trace_id, COUNT(*), COALESCE(SUM(duration_ms), 0)
		 FROM trace_spans`+where+`
		 GROUP BY trace_id ORDER BY MIN(started_at) DESC, trace_id DESC
		 LIMIT ? OFFSET ?`, append(args, limit, offset)...,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("查询链路列表失败: %w", err)
	}
	var summaries []TraceSummary
	for rows.Next() {
		var ts TraceSummary
		if err := rows.Scan(&ts.TraceID, &ts.SpanCount, &ts.DurationMs); err != nil {
			_ = rows.Close()
			return nil, 0, fmt.Errorf("扫描链路聚合行失败: %w", err)
		}
		summaries = append(summaries, ts)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, 0, fmt.Errorf("遍历链路列表失败: %w", err)
	}
	_ = rows.Close()

	if err := s.fillTraceRoots(summaries); err != nil {
		return nil, 0, err
	}
	return summaries, total, nil
}

// fillTraceRoots 为本页链路补 name/kind/started_at：取每条链路最早开始
// （同刻按写入序）的跨度。页大小有上限（≤100），IN 子句规模可控。
func (s *Store) fillTraceRoots(summaries []TraceSummary) error {
	if len(summaries) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(summaries)), ",")
	args := make([]any, 0, len(summaries))
	for _, ts := range summaries {
		args = append(args, ts.TraceID)
	}
	rows, err := s.db.Query(
		`SELECT trace_id, name, kind, started_at FROM trace_spans
		 WHERE trace_id IN (`+placeholders+`) ORDER BY started_at, id`, args...,
	)
	if err != nil {
		return fmt.Errorf("查询链路根跨度失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	roots := make(map[string]TraceSummary, len(summaries))
	for rows.Next() {
		var traceID, name, kind string
		var startedAt time.Time
		if err := rows.Scan(&traceID, &name, &kind, &startedAt); err != nil {
			return fmt.Errorf("扫描链路根跨度失败: %w", err)
		}
		if _, ok := roots[traceID]; !ok {
			roots[traceID] = TraceSummary{Name: name, Kind: kind, StartedAt: startedAt}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("遍历链路根跨度失败: %w", err)
	}
	for i := range summaries {
		if root, ok := roots[summaries[i].TraceID]; ok {
			summaries[i].Name = root.Name
			summaries[i].Kind = root.Kind
			summaries[i].StartedAt = root.StartedAt
		}
	}
	return nil
}

// GetSpans 按链路 ID 查询全部跨度，按开始时间升序（同刻按写入序）返回，
// 供前端按 parent_id 组树。
func (s *Store) GetSpans(traceID string) ([]Span, error) {
	rows, err := s.db.Query(
		`SELECT id, trace_id, parent_id, name, kind, started_at, duration_ms, attrs
		 FROM trace_spans WHERE trace_id = ? ORDER BY started_at, id`, traceID,
	)
	if err != nil {
		return nil, fmt.Errorf("查询链路 %q 的跨度失败: %w", traceID, err)
	}
	defer func() { _ = rows.Close() }()

	spans, err := scanSpans(rows)
	if err != nil {
		return nil, fmt.Errorf("查询链路 %q 的跨度失败: %w", traceID, err)
	}
	return spans, nil
}

// scanSpans 是跨度结果集的公共扫描逻辑（QuerySpans/GetSpans 共用）。
func scanSpans(rows *sql.Rows) ([]Span, error) {
	var spans []Span
	for rows.Next() {
		var sp Span
		if err := rows.Scan(
			&sp.ID, &sp.TraceID, &sp.ParentID, &sp.Name, &sp.Kind,
			&sp.StartedAt, &sp.DurationMs, &sp.Attrs,
		); err != nil {
			return nil, fmt.Errorf("扫描 trace 跨度失败: %w", err)
		}
		spans = append(spans, sp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 trace 跨度失败: %w", err)
	}
	return spans, nil
}
