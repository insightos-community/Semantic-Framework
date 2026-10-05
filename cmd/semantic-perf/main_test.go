package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/log"
)

// openPerfTestStore 在临时目录建库并写入基线报告测试夹具：
// 两条链路（chat 带工具调用、observe 单模型调用）+ 三条计量 + 一个 run。
func openPerfTestStore(t *testing.T) (string, time.Time) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "perf.db")
	st := storetest.OpenMigratedAt(t, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}), dbPath)

	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	for _, sp := range []store.Span{
		{TraceID: "tr-chat", Name: "Mock", Kind: "ChatModel", StartedAt: base, DurationMs: 120, Attrs: `{"stream_chunks":1}`},
		{TraceID: "tr-chat", Name: "artifact_put", Kind: "Tool", StartedAt: base.Add(time.Second), DurationMs: 30, Attrs: `{}`},
		{TraceID: "tr-chat", Name: "Mock", Kind: "ChatModel", StartedAt: base.Add(2 * time.Second), DurationMs: 80, Attrs: `{"stream_chunks":1}`},
		{TraceID: "tr-query", Name: "Mock", Kind: "ChatModel", StartedAt: base.Add(time.Minute), DurationMs: 50, Attrs: `{}`},
	} {
		if _, err := st.InsertSpan(sp); err != nil {
			t.Fatalf("InsertSpan 失败: %v", err)
		}
	}
	for _, m := range []store.Metering{
		{TraceID: "tr-chat", Agent: "leader", Model: "mock", Purpose: "chat",
			PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, CreatedAt: base},
		{TraceID: "tr-chat", Agent: "leader", Model: "mock", Purpose: "chat",
			PromptTokens: 140, CompletionTokens: 60, TotalTokens: 200, CreatedAt: base.Add(2 * time.Second)},
		{TraceID: "tr-query", Agent: "query-1", Model: "mock", Purpose: "query",
			PromptTokens: 60, CompletionTokens: 40, TotalTokens: 100, CreatedAt: base.Add(time.Minute)},
	} {
		if _, err := st.InsertMetering(m); err != nil {
			t.Fatalf("InsertMetering 失败: %v", err)
		}
	}
	if err := st.CreateRunSession(store.RunSession{
		ID: "run-1", AgentName: "leader", ChatSessionID: "cs-1",
		Status: store.RunStatusCompleted, StartedAt: base,
	}); err != nil {
		t.Fatalf("CreateRunSession 失败: %v", err)
	}
	return dbPath, base
}

// TestBuildReport 用夹具库验证报告的聚合正确性与关键段落形态。
func TestBuildReport(t *testing.T) {
	dbPath, base := openPerfTestStore(t)

	report, err := buildReport(dbPath, base.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("buildReport 失败: %v", err)
	}

	// 数据窗口：链路 2、跨度 4、计量 3、run 1，时间边界如实呈现。
	for _, want := range []string{
		"| 链路数 | 2 |", "| 跨度总数 | 4 |", "| 计量记录数 | 3 |", "| run 数 | 1 |",
		"| 链路最早开始 | " + base.Format(time.RFC3339) + " |",
		"| 计量最近记录 | " + base.Add(time.Minute).Format(time.RFC3339) + " |",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("报告应包含数据窗口项 %q\n---\n%s", want, report)
		}
	}

	// 计量分布：chat 2 次/350 tokens（77.8%），query 1 次/100 tokens（22.2%）。
	for _, want := range []string{
		"| chat | 2 | 66.7% | 240 | 110 | 350 | 77.8% |",
		"| query | 1 | 33.3% | 60 | 40 | 100 | 22.2% |",
		"| mock | 3 | 100.0% | 450 | 100.0% |",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("报告应包含计量分布行 %q\n---\n%s", want, report)
		}
	}

	// 每轮平均输入 token：chat 120.0、query 60.0、全量 100.0；
	// 含 span attrs 数据缺口标注。
	for _, want := range []string{
		"| chat | 2 | 240 | 120.0 |",
		"| **全量** | 3 | 300 | 100.0 |",
		"数据缺口", "不含输入 token",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("报告应包含输入 token 项 %q\n---\n%s", want, report)
		}
	}

	// 耗时三分解：模型 250ms（89.3%）、工具 30ms（10.7%）、其他 0。
	for _, want := range []string{
		"| 模型 | 3 | 250 | 89.3% |",
		"| 工具 | 1 | 30 | 10.7% |",
		"| 其他 | 0 | 0 | 0.0% |",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("报告应包含耗时分解行 %q\n---\n%s", want, report)
		}
	}

	// 模型调用归因说明：如实列出当前 purpose 取值。
	if !strings.Contains(report, "`chat`、`query`") || !strings.Contains(report, "reasoning_effort") {
		t.Errorf("报告应包含模型调用说明与 purpose 取值\n---\n%s", report)
	}
}

// TestBuildReportEmptyDB 空库（已迁移、无记录）报告应渲染空形态而不报错。
func TestBuildReportEmptyDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "empty.db")
	st := storetest.OpenMigratedAt(t, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}), dbPath)
	_ = st.Close()

	report, err := buildReport(dbPath, time.Now().UTC())
	if err != nil {
		t.Fatalf("空库 buildReport 不应报错: %v", err)
	}
	for _, want := range []string{"| 链路数 | 0 |", "（无计量记录）", "（无链路跨度）", "purpose 无取值"} {
		if !strings.Contains(report, want) {
			t.Errorf("空库报告应包含 %q\n---\n%s", want, report)
		}
	}
}

// TestBuildReportMissingDB 库文件不存在时应给出可读错误而非创建空库。
func TestBuildReportMissingDB(t *testing.T) {
	_, err := buildReport(filepath.Join(t.TempDir(), "missing.db"), time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), "不可读") {
		t.Fatalf("缺失库应报'不可读'错误，实际: %v", err)
	}
}
