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
	"fmt"
	"strings"
	"time"
)

// defaultMeteringLimit 是计量查询的默认返回上限，防止无界查询拖垮后续 API。
const defaultMeteringLimit = 500

// Metering 是一条模型调用计量记录（按 模型/角色/用途 归因）。
type Metering struct {
	// ID 自增主键，插入后回填。
	ID int64

	// TraceID 关联的链路 ID。
	TraceID string

	// Agent 发起调用的 Agent 名。
	Agent string

	// Role Agent 的角色（角色 profile 落地前可为空）。
	Role string

	// Model 实际调用的模型 ID。
	Model string

	// Purpose 调用用途（如 chat / task）。
	Purpose string

	// PromptTokens 输入侧 token 数。
	PromptTokens int

	// CompletionTokens 输出侧 token 数。
	CompletionTokens int

	// TotalTokens 总 token 数。
	TotalTokens int

	// CostEstimate 按端点单价估算的成本。
	CostEstimate float64

	// CreatedAt 记录创建时间。
	CreatedAt time.Time
}

// MeteringFilter 是计量查询过滤器，零值字段不参与过滤。
type MeteringFilter struct {
	// TraceID 按链路 ID 精确过滤（任务计量明细的归属维度）。
	TraceID string

	// Model 按模型 ID 精确过滤。
	Model string

	// Agent 按 Agent 名精确过滤。
	Agent string

	// Since 时间范围起点（created_at >= Since），零值不限。
	Since time.Time

	// Until 时间范围终点（created_at <= Until），零值不限。
	Until time.Time

	// Limit 返回上限，<= 0 时使用默认值 defaultMeteringLimit。
	Limit int

	// Offset 分页偏移（跳过前 N 条），<= 0 时不偏移。
	Offset int
}

// InsertMetering 写入一条计量记录，返回自增 ID。
func (s *Store) InsertMetering(m Metering) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO metering
		 (trace_id, agent, role, model, purpose, prompt_tokens, completion_tokens, total_tokens, cost_estimate, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.TraceID, m.Agent, m.Role, m.Model, m.Purpose,
		m.PromptTokens, m.CompletionTokens, m.TotalTokens, m.CostEstimate, m.CreatedAt,
	)
	if err != nil {
		return 0, fmt.Errorf("写入计量记录（model=%s）失败: %w", m.Model, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("读取计量记录自增 ID 失败: %w", err)
	}
	return id, nil
}

// meteringWhere 把过滤器翻译成 WHERE 子句与参数（QueryMetering/CountMetering 共用）。
func meteringWhere(f MeteringFilter) (string, []any) {
	var where []string
	var args []any
	if f.TraceID != "" {
		where = append(where, "trace_id = ?")
		args = append(args, f.TraceID)
	}
	if f.Model != "" {
		where = append(where, "model = ?")
		args = append(args, f.Model)
	}
	if f.Agent != "" {
		where = append(where, "agent = ?")
		args = append(args, f.Agent)
	}
	if !f.Since.IsZero() {
		where = append(where, "created_at >= ?")
		args = append(args, f.Since)
	}
	if !f.Until.IsZero() {
		where = append(where, "created_at <= ?")
		args = append(args, f.Until)
	}
	if len(where) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(where, " AND "), args
}

// QueryMetering 按过滤器查询计量记录，按 ID 升序返回。
func (s *Store) QueryMetering(f MeteringFilter) ([]Metering, error) {
	where, args := meteringWhere(f)
	limit := f.Limit
	if limit <= 0 {
		limit = defaultMeteringLimit
	}

	query := `SELECT id, trace_id, agent, role, model, purpose,
		prompt_tokens, completion_tokens, total_tokens, cost_estimate, created_at
		FROM metering` + where + " ORDER BY id LIMIT ?"
	args = append(args, limit)
	if f.Offset > 0 {
		query += " OFFSET ?"
		args = append(args, f.Offset)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询计量记录失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var records []Metering
	for rows.Next() {
		var m Metering
		if err := rows.Scan(
			&m.ID, &m.TraceID, &m.Agent, &m.Role, &m.Model, &m.Purpose,
			&m.PromptTokens, &m.CompletionTokens, &m.TotalTokens, &m.CostEstimate, &m.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("扫描计量记录失败: %w", err)
		}
		records = append(records, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历计量记录失败: %w", err)
	}
	return records, nil
}

// CountMetering 统计满足过滤器的计量记录总数（与 QueryMetering 同条件，分页用）。
func (s *Store) CountMetering(f MeteringFilter) (int, error) {
	where, args := meteringWhere(f)
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM metering`+where, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("统计计量记录失败: %w", err)
	}
	return total, nil
}

// MeteringSummary 是计量聚合视图的一行（按 模型/Agent/用途 三元组分组）。
type MeteringSummary struct {
	// Model 分组维度：模型 ID。
	Model string

	// Agent 分组维度：Agent 名。
	Agent string

	// Purpose 分组维度：调用用途。
	Purpose string

	// Calls 调用次数。
	Calls int

	// PromptTokens 输入侧 token 合计。
	PromptTokens int

	// CompletionTokens 输出侧 token 合计。
	CompletionTokens int

	// TotalTokens 总 token 合计。
	TotalTokens int

	// CostEstimate 成本估算合计。
	CostEstimate float64
}

// SummarizeMetering 聚合 since 以来的计量记录（since 零值不限起点），
// 按 模型/Agent/用途 分组，按总 token 降序返回。
func (s *Store) SummarizeMetering(since time.Time) ([]MeteringSummary, error) {
	query := `SELECT model, agent, purpose, COUNT(*),
		COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0),
		COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_estimate), 0)
		FROM metering`
	var args []any
	if !since.IsZero() {
		query += " WHERE created_at >= ?"
		args = append(args, since)
	}
	query += " GROUP BY model, agent, purpose ORDER BY SUM(total_tokens) DESC, model, agent, purpose"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("聚合计量记录失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var summaries []MeteringSummary
	for rows.Next() {
		var ms MeteringSummary
		if err := rows.Scan(
			&ms.Model, &ms.Agent, &ms.Purpose, &ms.Calls,
			&ms.PromptTokens, &ms.CompletionTokens, &ms.TotalTokens, &ms.CostEstimate,
		); err != nil {
			return nil, fmt.Errorf("扫描计量聚合行失败: %w", err)
		}
		summaries = append(summaries, ms)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历计量聚合失败: %w", err)
	}
	return summaries, nil
}
