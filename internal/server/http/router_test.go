package http

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"insightos.cn/semantic-framework/internal/agent/runtime"
	robotdomain "insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/server/http/handlers"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/llm"
	"insightos.cn/semantic-framework/pkg/log"
)

// discardLogger 返回静默日志器。
func discardLogger() *log.Logger {
	return log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
}

// TestWriteError 验证统一错误响应的格式与状态码。
func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusTeapot, "SOME_CODE", "出了点问题")

	if rec.Code != http.StatusTeapot {
		t.Errorf("状态码应为 418，实际: %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type 应为 application/json，实际: %q", ct)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体应为 JSON，实际: %s", rec.Body.String())
	}
	if body.Error.Code != "SOME_CODE" || body.Error.Message != "出了点问题" {
		t.Errorf("错误体内容不符: %+v", body.Error)
	}
}

// TestRequestID 验证请求 ID 的生成与透传。
func TestRequestID(t *testing.T) {
	// 下游 handler 回显 context 中的 trace_id。
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(log.TraceIDFromContext(r.Context())))
	})
	handler := RequestID()(echo)

	// 未携带时生成 32 字符十六进制 ID，并写入响应头。
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	id := rec.Header().Get(requestIDHeader)
	if len(id) != 32 {
		t.Errorf("生成的请求 ID 应为 32 字符，实际: %q", id)
	}
	if rec.Body.String() != id {
		t.Errorf("context 中的 trace_id 应与响应头一致: %q vs %q", rec.Body.String(), id)
	}

	// 已携带时透传原值。
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(requestIDHeader, "client-trace-1")
	handler.ServeHTTP(rec, req)
	if rec.Header().Get(requestIDHeader) != "client-trace-1" {
		t.Errorf("应透传客户端请求 ID，实际: %q", rec.Header().Get(requestIDHeader))
	}
	if rec.Body.String() != "client-trace-1" {
		t.Errorf("context 应注入透传的 trace_id，实际: %q", rec.Body.String())
	}
}

// TestRecovery 验证 panic 被恢复为 500 统一错误，且日志含 trace_id。
func TestRecovery(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(log.Options{Level: log.LevelDebug, Writer: &buf})

	panicHandler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})
	handler := RequestID()(Recovery(logger)(panicHandler))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(requestIDHeader, "trace-panic-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("panic 应恢复为 500，实际: %d", rec.Code)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "INTERNAL_ERROR" {
		t.Errorf("响应应为 INTERNAL_ERROR 统一格式，实际: %s", rec.Body.String())
	}
	logOut := buf.String()
	if !strings.Contains(logOut, "trace-panic-1") || !strings.Contains(logOut, "boom") {
		t.Errorf("panic 日志应含 trace_id 与 panic 内容，实际: %s", logOut)
	}
}

// TestLogging 验证访问日志包含 method/path/status/耗时/trace_id。
func TestLogging(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(log.Options{Level: log.LevelInfo, Writer: &buf})

	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	handler := RequestID()(Logging(logger)(ok))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/demo", nil)
	req.Header.Set(requestIDHeader, "trace-log-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码应透传为 201，实际: %d", rec.Code)
	}
	logOut := buf.String()
	for _, want := range []string{"trace-log-1", "GET", "/api/v1/demo", "201", "duration"} {
		if !strings.Contains(logOut, want) {
			t.Errorf("访问日志应包含 %q，实际: %s", want, logOut)
		}
	}
}

// newTestRouter 装配一个带真实 auth 服务（临时库）的路由。
func newTestRouter(t *testing.T) http.Handler {
	t.Helper()
	st := storetest.OpenMigrated(t, discardLogger())

	svc := auth.NewService(st, discardLogger())
	t.Setenv("SEMANTIC_ADMIN_PASSWORD", "s3cret")
	if err := svc.SeedAdmin(); err != nil {
		t.Fatalf("SeedAdmin 失败: %v", err)
	}

	// 对话域 handler 依赖 runtime.Service（此处只验证路由装配，不触发 run）。
	rt := runtime.NewService(runtime.Deps{Store: st, Logger: discardLogger()})
	// 设置域 handler 依赖注册表与 patcher（同样只做装配，不触发请求）。
	registry, err := llm.Load(config.LLMConfig{
		Default:   "mock",
		Providers: map[string]config.LLMProviderConfig{"mock": {Component: "mock", Model: "mock"}},
	})
	if err != nil {
		t.Fatalf("llm.Load 失败: %v", err)
	}
	settingsH := handlers.NewSettingsHandler(st, registry, &stubPatcher{}, discardLogger())
	agentsH := handlers.NewAgentsHandler(rt)
	skillsH := handlers.NewSkillsHandler(rt.SkillStore)
	toolsH := handlers.NewToolsHandler(tool.NewRegistry(), nil)
	return NewRouter(discardLogger(), svc, handlers.NewChatHandler(st, rt, discardLogger()),
		handlers.NewProjectsHandler(st, rt, discardLogger()), settingsH, agentsH,
		skillsH, toolsH, handlers.NewTracesHandler(st, discardLogger()),
		handlers.NewMeteringHandler(st, discardLogger()),
		handlers.NewInteractionsHandler(st, discardLogger()),
		handlers.NewRobotsHandler(robotdomain.NewService(st, nil), st), nil)
}

// stubPatcher 是路由装配测试用的最小 ConfigPatcher 实现（不会被请求触发）。
type stubPatcher struct{}

// ConfigPath 满足路由装配所需接口；该测试不触发实际配置写回。
func (s *stubPatcher) ConfigPath() string { return "/tmp/test-semantic-server.yaml" }

// CurrentTree 返回空树与空哈希。
func (s *stubPatcher) CurrentTree() (map[string]any, string, error) {
	return map[string]any{}, "", nil
}

// Patch 返回未实现拒绝（装配测试不触发）。
func (s *stubPatcher) Patch(string, map[string]any) (map[string]any, string, []string, error) {
	return nil, "", nil, &handlers.PatchReject{Status: http.StatusServiceUnavailable, Code: "SETTINGS_UNAVAILABLE", Message: "stub"}
}

// TestRouterAuthFlow 验证路由层鉴权链路：公开端点免 token，
// 受保护端点无 token 401、带合法 token 200 且回显 user_id。
func TestRouterAuthFlow(t *testing.T) {
	router := newTestRouter(t)

	// 公开端点：healthz/version 无 token 可访问。
	for _, path := range []string{"/api/v1/system/healthz", "/api/v1/system/version"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("公开端点 %s 应返回 200，实际: %d", path, rec.Code)
		}
	}

	// 登录获取 token。
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"username":"admin","password":"s3cret"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, loginReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("登录应返回 200，实际: %d（body: %s）", rec.Code, rec.Body.String())
	}
	var loginBody struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &loginBody); err != nil || loginBody.Token == "" {
		t.Fatalf("登录响应应含 token，实际: %s", rec.Body.String())
	}

	// 安全确认的 Handler 单测不能代替正式路由装配；曾因 action 正则遗漏，
	// 前端“确认现场安全”始终落到普通 404，导致失败 Workflow 无法终结。
	confirmReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/missing/workflows/missing/confirm-stop", strings.NewReader(`{}`))
	confirmReq.Header.Set("Authorization", "Bearer "+loginBody.Token)
	confirmRec := httptest.NewRecorder()
	router.ServeHTTP(confirmRec, confirmReq)
	if !json.Valid(confirmRec.Body.Bytes()) || confirmRec.Code != http.StatusNotFound {
		t.Fatalf("confirm-stop 应进入 Handler 的项目校验，实际: %d %s",
			confirmRec.Code, confirmRec.Body.String())
	}

	// 受保护端点：无 token 401 + 统一错误格式。
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/ping", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 token 访问 ping 应返回 401，实际: %d", rec.Code)
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil || errBody.Error.Code == "" {
		t.Errorf("401 响应应为统一错误格式，实际: %s", rec.Body.String())
	}

	// 带合法 token：200 + pong + user_id。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/system/ping", nil)
	req.Header.Set("Authorization", "Bearer "+loginBody.Token)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("带 token 访问 ping 应返回 200，实际: %d", rec.Code)
	}
	var pingBody struct {
		Pong   bool   `json:"pong"`
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pingBody); err != nil {
		t.Fatalf("ping 响应解析失败: %v", err)
	}
	if !pingBody.Pong || pingBody.UserID == "" {
		t.Errorf("ping 响应应为 pong=true 且含 user_id，实际: %+v", pingBody)
	}

	// Pilot transfer 只接受一次加入后签发的专用 credential。先经真实
	// enrollment API 获取凭据，确保管理员登录 token 不再被设备通道复用。
	req = httptest.NewRequest(http.MethodPost, "/api/v1/pilot-enrollments", nil)
	req.Header.Set("Authorization", "Bearer "+loginBody.Token)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建 Pilot enrollment 失败: %d %s", rec.Code, rec.Body.String())
	}
	var enrollment struct {
		Enrollment store.PilotEnrollment `json:"enrollment"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &enrollment); err != nil {
		t.Fatal(err)
	}
	claimBody, _ := json.Marshal(map[string]string{"join_code": enrollment.Enrollment.Code, "pilot_id": "pilot-router-test"})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/pilot-enrollments/claim", bytes.NewReader(claimBody))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var claim struct {
		Credential string `json:"credential"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &claim) != nil || claim.Credential == "" {
		t.Fatalf("claim Pilot enrollment 失败: %d %s", rec.Code, rec.Body.String())
	}

	// Pilot 的包和 Artifact 下载必须走 HTTP 网关，而不是误挂到独立的
	// WebSocket listener。不存在的 token 由 Robot Service 返回明确错误，
	// 同时证明请求已经命中传输 Handler。
	req = httptest.NewRequest(http.MethodGet, "/pilot/v1/transfers/missing", nil)
	req.Header.Set("Authorization", "Bearer "+claim.Credential)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "传输不存在或已经失效") {
		t.Fatalf("Pilot HTTP 传输入口未正确装配：code=%d body=%q", rec.Code, rec.Body.String())
	}
}
