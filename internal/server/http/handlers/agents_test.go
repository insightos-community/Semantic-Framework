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
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/agent/runtime"
	"insightos.cn/semantic-framework/internal/agent/team"
	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/llm"
	"insightos.cn/semantic-framework/pkg/log"
)

// newAgentsTestRouter 装配 Agent 目录测试路由：真实 runtime + Leader/Query Team。
func newAgentsTestRouter(t *testing.T) (http.Handler, *runtime.Service, *store.Store) {
	t.Helper()
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
	st := storetest.OpenMigrated(t, logger)

	// Leader 与请求式 Query profile 夹具。
	root := t.TempDir()
	roles := map[string]map[string]string{
		"leader": {
			"role.yaml": "name: leader\nmode: coordinator\nmodel: mock\n",
			"AGENT.md":  "# Role\n你是 Leader。",
		},
		"query": {
			"role.yaml": "name: query\nmode: service\nmodel: mock\n" +
				"subagent: {enabled: true, tool_name: ask_query, tool_description: 系统状态与产物查询助手}\n",
			"AGENT.md": "# Role\n你是查询助手。",
		},
		"developer": {
			"role.yaml": "name: developer\nmode: worker\nmodel: mock\n",
			"AGENT.md":  "# Role\n你是 Developer Task Agent。",
		},
	}
	for role, files := range roles {
		dir := filepath.Join(root, role)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("创建角色目录失败: %v", err)
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatalf("写入 %s/%s 失败: %v", role, name, err)
			}
		}
	}

	llmReg, err := llm.Load(config.LLMConfig{
		Default: "mock",
		Providers: map[string]config.LLMProviderConfig{
			"mock":    {Component: "mock", Model: "mock"},
			"mock-d3": {Component: "mock", Model: "mock-d3"},
		},
	})
	if err != nil {
		t.Fatalf("构建 LLM 注册表失败: %v", err)
	}

	rt := runtime.NewService(runtime.Deps{
		Profiles: profile.NewLoader(root),
		LLM:      llmReg,
		Store:    st,
		Bus:      event.NewBus(logger),
		Logger:   logger,
	})
	err = rt.AssembleTeam(context.Background(), &team.Def{
		Name:   "default",
		Leader: team.MemberDef{ID: "leader", Role: "leader"},
		Members: []team.MemberDef{{ID: "developer-1", Role: "developer"},
			{ID: "query-1", Role: "query"}},
	})
	if err != nil {
		t.Fatalf("AssembleTeam 失败: %v", err)
	}
	t.Cleanup(rt.Shutdown)

	h := NewAgentsHandler(rt)
	r := chi.NewRouter()
	r.Get("/api/v1/agents", h.HandleListAgents)
	r.Put("/api/v1/agents/{id}/models", h.HandleUpdateAgentModels)
	r.Get("/api/v1/chat/sessions/{id}/agents", h.HandleListSessionAgents)
	r.Get("/api/v1/chat/sessions/{id}/agents/{agent_id}/tools", h.HandleListSessionAgentTools)
	r.Put("/api/v1/chat/sessions/{id}/agents/{agent_id}/model", h.HandleUpdateSessionAgentModel)
	// 模拟认证中间件，把测试头中的用户 ID 写入请求上下文。
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := auth.ContextWithUserID(req.Context(), req.Header.Get("X-Test-User"))
		r.ServeHTTP(w, req.WithContext(ctx))
	})
	return wrapped, rt, st
}

func TestUpdateAgentModels(t *testing.T) {
	router, _, _ := newAgentsTestRouter(t)
	body := []byte(`{"model":"mock","reasoning_effort":"auto","reasoning_visibility":"hide"}`)
	req, err := http.NewRequest(http.MethodPut, "/api/v1/agents/leader/models", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("更新应返回 200，实际: %d body=%s", rec.Code, rec.Body.String())
	}

	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil))
	var response struct {
		Agents []runtime.AgentInfo `json:"agents"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析列表失败: %v", err)
	}
	var leader *runtime.AgentInfo
	for index := range response.Agents {
		if response.Agents[index].ID == "leader" {
			leader = &response.Agents[index]
			break
		}
	}
	if leader == nil || leader.ReasoningEffort != "auto" ||
		leader.ReasoningVisibility != "hide" {
		t.Fatalf("roster 未反映模型策略更新: %+v", response.Agents)
	}
}

// TestListAgents 验证 Agent 目录端点：Leader/Query 的 id/role/mode/状态/模型/活动
// 按 ID 升序返回，JSON 字段为 snake_case。
func TestListAgents(t *testing.T) {
	router, _, _ := newAgentsTestRouter(t)
	req, err := http.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际: %d（body: %s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(body.Agents) != 3 {
		t.Fatalf("应返回 3 个成员，实际: %s", rec.Body.String())
	}
	if body.Agents[0]["id"] != "developer-1" || body.Agents[1]["id"] != "leader" ||
		body.Agents[2]["id"] != "query-1" {
		t.Errorf("成员应按 ID 升序: %s", rec.Body.String())
	}

	byID := make(map[string]map[string]any, len(body.Agents))
	for _, a := range body.Agents {
		byID[a["id"].(string)] = a
	}
	leader := byID["leader"]
	if leader["role"] != "leader" || leader["mode"] != "coordinator" ||
		leader["status"] != "idle" || leader["model"] != "mock" {
		t.Errorf("leader 条目不符: %+v", leader)
	}
	if leader["description"] == "" || leader["tool_namespaces"] == nil ||
		leader["pinned_tools"] == nil || leader["skill_names"] == nil ||
		leader["max_turns"] == nil || leader["context_tokens"] == nil {
		t.Errorf("leader Profile 详情字段缺失: %+v", leader)
	}
	query := byID["query-1"]
	if query["role"] != "query" || query["mode"] != "service" || query["status"] != "idle" {
		t.Errorf("query 条目不符: %+v", query)
	}
}

// TestSessionAgentModelsAPI 验证会话创建时固化两名 Agent 的模型快照，且
// 用户可以只覆盖 Query，不会改变 Leader 的模型选择。
func TestSessionAgentModelsAPI(t *testing.T) {
	router, rt, st := newAgentsTestRouter(t)
	now := time.Now().UTC()
	session := store.ChatSession{ID: "cs-agent-model", UserID: "usr-1",
		Title: "模型配置", CreatedAt: now, UpdatedAt: now}
	if err := st.CreateChatSession(session); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := rt.InitializeSessionModels(session.ID); err != nil {
		t.Fatalf("初始化模型快照失败: %v", err)
	}

	getReq := httptest.NewRequest(http.MethodGet,
		"/api/v1/chat/sessions/cs-agent-model/agents", nil)
	getReq.Header.Set("X-Test-User", "usr-1")
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("查询会话 Agent 失败: status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	var listed struct {
		Agents []runtime.SessionAgentModelView `json:"agents"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &listed); err != nil || len(listed.Agents) != 3 {
		t.Fatalf("会话 Agent 列表不符: err=%v body=%s", err, getRec.Body.String())
	}

	body := bytes.NewBufferString(`{"endpoint_id":"mock-d3","reasoning_effort":"high"}`)
	putReq := httptest.NewRequest(http.MethodPut,
		"/api/v1/chat/sessions/cs-agent-model/agents/query-1/model", body)
	putReq.Header.Set("X-Test-User", "usr-1")
	putRec := httptest.NewRecorder()
	router.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("覆盖 Query 模型失败: status=%d body=%s", putRec.Code, putRec.Body.String())
	}
	query, err := st.GetSessionAgentModel(session.ID, "query-1")
	if err != nil || query.EndpointID != "mock-d3" ||
		query.Source != store.ModelSourceSessionOverride {
		t.Fatalf("Query 快照不符: snapshot=%+v err=%v", query, err)
	}
	leader, err := st.GetSessionAgentModel(session.ID, "leader")
	if err != nil || leader.EndpointID != "mock" {
		t.Fatalf("Query 覆盖不应影响 Leader: snapshot=%+v err=%v", leader, err)
	}
}

// TestSessionAgentToolsAPI 验证会话有效工具目录包含运行时 AgentTool 与 Eino
// Filesystem Middleware 工具；这些工具不属于全局 Registry，但确实会交给
// Leader 模型。Query 自身不能再次看到 ask_query，避免递归委派。
func TestSessionAgentToolsAPI(t *testing.T) {
	router, _, st := newAgentsTestRouter(t)
	project, err := st.EnsureDefaultProject("usr-tools")
	if err != nil {
		t.Fatalf("创建 Default Project 失败: %v", err)
	}
	now := time.Now().UTC()
	session := store.ChatSession{ID: "cs-agent-tools", UserID: "usr-tools",
		ProjectID: project.ID, Title: "工具目录", CreatedAt: now, UpdatedAt: now}
	if err := st.CreateChatSession(session); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}

	requestTools := func(agentID string) runtime.SessionAgentToolsView {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/chat/sessions/cs-agent-tools/agents/"+agentID+"/tools", nil)
		req.Header.Set("X-Test-User", "usr-tools")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("查询 %s 有效工具失败: status=%d body=%s", agentID, rec.Code, rec.Body.String())
		}
		var view runtime.SessionAgentToolsView
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatalf("解析有效工具响应失败: %v", err)
		}
		return view
	}

	leader := requestTools("leader")
	byName := make(map[string]runtime.SessionAgentToolView, len(leader.Tools))
	for _, item := range leader.Tools {
		byName[item.Name] = item
	}
	for _, name := range []string{"ask_query", "ls", "read_file", "glob", "grep"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("Leader 有效工具缺少 %q: %+v", name, leader.Tools)
		}
	}
	if byName["ask_query"].Delivery != runtime.ToolDeliveryAgent ||
		byName["read_file"].Delivery != runtime.ToolDeliveryMiddleware {
		t.Errorf("运行时工具来源或交付方式不符: ask=%+v read=%+v",
			byName["ask_query"], byName["read_file"])
	}

	developer := requestTools("developer-1")
	workerHasConsultation := false
	for _, item := range developer.Tools {
		if item.Name == "ask_query" && item.Delivery == runtime.ToolDeliveryAgent {
			workerHasConsultation = true
		}
		if item.Name == "robot.run" || item.Name == "robot.stop" {
			t.Fatalf("Developer Task Agent 的只读咨询能力不得扩大为 Robot 权限: %+v",
				developer.Tools)
		}
	}
	if !workerHasConsultation {
		t.Fatalf("正常 Task Agent 应获得同一 Team 的只读咨询工具: %+v", developer.Tools)
	}

	query := requestTools("query-1")
	for _, item := range query.Tools {
		if item.Name == "ask_query" {
			t.Fatalf("Query 不应递归获得 Leader 的委派工具: %+v", query.Tools)
		}
	}
}
