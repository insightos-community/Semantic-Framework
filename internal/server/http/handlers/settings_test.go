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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/llm"
	"insightos.cn/semantic-framework/pkg/log"
)

// fakePatcher 是测试用的 ConfigPatcher：预置 GET 树与 PATCH 结果，
// 记录 PATCH 入参供断言。
type fakePatcher struct {
	tree    map[string]any
	hash    string
	newTree map[string]any
	newHash string
	changed []string
	err     error

	gotBaseHash string
	gotPatch    map[string]any
}

// ConfigPath 返回测试固定路径，用于断言 handler 会透传当前实例的写回目标。
func (f *fakePatcher) ConfigPath() string { return "/tmp/test-semantic-server.yaml" }

// CurrentTree 返回预置的配置树与哈希。
func (f *fakePatcher) CurrentTree() (map[string]any, string, error) {
	return f.tree, f.hash, nil
}

// Patch 记录入参并返回预置结果。
func (f *fakePatcher) Patch(baseHash string, patch map[string]any) (map[string]any, string, []string, error) {
	f.gotBaseHash = baseHash
	f.gotPatch = patch
	if f.err != nil {
		return nil, "", nil, f.err
	}
	return f.newTree, f.newHash, f.changed, nil
}

// storeKeyStore 把 store 的 ErrNotFound 语义适配为 llm.KeyStore 约定的
// ("", nil)（与 bootstrap 装配的适配器同逻辑，测试内独立实现）。
type storeKeyStore struct{ st *store.Store }

// GetKey 按端点名读取托管密钥，不存在返回 ("", nil)。
func (a storeKeyStore) GetKey(name string) (string, error) {
	value, err := a.st.GetKey(name)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	return value, err
}

// newSettingsTestRouter 装配设置域测试路由：临时库 + 真实注册表（注入
// store 兜底密钥）+ 假 patcher + 注入 user_id 的 chi 路由。
func newSettingsTestRouter(t *testing.T, patcher ConfigPatcher) (*store.Store, *llm.Registry, http.Handler) {
	t.Helper()
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
	st := storetest.OpenMigrated(t, logger)

	// 强制清空相关 env，保证"store 兜底"断言不受外部环境干扰
	// （空串在解析链中等价未设置）。
	t.Setenv("SEMANTIC_LLM_API_KEY_DEEPSEEK_CHAT", "")
	t.Setenv("SEMANTIC_LLM_API_KEY_DEEPSEEK_REASONER", "")

	registry, err := llm.Load(config.LLMConfig{
		Default: "mock",
		Providers: map[string]config.LLMProviderConfig{
			"mock":          {Component: "mock", Model: "mock"},
			"deepseek-chat": {Component: "openai", BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat"},
		},
	})
	if err != nil {
		t.Fatalf("llm.Load 失败: %v", err)
	}
	registry.SetKeyStore(storeKeyStore{st})

	h := NewSettingsHandler(st, registry, patcher, logger)
	r := chi.NewRouter()
	r.Route("/api/v1/settings", func(r chi.Router) {
		r.Get("/", h.HandleGetSettings)
		r.Patch("/", h.HandlePatchSettings)
		r.Get("/keys", h.HandleListKeys)
		r.Put("/keys/{name}", h.HandlePutKey)
		r.Delete("/keys/{name}", h.HandleDeleteKey)
	})
	// 模拟 auth 中间件：按 X-Test-User 头注入 user_id。
	return st, registry, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := auth.ContextWithUserID(req.Context(), req.Header.Get("X-Test-User"))
		r.ServeHTTP(w, req.WithContext(ctx))
	})
}

// TestMaskSecret 验证敏感值掩码规则：保留前 6 字符，短值整体掩码。
func TestMaskSecret(t *testing.T) {
	if got := MaskSecret("sk-abcdef123456"); got != "sk-abc***" {
		t.Errorf("长值应保留前 6 字符，实际: %q", got)
	}
	if got := MaskSecret("sk-abc"); got != "***" {
		t.Errorf("恰好 6 字符应整体掩码，实际: %q", got)
	}
	if got := MaskSecret("x"); got != "***" {
		t.Errorf("短值应整体掩码，实际: %q", got)
	}
}

// TestMaskTree 验证配置树掩码：含 key/password/token 的键名（大小写不敏感）
// 被掩码，嵌套映射递归，普通值原样保留，输入树不被修改。
func TestMaskTree(t *testing.T) {
	tree := map[string]any{
		"log": map[string]any{"level": "info"},
		"llm": map[string]any{
			"providers": map[string]any{
				"foo": map[string]any{
					"options": map[string]any{
						"api_key":  "sk-abcdef123456",
						"Password": "short",
						"tokens":   4096,
					},
				},
			},
		},
		"mcp_servers": []any{
			map[string]any{
				"name": "helper",
				"env":  []any{"HELPER_TOKEN=secret-value-123", "PLAIN_VAR=visible", "NO_EQUALS"},
			},
		},
	}
	masked := MaskTree(tree)

	options := masked["llm"].(map[string]any)["providers"].(map[string]any)["foo"].(map[string]any)["options"].(map[string]any)
	if got := options["api_key"]; got != "sk-abc***" {
		t.Errorf("api_key 应掩码为 sk-abc***，实际: %v", got)
	}
	if got := options["Password"]; got != "***" {
		t.Errorf("Password（短值）应整体掩码，实际: %v", got)
	}
	if got := options["tokens"]; got != 4096 {
		t.Errorf("数值敏感键（如 max_tokens）不是凭据，不应掩码，实际: %v", got)
	}
	if got := masked["log"].(map[string]any)["level"]; got != "info" {
		t.Errorf("普通值不应掩码，实际: %v", got)
	}

	// mcp_servers[].env：K 命中敏感片段的 "K=V" 元素掩码 V，其余原样。
	env := masked["mcp_servers"].([]any)[0].(map[string]any)["env"].([]any)
	if got := env[0]; got != "HELPER_TOKEN=secret***" {
		t.Errorf("敏感 env 元素应掩码 V，实际: %v", got)
	}
	if got := env[1]; got != "PLAIN_VAR=visible" {
		t.Errorf("非敏感 env 元素不应掩码，实际: %v", got)
	}
	if got := env[2]; got != "NO_EQUALS" {
		t.Errorf("非赋值式元素不应掩码，实际: %v", got)
	}

	// 输入树不被修改。
	orig := tree["llm"].(map[string]any)["providers"].(map[string]any)["foo"].(map[string]any)["options"].(map[string]any)
	if orig["api_key"] != "sk-abcdef123456" {
		t.Error("MaskTree 不应修改输入树")
	}
	if tree["mcp_servers"].([]any)[0].(map[string]any)["env"].([]any)[0] != "HELPER_TOKEN=secret-value-123" {
		t.Error("MaskTree 不应修改输入树的 env 元素")
	}
}

// TestGetSettings 验证 GET 返回掩码后的配置树、base_hash 与不含密钥值的
// 实际凭据来源；环境变量优先时必须明确标为 env。
func TestGetSettings(t *testing.T) {
	patcher := &fakePatcher{
		tree: map[string]any{
			"llm": map[string]any{
				"default": "mock",
				"providers": map[string]any{
					"mock": map[string]any{"options": map[string]any{"api_key": "sk-abcdef123456"}},
				},
			},
		},
		hash: "hash-1",
	}
	_, _, router := newSettingsTestRouter(t, patcher)
	t.Setenv("SEMANTIC_LLM_API_KEY_DEEPSEEK_CHAT", "sk-env-secret-never-return")

	code, resp := doJSON(t, router, http.MethodGet, "/api/v1/settings", "usr-1", "")
	if code != http.StatusOK {
		t.Fatalf("GET 应返回 200，实际: %d（%v）", code, resp)
	}
	if resp["base_hash"] != "hash-1" {
		t.Errorf("base_hash 不符: %v", resp["base_hash"])
	}
	settings := resp["settings"].(map[string]any)
	options := settings["llm"].(map[string]any)["providers"].(map[string]any)["mock"].(map[string]any)["options"].(map[string]any)
	if options["api_key"] != "sk-abc***" {
		t.Errorf("响应中的 api_key 应掩码，实际: %v", options["api_key"])
	}
	sources := resp["key_sources"].(map[string]any)
	if sources["deepseek-chat"] != "env" || sources["mock"] != "none" {
		t.Errorf("凭据来源应反映 env 优先且 mock 无需密钥，实际: %v", sources)
	}
	if strings.Contains(fmt.Sprint(resp), "sk-env-secret-never-return") {
		t.Error("key_sources 响应绝不能包含环境变量密钥值")
	}
	defaults, ok := resp["runtime_defaults"].(map[string]any)
	if !ok {
		t.Fatalf("GET 应返回 runtime_defaults，实际: %v", resp["runtime_defaults"])
	}
	openai := defaults["openai"].(map[string]any)
	if openai["timeout_seconds"].(float64) != float64(kernel.DefaultModelRequestTimeoutSeconds) ||
		openai["max_tokens"].(float64) != float64(kernel.DefaultMaxOutputTokens) {
		t.Errorf("openai 运行时默认值不符: %v", openai)
	}
	claude := defaults["claude"].(map[string]any)
	if _, hasTimeout := claude["timeout_seconds"]; hasTimeout {
		t.Errorf("claude 不应暴露 timeout_seconds，实际: %v", claude)
	}
	if claude["max_tokens"].(float64) != float64(kernel.DefaultMaxOutputTokens) {
		t.Errorf("claude max_tokens 默认值不符: %v", claude)
	}
}

// TestPatchSettingsSuccess 验证 PATCH 成功路径：入参透传（base_hash/patch）、
// 返回新快照与新哈希、审计写入（变更键清单，不含值）。
func TestPatchSettingsSuccess(t *testing.T) {
	patcher := &fakePatcher{
		newTree: map[string]any{"llm": map[string]any{"default": "deepseek-chat"}},
		newHash: "hash-2",
		changed: []string{"llm.default"},
	}
	st, _, router := newSettingsTestRouter(t, patcher)

	code, resp := doJSON(t, router, http.MethodPatch, "/api/v1/settings", "usr-1",
		`{"base_hash":"hash-1","patch":{"llm":{"default":"deepseek-chat"}}}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH 应返回 200，实际: %d（%v）", code, resp)
	}
	if patcher.gotBaseHash != "hash-1" {
		t.Errorf("base_hash 应透传给 patcher，实际: %q", patcher.gotBaseHash)
	}
	if patcher.gotPatch["llm"].(map[string]any)["default"] != "deepseek-chat" {
		t.Errorf("patch 应透传给 patcher，实际: %v", patcher.gotPatch)
	}
	if resp["base_hash"] != "hash-2" {
		t.Errorf("应返回新 base_hash，实际: %v", resp["base_hash"])
	}
	changed, ok := resp["changed"].([]any)
	if !ok || len(changed) != 1 || changed[0] != "llm.default" {
		t.Errorf("应返回变更键清单，实际: %v", resp["changed"])
	}

	entries, err := st.ListAudit(10)
	if err != nil {
		t.Fatalf("ListAudit 不应失败: %v", err)
	}
	if len(entries) != 1 || entries[0].Action != "settings.patch" ||
		entries[0].UserID != "usr-1" || entries[0].Detail != "llm.default" {
		t.Fatalf("应写入一条 settings.patch 审计（键清单），实际: %+v", entries)
	}
	if strings.Contains(entries[0].Detail, "deepseek-chat") {
		t.Error("审计 detail 不应包含变更值")
	}
}

// TestPatchSettingsConflict 验证乐观锁冲突映射为 409 SETTINGS_CONFLICT。
func TestPatchSettingsConflict(t *testing.T) {
	patcher := &fakePatcher{err: &PatchReject{
		Status: http.StatusConflict, Code: CodeSettingsConflict, Message: "配置快照已被修改",
	}}
	_, _, router := newSettingsTestRouter(t, patcher)

	code, resp := doJSON(t, router, http.MethodPatch, "/api/v1/settings", "usr-1",
		`{"base_hash":"stale","patch":{"llm":{"default":"mock"}}}`)
	if code != http.StatusConflict {
		t.Fatalf("冲突应返回 409，实际: %d（%v）", code, resp)
	}
	errBody := resp["error"].(map[string]any)
	if errBody["code"] != CodeSettingsConflict {
		t.Errorf("错误码应为 SETTINGS_CONFLICT，实际: %v", errBody["code"])
	}
}

// TestPatchSettingsValidationFailure 验证 schema 校验失败映射为 400 且带位置信息。
func TestPatchSettingsValidationFailure(t *testing.T) {
	patcher := &fakePatcher{err: &PatchReject{
		Status: http.StatusBadRequest, Code: CodeBadRequest,
		Message: "配置校验失败:\n  - llm.providers.mock.componet: 未知配置键",
	}}
	_, _, router := newSettingsTestRouter(t, patcher)

	code, resp := doJSON(t, router, http.MethodPatch, "/api/v1/settings", "usr-1",
		`{"base_hash":"hash-1","patch":{"llm":{"providers":{"mock":{"componet":"x"}}}}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("校验失败应返回 400，实际: %d（%v）", code, resp)
	}
	errBody := resp["error"].(map[string]any)
	if !strings.Contains(errBody["message"].(string), "llm.providers.mock.componet") {
		t.Errorf("错误信息应含问题位置，实际: %v", errBody["message"])
	}
}

// TestPatchSettingsBadRequest 验证请求体层面的 400：非法 JSON、缺 base_hash、空 patch。
func TestPatchSettingsBadRequest(t *testing.T) {
	patcher := &fakePatcher{}
	_, _, router := newSettingsTestRouter(t, patcher)

	cases := map[string]string{
		"非法 JSON":     `{"base_hash":`,
		"缺 base_hash": `{"patch":{"llm":{"default":"mock"}}}`,
		"空 patch":     `{"base_hash":"hash-1","patch":{}}`,
	}
	for name, body := range cases {
		code, resp := doJSON(t, router, http.MethodPatch, "/api/v1/settings", "usr-1", body)
		if code != http.StatusBadRequest {
			t.Errorf("用例 %q 应返回 400，实际: %d（%v）", name, code, resp)
		}
	}
}

// TestPutKey 验证 PUT 成功路径：写库、注册表经 store 兜底解析到新 key
// （env 为空场景）、来源标记、审计写入。
func TestPutKey(t *testing.T) {
	patcher := &fakePatcher{}
	st, registry, router := newSettingsTestRouter(t, patcher)

	code, resp := doJSON(t, router, http.MethodPut, "/api/v1/settings/keys/deepseek-chat", "usr-1",
		`{"key_value":"sk-managed-123456"}`)
	if code != http.StatusOK {
		t.Fatalf("PUT 应返回 200，实际: %d（%v）", code, resp)
	}

	value, err := st.GetKey("deepseek-chat")
	if err != nil || value != "sk-managed-123456" {
		t.Fatalf("库内应存新密钥，实际: %q, %v", value, err)
	}
	if key := registry.APIKey("deepseek-chat"); key != "sk-managed-123456" {
		t.Errorf("注册表应经 store 兜底解析到新 key，实际: %q", key)
	}
	if source := registry.APIKeySource("deepseek-chat"); source != llm.KeySourceStore {
		t.Errorf("来源应为 store，实际: %q", source)
	}
	getCode, getResp := doJSON(t, router, http.MethodGet, "/api/v1/settings", "usr-1", "")
	if getCode != http.StatusOK ||
		getResp["key_sources"].(map[string]any)["deepseek-chat"] != "store" {
		t.Errorf("写入后 GET settings 应显示 store 生效，status=%d resp=%v", getCode, getResp)
	}

	entries, err := st.ListAudit(10)
	if err != nil {
		t.Fatalf("ListAudit 不应失败: %v", err)
	}
	if len(entries) != 1 || entries[0].Action != "settings.key_set" ||
		entries[0].UserID != "usr-1" || entries[0].Detail != "deepseek-chat" {
		t.Fatalf("应写入一条 settings.key_set 审计（只记名称），实际: %+v", entries)
	}
}

// TestPutKeyBadRequest 验证 PUT 的 400 场景：值过短、端点不存在、非法 JSON。
func TestPutKeyBadRequest(t *testing.T) {
	patcher := &fakePatcher{}
	_, _, router := newSettingsTestRouter(t, patcher)

	cases := map[string]struct {
		name string
		body string
	}{
		"值过短":     {"deepseek-chat", `{"key_value":"sk-1"}`},
		"端点不存在":   {"not-exist", `{"key_value":"sk-managed-123456"}`},
		"非法 JSON": {"deepseek-chat", `{"key_value":`},
	}
	for caseName, c := range cases {
		code, resp := doJSON(t, router, http.MethodPut, "/api/v1/settings/keys/"+c.name, "usr-1", c.body)
		if code != http.StatusBadRequest {
			t.Errorf("用例 %q 应返回 400，实际: %d（%v）", caseName, code, resp)
		}
	}
}

// TestDeleteKey 验证 DELETE：存在的密钥删除后返回 204、库内移除、审计写入；
// 不存在返回 404 SETTINGS_KEY_NOT_FOUND。
func TestDeleteKey(t *testing.T) {
	patcher := &fakePatcher{}
	st, registry, router := newSettingsTestRouter(t, patcher)

	if err := st.SetKey("deepseek-chat", "sk-managed-123456"); err != nil {
		t.Fatalf("预置密钥失败: %v", err)
	}
	registry.InvalidateKeyCache()
	if key := registry.APIKey("deepseek-chat"); key != "sk-managed-123456" {
		t.Fatalf("预置后注册表应读到密钥，实际: %q", key)
	}

	code, _ := doJSON(t, router, http.MethodDelete, "/api/v1/settings/keys/deepseek-chat", "usr-1", "")
	if code != http.StatusNoContent {
		t.Fatalf("DELETE 应返回 204，实际: %d", code)
	}
	if _, err := st.GetKey("deepseek-chat"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("删除后库内应不存在，实际: %v", err)
	}
	if key := registry.APIKey("deepseek-chat"); key != "" {
		t.Errorf("删除并清缓存后注册表应读不到密钥，实际: %q", key)
	}

	entries, err := st.ListAudit(10)
	if err != nil {
		t.Fatalf("ListAudit 不应失败: %v", err)
	}
	if len(entries) != 1 || entries[0].Action != "settings.key_delete" || entries[0].Detail != "deepseek-chat" {
		t.Fatalf("应写入一条 settings.key_delete 审计，实际: %+v", entries)
	}

	code, resp := doJSON(t, router, http.MethodDelete, "/api/v1/settings/keys/deepseek-chat", "usr-1", "")
	if code != http.StatusNotFound {
		t.Fatalf("删除不存在的密钥应返回 404，实际: %d（%v）", code, resp)
	}
	if resp["error"].(map[string]any)["code"] != CodeSettingsKeyNotFound {
		t.Errorf("错误码应为 SETTINGS_KEY_NOT_FOUND，实际: %v", resp)
	}
}

// TestListKeys 验证 GET keys：名称升序、值掩码、含更新时间。
func TestListKeys(t *testing.T) {
	patcher := &fakePatcher{}
	st, _, router := newSettingsTestRouter(t, patcher)

	for _, k := range [][2]string{{"deepseek-chat", "sk-chat-123456"}, {"mock", "sk-mock-123456"}} {
		if err := st.SetKey(k[0], k[1]); err != nil {
			t.Fatalf("预置密钥失败: %v", err)
		}
	}

	code, resp := doJSON(t, router, http.MethodGet, "/api/v1/settings/keys", "usr-1", "")
	if code != http.StatusOK {
		t.Fatalf("GET keys 应返回 200，实际: %d（%v）", code, resp)
	}
	keys := resp["keys"].([]any)
	if len(keys) != 2 {
		t.Fatalf("应返回 2 条密钥，实际: %v", keys)
	}
	first := keys[0].(map[string]any)
	if first["name"] != "deepseek-chat" {
		t.Errorf("清单应按名称升序，实际首条: %v", first["name"])
	}
	if first["key_value"] != "sk-cha***" {
		t.Errorf("值应掩码为 sk-cha***，实际: %v", first["key_value"])
	}
	if first["updated_at"] == "" || first["updated_at"] == nil {
		t.Errorf("应含 updated_at，实际: %v", first)
	}
}
