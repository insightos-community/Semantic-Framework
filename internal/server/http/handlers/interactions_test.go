package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/log"
)

// newInteractionsTestRouter 装配交互记录端点的测试路由（真实 store）。
func newInteractionsTestRouter(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st := storetest.OpenMigrated(t, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))

	h := NewInteractionsHandler(st, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			ctx := auth.ContextWithUserID(request.Context(), request.Header.Get("X-Test-User"))
			next.ServeHTTP(w, request.WithContext(ctx))
		})
	})
	r.Get("/api/v1/interactions", h.HandleListInteractions)
	return r, st
}

// TestListInteractionsEndpoint 验证交互列表端点：状态/会话过滤、倒序分页、
// payload/reply 的 JSON 形态与非法 status 的 400。
func TestListInteractionsEndpoint(t *testing.T) {
	router, st := newInteractionsTestRouter(t)
	base := time.Now().UTC().Truncate(time.Second)

	it1 := store.Interaction{
		ID: store.NewInteractionID(), SessionID: "cs-1", Agent: "leader",
		Type: store.InteractionTypeConfirm, Status: store.InteractionStatusPending,
		Payload: `{"question":"是否批准写文件？","risk":"high"}`, RunID: "run-1",
		CheckpointID: "run-1", CreatedAt: base,
	}
	it2 := store.Interaction{
		ID: store.NewInteractionID(), SessionID: "cs-2", Agent: "leader",
		Type: store.InteractionTypeConfirm, Status: store.InteractionStatusPending,
		Payload: `{"question":"继续吗？"}`, RunID: "run-2",
		CheckpointID: "run-2", CreatedAt: base.Add(time.Second),
	}
	for _, it := range []store.Interaction{it1, it2} {
		if err := st.CreateInteraction(it); err != nil {
			t.Fatalf("CreateInteraction 失败: %v", err)
		}
	}
	if err := st.AnswerInteraction(it1.ID, `{"approved":true}`, base.Add(2*time.Second)); err != nil {
		t.Fatalf("AnswerInteraction 失败: %v", err)
	}

	// status=pending（审批卡恢复）：只剩 it2，reply 为 null，payload 为对象。
	code, body := getJSON(t, router, "/api/v1/interactions?status=pending")
	if code != http.StatusOK {
		t.Fatalf("应返回 200，实际: %d（%v）", code, body)
	}
	if body["total"].(float64) != 1 {
		t.Fatalf("pending 应有 1 条，实际: %v", body)
	}
	got := body["interactions"].([]any)[0].(map[string]any)
	if got["id"] != it2.ID || got["status"] != "pending" || got["session_id"] != "cs-2" ||
		got["agent"] != "leader" || got["type"] != "confirm" || got["run_id"] != "run-2" {
		t.Errorf("pending 条目不符: %v", got)
	}
	if payload, ok := got["payload"].(map[string]any); !ok || payload["question"] != "继续吗？" {
		t.Errorf("payload 应为 JSON 对象，实际: %v", got["payload"])
	}
	if got["reply"] != nil {
		t.Errorf("未应答的 reply 应为 null，实际: %v", got["reply"])
	}
	if got["answered_at"] != nil {
		t.Errorf("未应答的 answered_at 应为 null，实际: %v", got["answered_at"])
	}

	// status=answered：it1，reply 为对象，answered_at 非空。
	_, body = getJSON(t, router, "/api/v1/interactions?status=answered")
	got = body["interactions"].([]any)[0].(map[string]any)
	if got["id"] != it1.ID || got["answered_at"] == nil {
		t.Errorf("answered 条目不符: %v", got)
	}
	if reply, ok := got["reply"].(map[string]any); !ok || reply["approved"] != true {
		t.Errorf("reply 应为 JSON 对象，实际: %v", got["reply"])
	}

	// session_id 过滤 + 无过滤倒序（it2 在前）。
	_, body = getJSON(t, router, "/api/v1/interactions?session_id=cs-1")
	if body["total"].(float64) != 1 ||
		body["interactions"].([]any)[0].(map[string]any)["id"] != it1.ID {
		t.Errorf("session 过滤不符: %v", body)
	}
	_, body = getJSON(t, router, "/api/v1/interactions")
	items := body["interactions"].([]any)
	if body["total"].(float64) != 2 || len(items) != 2 ||
		items[0].(map[string]any)["id"] != it2.ID || items[1].(map[string]any)["id"] != it1.ID {
		t.Errorf("无过滤应按创建时间倒序，实际: %v", body)
	}

	// 分页：page_size=1 第 2 页是 it1。
	_, body = getJSON(t, router, "/api/v1/interactions?page=2&page_size=1")
	items = body["interactions"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != it1.ID {
		t.Errorf("分页结果不符: %v", body)
	}

	// 非法 status：400 BAD_REQUEST。
	code, body = getJSON(t, router, "/api/v1/interactions?status=bogus")
	if code != http.StatusBadRequest ||
		body["error"].(map[string]any)["code"] != CodeBadRequest {
		t.Errorf("非法 status 应返回 400，实际: %d %v", code, body)
	}
}

// TestListInteractionsEndpointIsolatesOwners 验证未指定 project_id 的全局列表
// 仍只返回当前用户 Project 下的记录，不能把其他用户的 Interaction 泄漏出来。
func TestListInteractionsEndpointIsolatesOwners(t *testing.T) {
	router, st := newInteractionsTestRouter(t)
	ownerProject, err := st.CreateProject("usr-owner-a", "owner A")
	if err != nil {
		t.Fatalf("CreateProject(owner A) 失败: %v", err)
	}
	otherProject, err := st.CreateProject("usr-owner-b", "owner B")
	if err != nil {
		t.Fatalf("CreateProject(owner B) 失败: %v", err)
	}
	now := time.Now().UTC()
	for _, fixture := range []struct {
		session     store.ChatSession
		interaction store.Interaction
	}{
		{
			session:     store.ChatSession{ID: "cs-owner-a", UserID: "usr-owner-a", ProjectID: ownerProject.ID, Title: "owner A", CreatedAt: now, UpdatedAt: now},
			interaction: store.Interaction{ID: store.NewInteractionID(), ProjectID: ownerProject.ID, SessionID: "cs-owner-a", Agent: "leader", Type: store.InteractionTypeConfirm, Status: store.InteractionStatusPending, Payload: `{}`, CreatedAt: now},
		},
		{
			session:     store.ChatSession{ID: "cs-owner-b", UserID: "usr-owner-b", ProjectID: otherProject.ID, Title: "owner B", CreatedAt: now, UpdatedAt: now},
			interaction: store.Interaction{ID: store.NewInteractionID(), ProjectID: otherProject.ID, SessionID: "cs-owner-b", Agent: "leader", Type: store.InteractionTypeConfirm, Status: store.InteractionStatusPending, Payload: `{}`, CreatedAt: now.Add(time.Second)},
		},
	} {
		if _, err := st.ActivateProject(fixture.session.UserID, fixture.session.ProjectID); err != nil {
			t.Fatalf("ActivateProject(%s) 失败: %v", fixture.session.UserID, err)
		}
		if err := st.CreateChatSession(fixture.session); err != nil {
			t.Fatalf("CreateChatSession 失败: %v", err)
		}
		if err := st.CreateInteraction(fixture.interaction); err != nil {
			t.Fatalf("CreateInteraction 失败: %v", err)
		}
		if fixture.session.UserID == "usr-owner-a" {
			if err := st.AnswerInteraction(fixture.interaction.ID, `{"approved":true}`, now); err != nil {
				t.Fatalf("AnswerInteraction(owner A) 失败: %v", err)
			}
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/interactions", nil)
	request.Header.Set("X-Test-User", "usr-owner-a")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际: %d %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	items := body["interactions"].([]any)
	if body["total"].(float64) != 1 || len(items) != 1 ||
		items[0].(map[string]any)["project_id"] != ownerProject.ID {
		t.Fatalf("全局列表必须按 owner 隔离，实际: %v", body)
	}
}
