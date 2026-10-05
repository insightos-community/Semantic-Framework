package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/log"
)

// newTracesTestRouter 装配链路追踪端点的测试路由（真实 store）。
func newTracesTestRouter(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st := storetest.OpenMigrated(t, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))

	h := NewTracesHandler(st, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	r := chi.NewRouter()
	r.Get("/api/v1/traces", h.HandleListTraces)
	r.Get("/api/v1/traces/{trace_id}/spans", h.HandleGetSpans)
	r.Get("/api/v1/traces/{trace_id}/spans/{span_id}/io", h.HandleGetSpanIO)
	return r, st
}

// seedSpans 写入两条链路：tr-1（2 跨度）与 tr-2（1 跨度，开始更晚）。
func seedSpans(t *testing.T, st *store.Store) time.Time {
	t.Helper()
	base := time.Now().UTC().Truncate(time.Second)
	for _, sp := range []store.Span{
		{TraceID: "tr-1", Name: "Mock", Kind: "ChatModel", StartedAt: base, DurationMs: 12, Attrs: `{"model":"mock"}`},
		{TraceID: "tr-1", Name: "get_weather", Kind: "Tool", StartedAt: base.Add(time.Second), DurationMs: 3, Attrs: `{}`},
		{TraceID: "tr-2", Name: "OpenAI", Kind: "ChatModel", StartedAt: base.Add(2 * time.Second), DurationMs: 100, Attrs: `{}`},
	} {
		if _, err := st.InsertSpan(sp); err != nil {
			t.Fatalf("InsertSpan 失败: %v", err)
		}
	}
	return base
}

// getJSON 发起 GET 并解析响应体。
func getJSON(t *testing.T, router http.Handler, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body: %s）", err, rec.Body.String())
	}
	return rec.Code, body
}

// TestListTracesEndpoint 验证 trace 列表端点的倒序分页、聚合字段和过滤。
func TestListTracesEndpoint(t *testing.T) {
	router, st := newTracesTestRouter(t)
	seedSpans(t, st)

	// 无过滤：tr-2 在前（开始更晚），total=2。
	code, body := getJSON(t, router, "/api/v1/traces")
	if code != http.StatusOK {
		t.Fatalf("应返回 200，实际: %d（%v）", code, body)
	}
	if body["total"].(float64) != 2 || body["page"].(float64) != 1 {
		t.Errorf("total/page 不符: %v", body)
	}
	traces := body["traces"].([]any)
	if len(traces) != 2 {
		t.Fatalf("应返回 2 条链路，实际: %v", body)
	}
	first := traces[0].(map[string]any)
	if first["trace_id"] != "tr-2" || first["span_count"].(float64) != 1 ||
		first["name"] != "OpenAI" || first["duration_ms"].(float64) != 100 {
		t.Errorf("首条链路不符: %v", first)
	}
	second := traces[1].(map[string]any)
	if second["trace_id"] != "tr-1" || second["span_count"].(float64) != 2 ||
		second["name"] != "Mock" || second["kind"] != "ChatModel" ||
		second["duration_ms"].(float64) != 15 {
		t.Errorf("次条链路聚合不符: %v", second)
	}
	if second["started_at"] == nil || second["started_at"] == "" {
		t.Errorf("started_at 不应为空: %v", second)
	}

	// trace_id 精确过滤。
	_, body = getJSON(t, router, "/api/v1/traces?trace_id=tr-1")
	if body["total"].(float64) != 1 ||
		body["traces"].([]any)[0].(map[string]any)["trace_id"] != "tr-1" {
		t.Errorf("trace_id 过滤不符: %v", body)
	}

	// 分页：page_size=1 第 2 页是 tr-1；page_size 超上限收敛到 100。
	_, body = getJSON(t, router, "/api/v1/traces?page=2&page_size=1")
	if body["total"].(float64) != 2 || len(body["traces"].([]any)) != 1 ||
		body["traces"].([]any)[0].(map[string]any)["trace_id"] != "tr-1" {
		t.Errorf("分页结果不符: %v", body)
	}
	_, body = getJSON(t, router, "/api/v1/traces?page_size=9999")
	if body["page_size"].(float64) != 100 {
		t.Errorf("page_size 应收敛到 100，实际: %v", body["page_size"])
	}
}

// TestGetSpansEndpoint 验证 span 明细端点：按开始时间升序、attrs 为 JSON 对象。
func TestGetSpansEndpoint(t *testing.T) {
	router, st := newTracesTestRouter(t)
	seedSpans(t, st)

	code, body := getJSON(t, router, "/api/v1/traces/tr-1/spans")
	if code != http.StatusOK {
		t.Fatalf("应返回 200，实际: %d（%v）", code, body)
	}
	spans := body["spans"].([]any)
	if len(spans) != 2 {
		t.Fatalf("tr-1 应有 2 条跨度，实际: %v", body)
	}
	first := spans[0].(map[string]any)
	if first["name"] != "Mock" || first["kind"] != "ChatModel" || first["duration_ms"].(float64) != 12 {
		t.Errorf("首条跨度不符: %v", first)
	}
	if _, hasParent := first["parent_id"]; !hasParent {
		t.Errorf("跨度应含 parent_id 字段: %v", first)
	}
	attrs, ok := first["attrs"].(map[string]any)
	if !ok || attrs["model"] != "mock" {
		t.Errorf("attrs 应为 JSON 对象，实际: %v", first["attrs"])
	}

	// 不存在的链路：空列表。
	code, body = getJSON(t, router, "/api/v1/traces/tr-x/spans")
	if code != http.StatusOK || len(body["spans"].([]any)) != 0 {
		t.Errorf("不存在的链路应返回空列表，实际: %d %v", code, body)
	}
}

func TestGetSpanIOEndpoint(t *testing.T) {
	router, st := newTracesTestRouter(t)
	base := seedSpans(t, st)
	_ = base
	spans, err := st.GetSpans("tr-1")
	if err != nil || len(spans) == 0 {
		t.Fatalf("准备跨度失败: %v %v", err, spans)
	}
	modelSpan := spans[0]
	if err := st.UpsertSpanIO(store.PrepareSpanIO(modelSpan.ID, "user: hello", "assistant: hi")); err != nil {
		t.Fatalf("写入输入输出失败: %v", err)
	}

	code, body := getJSON(t, router, "/api/v1/traces/tr-1/spans")
	if code != http.StatusOK {
		t.Fatalf("spans 应返回 200，实际: %d %v", code, body)
	}
	first := body["spans"].([]any)[0].(map[string]any)
	if first["has_io"] != true {
		t.Fatalf("有正文的跨度应 has_io=true，实际: %v", first)
	}
	if _, hasInput := first["input"]; hasInput {
		t.Fatalf("列表不得携带 input 正文: %v", first)
	}

	code, body = getJSON(t, router, "/api/v1/traces/tr-1/spans/"+strconv.FormatInt(modelSpan.ID, 10)+"/io")
	if code != http.StatusOK {
		t.Fatalf("io 应返回 200，实际: %d %v", code, body)
	}
	if body["input"] != "user: hello" || body["output"] != "assistant: hi" {
		t.Fatalf("io 正文不符: %v", body)
	}

	code, body = getJSON(t, router, "/api/v1/traces/tr-2/spans/"+strconv.FormatInt(modelSpan.ID, 10)+"/io")
	if code != http.StatusNotFound {
		t.Fatalf("跨链路读取应 404，实际: %d %v", code, body)
	}
}
