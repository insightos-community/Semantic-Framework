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

package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/agent/runtime"
	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/llm"
	"insightos.cn/semantic-framework/pkg/log"
)

// newChatTestRouter 装配测试路由：store（临时库）+ handler + 注入 user_id 的 chi 路由。
// runtime 传 nil 时构造一个仅用于 EvictSession 的最小 Service（删除路径依赖）。
func newChatTestRouter(t *testing.T) (*store.Store, http.Handler) {
	return newChatTestRouterWithHost(t, false)
}

// newChatTestRouterWithHost 允许测试显式控制服务端宿主执行硬开关。
func newChatTestRouterWithHost(t *testing.T, allowHost bool) (*store.Store, http.Handler) {
	return newChatTestRouterWithEvents(t, allowHost, nil)
}

// newChatTestRouterWithEvents 允许测试观察迁移期 /chat 写入口发布的 Project
// 资源事件；普通 Chat 测试继续不装配事件总线。
func newChatTestRouterWithEvents(t *testing.T, allowHost bool,
	events projectEventBus) (*store.Store, http.Handler) {
	t.Helper()
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
	st := storetest.OpenMigrated(t, logger)

	// REST 创建会话会同步固化 Leader 模型快照，因此测试装配最小 Profile
	// 和 LLM 注册表，避免用绕过生产约束的空 Runtime。
	profileRoot := t.TempDir()
	leaderDir := filepath.Join(profileRoot, "leader")
	if err := os.MkdirAll(leaderDir, 0o755); err != nil {
		t.Fatalf("创建 Leader Profile 目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(leaderDir, "role.yaml"),
		[]byte("name: leader\nmode: coordinator\n"), 0o644); err != nil {
		t.Fatalf("写入 Leader Profile 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(leaderDir, "AGENT.md"),
		[]byte("# Leader\n处理用户对话。"), 0o644); err != nil {
		t.Fatalf("写入 Leader 指令失败: %v", err)
	}
	llmRegistry, err := llm.Load(config.LLMConfig{Default: "mock",
		Providers: map[string]config.LLMProviderConfig{
			"mock": {Component: "mock", Model: "mock"},
		}})
	if err != nil {
		t.Fatalf("创建 LLM 注册表失败: %v", err)
	}
	rt := runtime.NewService(runtime.Deps{Store: st, Logger: logger,
		Profiles: profile.NewLoader(profileRoot), LLM: llmRegistry,
		AllowHostExecution: allowHost})
	var h *ChatHandler
	if events == nil {
		h = NewChatHandler(st, rt, logger)
	} else {
		h = NewChatHandler(st, rt, logger, events)
	}

	r := chi.NewRouter()
	r.Route("/api/v1/chat", func(r chi.Router) {
		r.Post("/sessions", h.HandleCreateSession)
		r.Get("/sessions", h.HandleListSessions)
		r.Get("/sessions/{id}/messages", h.HandleListMessages)
		r.Get("/sessions/{id}/host-execution", h.HandleGetHostExecution)
		r.Put("/sessions/{id}/host-execution", h.HandleUpdateHostExecution)
		r.Delete("/sessions/{id}", h.HandleDeleteSession)
		r.Post("/attachments", h.HandleUploadAttachment)
		r.Get("/attachments/{id}", h.HandleGetAttachment)
		r.Post("/artifacts/register", h.HandleRegisterWorkspaceArtifact)
		r.Get("/artifacts", h.HandleListArtifacts)
		r.Get("/artifacts/{id}", h.HandleGetAttachment)
		r.Delete("/artifacts/{id}", h.HandleDeleteArtifact)
	})
	// 模拟 auth 中间件：按 X-Test-User 头注入 user_id。
	return st, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := auth.ContextWithUserID(req.Context(), req.Header.Get("X-Test-User"))
		r.ServeHTTP(w, req.WithContext(ctx))
	})
}

type recordingProjectEventBus struct {
	payloads []any
}

func (b *recordingProjectEventBus) Publish(_ string, payload any) {
	b.payloads = append(b.payloads, payload)
}

// TestConversationCreateAndArchivePublishProjectEvents 验证迁移期 /chat 写入口
// 与 Project 公共接口一致：创建和归档都会发布可由 Studio 增量消费的资源
// Envelope，归档负载包含 archived_at，而不是要求其他连接等待刷新。
func TestConversationCreateAndArchivePublishProjectEvents(t *testing.T) {
	events := &recordingProjectEventBus{}
	_, router := newChatTestRouterWithEvents(t, false, events)

	create := httptest.NewRequest(http.MethodPost, "/api/v1/chat/sessions",
		strings.NewReader(`{"title":"事件测试"}`))
	create.Header.Set("X-Test-User", "usr-events")
	createdResponse := httptest.NewRecorder()
	router.ServeHTTP(createdResponse, create)
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("创建 Conversation 失败: %d %s",
			createdResponse.Code, createdResponse.Body.String())
	}
	var createdBody struct {
		Session sessionView `json:"session"`
	}
	if err := json.NewDecoder(createdResponse.Body).Decode(&createdBody); err != nil {
		t.Fatal(err)
	}
	if len(events.payloads) != 1 {
		t.Fatalf("创建后应有一个资源事件，实际: %d", len(events.payloads))
	}
	created, ok := events.payloads[0].(ws.Envelope)
	if !ok || created.Type != "conversation.created" ||
		created.ProjectID != createdBody.Session.ProjectID ||
		created.ResourceType != "conversation" ||
		created.ResourceID != createdBody.Session.ID ||
		created.Revision != createdBody.Session.Revision {
		t.Fatalf("创建事件不符: %#v", events.payloads[0])
	}

	archive := httptest.NewRequest(http.MethodDelete,
		"/api/v1/chat/sessions/"+createdBody.Session.ID, nil)
	archive.Header.Set("X-Test-User", "usr-events")
	archivedResponse := httptest.NewRecorder()
	router.ServeHTTP(archivedResponse, archive)
	if archivedResponse.Code != http.StatusNoContent {
		t.Fatalf("归档 Conversation 失败: %d %s",
			archivedResponse.Code, archivedResponse.Body.String())
	}
	if len(events.payloads) != 2 {
		t.Fatalf("归档后应新增一个资源事件，实际: %d", len(events.payloads))
	}
	archived, ok := events.payloads[1].(ws.Envelope)
	if !ok || archived.Type != "conversation.archived" ||
		archived.ResourceID != createdBody.Session.ID || archived.Revision != 2 {
		t.Fatalf("归档事件不符: %#v", events.payloads[1])
	}
	payload, ok := archived.Payload.(map[string]any)
	view, viewOK := payload["conversation"].(sessionView)
	if !ok || !viewOK || view.ArchivedAt == nil {
		t.Fatalf("归档事件必须包含 archived_at: %#v", archived.Payload)
	}
}

// TestWorkspaceArtifactRegistrationAndDeletion 验证普通工作区文件只有显式登记
// 后才获得 ArtifactRef，已被消息引用时默认拒绝删除，force 后历史引用保留。
func TestWorkspaceArtifactRegistrationAndDeletion(t *testing.T) {
	st, router := newChatTestRouter(t)
	project, err := st.EnsureDefaultProject("usr-artifact")
	if err != nil {
		t.Fatalf("创建 Default Project 失败: %v", err)
	}
	reportPath := filepath.Join(project.WorkspaceRoot, "reports", "result.json")
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o700); err != nil {
		t.Fatalf("创建报告目录失败: %v", err)
	}
	if err := os.WriteFile(reportPath, []byte(`{"rows":3}`), 0o600); err != nil {
		t.Fatalf("写入工作区报告失败: %v", err)
	}

	body := strings.NewReader(`{"project_id":"` + project.ID +
		`","path":"reports/result.json","summary":"数据报告"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/artifacts/register", body)
	req.Header.Set("X-Test-User", "usr-artifact")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("登记 Artifact 应返回 201，实际: %d body=%s", rec.Code, rec.Body.String())
	}
	var registered struct {
		Artifact artifactView `json:"artifact"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &registered); err != nil || registered.Artifact.ID == "" {
		t.Fatalf("登记响应无有效 ArtifactRef: body=%s err=%v", rec.Body.String(), err)
	}
	artifact, content, err := st.GetArtifact(registered.Artifact.ID)
	if err != nil || artifact.OwnerID != "usr-artifact" || string(content) != `{"rows":3}` {
		t.Fatalf("Artifact 内容或归属不一致: artifact=%+v content=%q err=%v", artifact, content, err)
	}

	now := time.Now().UTC()
	if err := st.CreateChatSession(store.ChatSession{ID: "cs-artifact-ref", UserID: "usr-artifact",
		ProjectID: project.ID, Title: "引用测试", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("创建引用会话失败: %v", err)
	}
	if err := st.AppendChatMessage(store.ChatMessage{ID: store.NewChatMessageID(),
		SessionID: "cs-artifact-ref", Message: schema.UserMessage("查看报告"),
		ArtifactRefs: []string{artifact.ID}, CreatedAt: now}); err != nil {
		t.Fatalf("写入 Artifact 引用失败: %v", err)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete,
		"/api/v1/chat/artifacts/"+artifact.ID, nil)
	deleteReq.Header.Set("X-Test-User", "usr-artifact")
	deleteRec := httptest.NewRecorder()
	router.ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusConflict || !strings.Contains(deleteRec.Body.String(), "ARTIFACT_REFERENCED") {
		t.Fatalf("被引用 Artifact 默认应拒绝删除: status=%d body=%s", deleteRec.Code, deleteRec.Body.String())
	}

	forceReq := httptest.NewRequest(http.MethodDelete,
		"/api/v1/chat/artifacts/"+artifact.ID+"?force=true", nil)
	forceReq.Header.Set("X-Test-User", "usr-artifact")
	forceRec := httptest.NewRecorder()
	router.ServeHTTP(forceRec, forceReq)
	if forceRec.Code != http.StatusNoContent {
		t.Fatalf("强制删除应返回 204，实际: %d body=%s", forceRec.Code, forceRec.Body.String())
	}
	messages, err := st.ListChatMessages("cs-artifact-ref", 0, 0)
	if err != nil || len(messages) != 1 || len(messages[0].ArtifactRefs) != 1 {
		t.Fatalf("强制删除后历史引用应保留: messages=%+v err=%v", messages, err)
	}
}

// TestWorkspaceArtifactRejectsSymlinkEscape 验证登记接口拒绝 Project 内指向
// 外部文件的符号链接。
func TestWorkspaceArtifactRejectsSymlinkEscape(t *testing.T) {
	st, router := newChatTestRouter(t)
	project, err := st.EnsureDefaultProject("usr-artifact-link")
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(project.WorkspaceRoot, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/artifacts/register",
		strings.NewReader(`{"project_id":"`+project.ID+`","path":"escape.txt"}`))
	req.Header.Set("X-Test-User", "usr-artifact-link")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "符号链接") {
		t.Fatalf("符号链接逃逸应被拒绝: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestSessionHostExecutionPolicyAPI 验证宿主执行必须同时满足服务端总开关，
// 且会话策略能通过同一接口原子设置 mode 与 enabled。
func TestSessionHostExecutionPolicyAPI(t *testing.T) {
	for _, tc := range []struct {
		name       string
		allowHost  bool
		wantStatus int
	}{
		{name: "服务端关闭时拒绝", allowHost: false, wantStatus: http.StatusBadRequest},
		{name: "服务端开启时允许", allowHost: true, wantStatus: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, router := newChatTestRouterWithHost(t, tc.allowHost)
			now := time.Now().UTC()
			if err := st.CreateChatSession(store.ChatSession{ID: "cs-host", UserID: "usr-host",
				Title: "宿主执行", CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatalf("创建会话失败: %v", err)
			}
			req := httptest.NewRequest(http.MethodPut,
				"/api/v1/chat/sessions/cs-host/host-execution",
				strings.NewReader(`{"mode":"full","enabled":true}`))
			req.Header.Set("X-Test-User", "usr-host")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("状态码应为 %d，实际: %d body=%s", tc.wantStatus, rec.Code, rec.Body.String())
			}
			if !tc.allowHost {
				return
			}

			getReq := httptest.NewRequest(http.MethodGet,
				"/api/v1/chat/sessions/cs-host/host-execution", nil)
			getReq.Header.Set("X-Test-User", "usr-host")
			getRec := httptest.NewRecorder()
			router.ServeHTTP(getRec, getReq)
			if getRec.Code != http.StatusOK ||
				!strings.Contains(getRec.Body.String(), `"mode":"full"`) ||
				!strings.Contains(getRec.Body.String(), `"host_execution_enabled":true`) {
				t.Fatalf("策略查询结果不一致: status=%d body=%s", getRec.Code, getRec.Body.String())
			}
		})
	}
}

func TestChatImageAttachmentOwnership(t *testing.T) {
	_, router := newChatTestRouter(t)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "camera.png")
	if err != nil {
		t.Fatalf("创建 multipart 失败: %v", err)
	}
	png := []byte{'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n', 0, 0, 0, 0}
	if _, err := part.Write(png); err != nil {
		t.Fatalf("写入图片失败: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭 multipart 失败: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/attachments", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Test-User", "usr-1")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传应返回 201，实际: %d body=%s", rec.Code, rec.Body.String())
	}
	var uploaded struct {
		Attachment attachmentView `json:"attachment"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &uploaded); err != nil || uploaded.Attachment.ID == "" {
		t.Fatalf("上传响应不符: err=%v body=%s", err, rec.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, uploaded.Attachment.ContentURL, nil)
	get.Header.Set("X-Test-User", "usr-1")
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, get)
	if getRec.Code != http.StatusOK || !bytes.Equal(getRec.Body.Bytes(), png) ||
		getRec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("本人读取图片失败: status=%d type=%q", getRec.Code, getRec.Header().Get("Content-Type"))
	}

	other := httptest.NewRequest(http.MethodGet, uploaded.Attachment.ContentURL, nil)
	other.Header.Set("X-Test-User", "usr-2")
	otherRec := httptest.NewRecorder()
	router.ServeHTTP(otherRec, other)
	if otherRec.Code != http.StatusNotFound {
		t.Fatalf("其他用户读取应返回 404，实际: %d", otherRec.Code)
	}
}

// doJSON 发送 JSON 请求并解析响应，返回状态码与响应体。
func doJSON(t *testing.T, router http.Handler, method, path, user, body string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, path, reader)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("X-Test-User", user)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	var parsed map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
			t.Fatalf("响应不是合法 JSON: %v（body: %s）", err, rec.Body.String())
		}
	}
	return rec.Code, parsed
}

// createSession 创建一个会话语义并返回会话 ID。
func createSession(t *testing.T, router http.Handler, user, title string) string {
	t.Helper()
	body := `{}`
	if title != "" {
		body = `{"title":"` + title + `"}`
	}
	code, resp := doJSON(t, router, http.MethodPost, "/api/v1/chat/sessions", user, body)
	if code != http.StatusCreated {
		t.Fatalf("创建会话应返回 201，实际: %d（%v）", code, resp)
	}
	sess, ok := resp["session"].(map[string]any)
	if !ok {
		t.Fatalf("响应应含 session，实际: %v", resp)
	}
	id, _ := sess["id"].(string)
	if id == "" {
		t.Fatalf("会话 ID 不应为空: %v", sess)
	}
	return id
}

// TestChatRESTFlow 验证 REST 全链路：创建（默认/指定标题）→ 列表（归属过滤）→
// 消息分页 → 删除（级联 + 他人不可见）。
func TestChatRESTFlow(t *testing.T) {
	st, router := newChatTestRouter(t)

	// 创建：默认标题与指定标题。
	cs1 := createSession(t, router, "usr-1", "")
	cs2 := createSession(t, router, "usr-1", "取快递任务")
	createSession(t, router, "usr-2", "别人的会话")

	sess, err := st.GetChatSession(cs1)
	if err != nil || sess.Title != defaultSessionTitle {
		t.Errorf("默认标题应为 %q，实际: %+v", defaultSessionTitle, sess)
	}

	// 列表：usr-1 只看到本人 2 个会话。
	code, resp := doJSON(t, router, http.MethodGet, "/api/v1/chat/sessions", "usr-1", "")
	if code != http.StatusOK {
		t.Fatalf("列表应返回 200，实际: %d", code)
	}
	sessions, ok := resp["sessions"].([]any)
	if !ok || len(sessions) != 2 {
		t.Fatalf("usr-1 应有 2 个会话，实际: %v", resp["sessions"])
	}

	// 写入消息后分页查询：追加 3 条，page_size=2 查第 2 页应剩 1 条且 total=3。
	for _, c := range []string{"一", "二", "三"} {
		if err := st.AppendChatMessage(store.ChatMessage{
			ID: store.NewChatMessageID(), SessionID: cs2, Message: schema.UserMessage(c),
		}); err != nil {
			t.Fatalf("AppendChatMessage 失败: %v", err)
		}
	}
	code, resp = doJSON(t, router, http.MethodGet, "/api/v1/chat/sessions/"+cs2+"/messages?page=2&page_size=2", "usr-1", "")
	if code != http.StatusOK {
		t.Fatalf("消息查询应返回 200，实际: %d", code)
	}
	messages, ok := resp["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("第 2 页应有 1 条消息，实际: %v", resp["messages"])
	}
	if total, _ := resp["total"].(float64); total != 3 {
		t.Errorf("total 应为 3，实际: %v", resp["total"])
	}

	// 归属过滤：usr-2 查 usr-1 的会话消息返回 404。
	code, _ = doJSON(t, router, http.MethodGet, "/api/v1/chat/sessions/"+cs2+"/messages", "usr-2", "")
	if code != http.StatusNotFound {
		t.Errorf("他人会话应返回 404，实际: %d", code)
	}

	// 删除入口改为归档：从会话列表隐藏，但历史消息仍可只读复盘。
	code, _ = doJSON(t, router, http.MethodDelete, "/api/v1/chat/sessions/"+cs2, "usr-1", "")
	if code != http.StatusNoContent {
		t.Fatalf("归档应返回 204，实际: %d", code)
	}
	archived, err := st.GetChatSession(cs2)
	if err != nil || archived.ArchivedAt == nil {
		t.Fatalf("会话应标记为已归档，实际: %+v, %v", archived, err)
	}
	code, _ = doJSON(t, router, http.MethodGet, "/api/v1/chat/sessions/"+cs2+"/messages", "usr-1", "")
	if code != http.StatusOK {
		t.Errorf("归档后历史消息应可只读查询，实际: %d", code)
	}
	msgs, err := st.ListChatMessages(cs2, 0, 0)
	if err != nil || len(msgs) != 3 {
		t.Errorf("归档后消息必须保留，实际: %v, %d", err, len(msgs))
	}
	// 他人删除我的会话：404（不暴露存在性），会话仍在。
	code, _ = doJSON(t, router, http.MethodDelete, "/api/v1/chat/sessions/"+cs1, "usr-2", "")
	if code != http.StatusNotFound {
		t.Errorf("他人删除应返回 404，实际: %d", code)
	}
	if _, err := st.GetChatSession(cs1); err != nil {
		t.Errorf("会话不应被他人删除: %v", err)
	}
}

// TestChatRESTBadRequest 验证非法请求体返回 400 统一错误格式。
func TestChatRESTBadRequest(t *testing.T) {
	_, router := newChatTestRouter(t)
	code, resp := doJSON(t, router, http.MethodPost, "/api/v1/chat/sessions", "usr-1", `{invalid`)
	if code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 应返回 400，实际: %d", code)
	}
	errObj, ok := resp["error"].(map[string]any)
	if !ok || errObj["code"] == "" {
		t.Errorf("400 响应应为统一错误格式，实际: %v", resp)
	}
}
