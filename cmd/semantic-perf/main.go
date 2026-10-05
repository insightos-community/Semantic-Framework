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

// semantic-perf 入口：读取观测库（默认 .output/semantic.db）聚合输出
// 性能基线报告（markdown 到 stdout）。报告口径：计量分布（purpose/model
// 占比）、每轮平均输入 token、耗时三分解（模型/工具/其他）、数据窗口。
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/log"
)

func main() {
	dbPath := flag.String("db", ".output/semantic.db", "观测库 SQLite 路径")
	flag.Parse()

	report, err := buildReport(*dbPath, time.Now().UTC())
	if err != nil {
		fmt.Fprintf(os.Stderr, "生成性能基线报告失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(report)
}

// buildReport 打开观测库并聚合生成 markdown 报告（可测试的全部逻辑在此）。
func buildReport(dbPath string, now time.Time) (string, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return "", fmt.Errorf("观测库 %s 不可读: %w", dbPath, err)
	}

	st, err := store.Open(config.StoreConfig{Driver: "sqlite", SQLitePath: dbPath},
		log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	if err != nil {
		return "", err
	}
	defer func() { _ = st.Close() }()

	window, err := st.PerfDataWindow()
	if err != nil {
		return "", err
	}
	kindStats, err := st.SummarizeSpanKinds()
	if err != nil {
		return "", err
	}
	metering, err := st.SummarizeMetering(time.Time{})
	if err != nil {
		return "", err
	}

	var b strings.Builder
	writeHeader(&b, dbPath, now)
	writeDataWindow(&b, window)
	writeMeteringDist(&b, metering)
	writePromptTokens(&b, metering)
	writeDurationSplit(&b, kindStats)
	writeRoutingNote(&b, metering)
	return b.String(), nil
}

// writeHeader 写报告头（标题/生成时间/库路径）。
func writeHeader(b *strings.Builder, dbPath string, now time.Time) {
	fmt.Fprintf(b, "# 性能基线报告\n\n")
	fmt.Fprintf(b, "- 生成时间：%s\n", now.Format(time.RFC3339))
	fmt.Fprintf(b, "- 数据库：`%s`\n", dbPath)
	fmt.Fprintf(b, "- 工具：`semantic-perf`（`cmd/semantic-perf`，`make perf-baseline` 构建）\n\n")
}

// writeDataWindow 写数据窗口段（时间范围与记录规模）。
func writeDataWindow(b *strings.Builder, w store.PerfWindow) {
	fmt.Fprintf(b, "## 数据窗口\n\n")
	fmt.Fprintf(b, "| 指标 | 值 |\n|---|---|\n")
	fmt.Fprintf(b, "| 链路数 | %d |\n", w.Traces)
	fmt.Fprintf(b, "| 跨度总数 | %d |\n", w.Spans)
	fmt.Fprintf(b, "| 计量记录数 | %d |\n", w.MeteringCalls)
	fmt.Fprintf(b, "| run 数 | %d |\n", w.Runs)
	fmt.Fprintf(b, "| 链路最早开始 | %s |\n", fmtTime(w.TraceEarliest))
	fmt.Fprintf(b, "| 链路最近开始 | %s |\n", fmtTime(w.TraceLatest))
	fmt.Fprintf(b, "| 计量最早记录 | %s |\n", fmtTime(w.MeteringEarliest))
	fmt.Fprintf(b, "| 计量最近记录 | %s |\n\n", fmtTime(w.MeteringLatest))
}

// writeMeteringDist 写计量分布段：按 purpose 与按 model 两组的
// calls/tokens 占比（calls 占比按调用次数，tokens 占比按 total tokens）。
func writeMeteringDist(b *strings.Builder, rows []store.MeteringSummary) {
	fmt.Fprintf(b, "## 计量分布\n\n")
	if len(rows) == 0 {
		fmt.Fprintf(b, "（无计量记录）\n\n")
		return
	}
	var totalCalls, totalTokens int
	for _, r := range rows {
		totalCalls += r.Calls
		totalTokens += r.TotalTokens
	}

	fmt.Fprintf(b, "按用途（purpose）分组，占比按 calls / total tokens 计算：\n\n")
	fmt.Fprintf(b, "| purpose | calls | calls 占比 | prompt | completion | total tokens | tokens 占比 | cost 估算 |\n")
	fmt.Fprintf(b, "|---|---|---|---|---|---|---|---|\n")
	for _, g := range groupMetering(rows, func(r store.MeteringSummary) string { return r.Purpose }) {
		fmt.Fprintf(b, "| %s | %d | %.1f%% | %d | %d | %d | %.1f%% | %.4f |\n",
			g.key, g.calls, pct(g.calls, totalCalls), g.prompt, g.completion,
			g.tokens, pct(g.tokens, totalTokens), g.cost)
	}
	fmt.Fprintf(b, "| **合计** | %d | 100.0%% | - | - | %d | 100.0%% | - |\n\n", totalCalls, totalTokens)

	fmt.Fprintf(b, "按模型（model）分组：\n\n")
	fmt.Fprintf(b, "| model | calls | calls 占比 | total tokens | tokens 占比 | cost 估算 |\n")
	fmt.Fprintf(b, "|---|---|---|---|---|---|\n")
	for _, g := range groupMetering(rows, func(r store.MeteringSummary) string { return r.Model }) {
		fmt.Fprintf(b, "| %s | %d | %.1f%% | %d | %.1f%% | %.4f |\n",
			g.key, g.calls, pct(g.calls, totalCalls), g.tokens, pct(g.tokens, totalTokens), g.cost)
	}
	fmt.Fprintf(b, "\n")
}

// writePromptTokens 写每轮平均输入 token 段（来源 metering.prompt_tokens），
// 并如实标注 span attrs 侧的数据缺口。
func writePromptTokens(b *strings.Builder, rows []store.MeteringSummary) {
	fmt.Fprintf(b, "## 每轮平均输入 token\n\n")
	if len(rows) == 0 {
		fmt.Fprintf(b, "（无计量记录）\n\n")
		return
	}
	groups := groupMetering(rows, func(r store.MeteringSummary) string { return r.Purpose })
	var calls, prompt int
	for _, g := range groups {
		calls += g.calls
		prompt += g.prompt
	}
	fmt.Fprintf(b, "口径：每次模型调用的平均 prompt tokens（来源 `metering.prompt_tokens`）。\n\n")
	fmt.Fprintf(b, "| purpose | 调用次数 | prompt tokens 合计 | 平均每轮输入 token |\n|---|---|---|---|\n")
	for _, g := range groups {
		fmt.Fprintf(b, "| %s | %d | %d | %.1f |\n", g.key, g.calls, g.prompt, avg(g.prompt, g.calls))
	}
	fmt.Fprintf(b, "| **全量** | %d | %d | %.1f |\n\n", calls, prompt, avg(prompt, calls))

	fmt.Fprintf(b, "> 数据缺口：trace_spans 模型跨度的 attrs 当前只记录 "+
		"content_len / finish_reason / stream_chunks，不含输入 token 与上下文体积——"+
		"每轮输入 token 以 metering 为准；上下文体积与缓存表现（§3.4 验收证据）"+
		"待 span attrs 补齐后才有数据源。\n\n")
}

// writeDurationSplit 写耗时三分解段：模型（ChatModel）/工具（Tool）/其他
// 按跨度类型归组，口径为跨度耗时合计（嵌套跨度不去重）。
func writeDurationSplit(b *strings.Builder, stats []store.SpanKindStat) {
	fmt.Fprintf(b, "## 耗时三分解（按跨度类型）\n\n")
	if len(stats) == 0 {
		fmt.Fprintf(b, "（无链路跨度）\n\n")
		return
	}
	var totalMs int64
	for _, s := range stats {
		totalMs += s.DurationMs
	}
	fmt.Fprintf(b, "归类口径：`ChatModel` → 模型，`Tool` → 工具，其余（Chain/Agent 等编排跨度）→ 其他；"+
		"耗时为跨度合计，嵌套跨度不去重。\n\n")
	fmt.Fprintf(b, "| 类别 | 跨度数 | 耗时合计 (ms) | 占比 |\n|---|---|---|---|\n")
	for _, bucket := range []struct {
		label string
		match func(kind string) bool
	}{
		{"模型", func(k string) bool { return k == "ChatModel" }},
		{"工具", func(k string) bool { return k == "Tool" }},
		{"其他", func(k string) bool { return k != "ChatModel" && k != "Tool" }},
	} {
		var spans int
		var ms int64
		for _, s := range stats {
			if bucket.match(s.Kind) {
				spans += s.Spans
				ms += s.DurationMs
			}
		}
		fmt.Fprintf(b, "| %s | %d | %d | %.1f%% |\n", bucket.label, spans, ms, pct64(ms, totalMs))
	}
	fmt.Fprintf(b, "| **合计** | - | %d | 100.0%% |\n\n", totalMs)
}

// writeRoutingNote 写模型调用用途说明：如实呈现当前 purpose 取值，并明确
// reasoning_effort 只调节当前模型，不再表示跨模型路由。
func writeRoutingNote(b *strings.Builder, rows []store.MeteringSummary) {
	purposes := make([]string, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, r := range rows {
		if !seen[r.Purpose] {
			seen[r.Purpose] = true
			purposes = append(purposes, r.Purpose)
		}
	}
	sort.Strings(purposes)

	fmt.Fprintf(b, "## 模型调用归因说明\n\n")
	if len(purposes) == 0 {
		fmt.Fprintf(b, "当前库中无计量记录，purpose 无取值。\n\n")
	} else {
		fmt.Fprintf(b, "当前库中 purpose 实际取值：`%s`。", strings.Join(purposes, "`、`"))
		fmt.Fprintf(b, "reasoning_effort 只作用于当前模型，本报告不据此推断模型切换。\n\n")
	}
}

// meteringGroup 是计量分布的一个分组聚合结果。
type meteringGroup struct {
	// key 分组键（purpose 或 model）。
	key string

	// calls 调用次数合计。
	calls int

	// prompt 输入侧 token 合计。
	prompt int

	// completion 输出侧 token 合计。
	completion int

	// tokens 总 token 合计。
	tokens int

	// cost 成本估算合计。
	cost float64
}

// groupMetering 按 keyFunc 维度聚合计量行，按总 token 降序（同量按键名）返回。
func groupMetering(rows []store.MeteringSummary, keyFunc func(store.MeteringSummary) string) []meteringGroup {
	byKey := make(map[string]*meteringGroup)
	for _, r := range rows {
		key := keyFunc(r)
		g, ok := byKey[key]
		if !ok {
			g = &meteringGroup{key: key}
			byKey[key] = g
		}
		g.calls += r.Calls
		g.prompt += r.PromptTokens
		g.completion += r.CompletionTokens
		g.tokens += r.TotalTokens
		g.cost += r.CostEstimate
	}
	groups := make([]meteringGroup, 0, len(byKey))
	for _, g := range byKey {
		groups = append(groups, *g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].tokens != groups[j].tokens {
			return groups[i].tokens > groups[j].tokens
		}
		return groups[i].key < groups[j].key
	})
	return groups
}

// pct 计算部分占整体的百分比（整体为 0 时返回 0）。
func pct(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) * 100 / float64(total)
}

// pct64 是 pct 的 int64 版本（耗时占比用）。
func pct64(part, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) * 100 / float64(total)
}

// avg 计算平均值（次数为 0 时返回 0）。
func avg(sum, count int) float64 {
	if count == 0 {
		return 0
	}
	return float64(sum) / float64(count)
}

// fmtTime 格式化时间（零值显示为 "-"，表示无数据）。
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}
