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

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/bootstrap"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/llm"
	"insightos.cn/semantic-framework/pkg/log"
)

// settingsTestYAML 是集成测试用的配置文件内容：mock 为默认模型，
// deepseek-chat 端点（openai 驱动）用于托管密钥与 PATCH 切换默认模型。
// options 只允许 applyOptions 白名单键；密钥走 env / 托管密钥库，不入配置。
const settingsTestYAML = `llm:
  default: mock
  providers:
    mock:
      component: mock
      model: "mock"
      capabilities: [text, tool_call]
      price: {prompt: 0, completion: 0}
    deepseek-chat:
      component: openai
      base_url: "https://api.deepseek.com/v1"
      model: "deepseek-chat"
      capabilities: [text, tool_call]
      options: {temperature: 0.7}
      price: {prompt: 0.001, completion: 0.002}
`

// startSettingsApp 以临时配置文件 + 临时库启动装配后的服务，
// 并开启热重载（settings PATCH 的写回与热应用依赖它）。
func startSettingsApp(t *testing.T, configPath string) (httpBase string, app *bootstrap.App, stop func()) {
	return startSettingsAppWithLeaderModel(t, configPath, "mock")
}

// startSettingsAppWithLeaderModel 允许测试使用固定模型或空模型（继承系统
// Default）的 Leader Profile；生产安装模板采用后者。
func startSettingsAppWithLeaderModel(t *testing.T, configPath, leaderModel string) (
	httpBase string, app *bootstrap.App, stop func()) {
	t.Helper()
	t.Setenv("SEMANTIC_ADMIN_PASSWORD", "test-admin-pass")
	// 强制清空端点 env（空串在解析链中等价未设置），
	// 保证"store 兜底"断言不受外部环境干扰。
	t.Setenv("SEMANTIC_LLM_API_KEY_DEEPSEEK_CHAT", "")

	// 测试使用自有的最小 leader profile，不读取仓库模板或开发者安装配置；
	// 这样本测试只验证设置 API，不会被工作区中正在编辑的 Agent 模型影响。
	profilesDir := filepath.Join(t.TempDir(), "agents")
	leaderDir := filepath.Join(profilesDir, "leader")
	if err := os.MkdirAll(leaderDir, 0o700); err != nil {
		t.Fatalf("创建测试 profile 目录失败: %v", err)
	}
	roleYAML := "name: leader\nmode: coordinator\n"
	if leaderModel != "" {
		roleYAML += "model: " + leaderModel + "\n"
	}
	if err := os.WriteFile(filepath.Join(leaderDir, "role.yaml"),
		[]byte(roleYAML), 0o600); err != nil {
		t.Fatalf("写入测试 role.yaml 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(leaderDir, "AGENT.md"), []byte("你是测试协调 Agent。\n"), 0o600); err != nil {
		t.Fatalf("写入测试 AGENT.md 失败: %v", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("加载测试配置失败: %v", err)
	}
	cfg.Server.HTTPAddr = freeAddr(t)
	cfg.Server.WSAddr = freeAddr(t)
	cfg.Store.SQLitePath = filepath.Join(t.TempDir(), "settings-it.db")
	storetest.SeedMigratedAt(t, cfg.Store.SQLitePath)
	cfg.Agents.ProfilesDir = profilesDir
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})

	app, err = bootstrap.Wire(cfg, logger)
	if err != nil {
		t.Fatalf("Wire 装配失败: %v", err)
	}
	if err := app.EnableConfigReload(configPath, nil); err != nil {
		t.Fatalf("EnableConfigReload 失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- app.Run(ctx)
	}()

	httpBase = fmt.Sprintf("http://%s", cfg.Server.HTTPAddr)
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(httpBase + "/api/v1/system/healthz")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("服务在 30s 内未就绪，最后一次错误: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	stop = func() {
		cancel()
		select {
		case err := <-runDone:
			if err != nil {
				t.Errorf("App.Run 应随 ctx 取消正常退出，实际返回: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Errorf("App.Run 未在 15s 内退出")
		}
	}
	return httpBase, app, stop
}

// TestSettingsPatchWithDefaultInheritedAgent 回归真实 init 场景：安装模板中的
// Leader 不写 model，首次在前端切换系统 Default 时必须通过语义预校验。
// 空 model 不是悬空端点，已有会话仍由各自快照保持不变。
func TestSettingsPatchWithDefaultInheritedAgent(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "semantic-server.yaml")
	if err := os.WriteFile(configPath, []byte(settingsTestYAML), 0o600); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}
	httpBase, app, stop := startSettingsAppWithLeaderModel(t, configPath, "")
	defer stop()

	token := login(t, httpBase)
	settingsURL := httpBase + "/api/v1/settings"
	code, body := settingsRequest(t, http.MethodGet, settingsURL, token, "")
	if code != http.StatusOK {
		t.Fatalf("GET settings 应返回 200，实际: %d（%s）", code, body)
	}
	var current struct {
		BaseHash string `json:"base_hash"`
	}
	if err := json.Unmarshal(body, &current); err != nil || current.BaseHash == "" {
		t.Fatalf("解析初始 base_hash 失败: body=%s err=%v", body, err)
	}

	patchBody := fmt.Sprintf(`{"base_hash":%q,"patch":{"llm":{"default":"deepseek-chat"}}}`,
		current.BaseHash)
	code, body = settingsRequest(t, http.MethodPatch, settingsURL, token, patchBody)
	if code != http.StatusOK {
		t.Fatalf("继承 Default 的 Agent 不应阻塞首次模型配置: status=%d body=%s", code, body)
	}
	if got := app.LLMRegistry().Default().Name; got != "deepseek-chat" {
		t.Fatalf("热应用后的系统 Default 不符: %q", got)
	}
}

// TestSettingsPatchMaterializesProviderOptions 验证 PATCH 新增 openai 端点时
// 物化默认 options（timeout_seconds/max_tokens），且 options 非法键在保存时 400。
func TestSettingsPatchMaterializesProviderOptions(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "semantic-server.yaml")
	if err := os.WriteFile(configPath, []byte(settingsTestYAML), 0o644); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}
	httpBase, _, stop := startSettingsApp(t, configPath)
	defer stop()

	token := login(t, httpBase)
	settingsURL := httpBase + "/api/v1/settings"

	code, body := settingsRequest(t, http.MethodGet, settingsURL, token, "")
	if code != http.StatusOK {
		t.Fatalf("GET settings 应返回 200，实际: %d（%s）", code, body)
	}
	var current struct {
		BaseHash string `json:"base_hash"`
	}
	if err := json.Unmarshal(body, &current); err != nil || current.BaseHash == "" {
		t.Fatalf("解析初始 base_hash 失败: body=%s err=%v", body, err)
	}

	// ① 新增 openai 端点（不带 options）：应被物化默认值。
	patchBody := fmt.Sprintf(`{"base_hash":%q,"patch":{"llm":{"providers":{"local-llama":{"component":"openai","base_url":"http://127.0.0.1:9999/v1","model":"local","capabilities":["text","tool_call"]}}}}}`,
		current.BaseHash)
	code, body = settingsRequest(t, http.MethodPatch, settingsURL, token, patchBody)
	if code != http.StatusOK {
		t.Fatalf("PATCH 新增端点应返回 200，实际: %d（%s）", code, body)
	}

	// ② GET 应能看到物化的 options。
	code, body = settingsRequest(t, http.MethodGet, settingsURL, token, "")
	if code != http.StatusOK {
		t.Fatalf("GET settings 应返回 200，实际: %d（%s）", code, body)
	}
	var after struct {
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(body, &after); err != nil {
		t.Fatalf("解析 GET 响应失败: %v", err)
	}
	providers := after.Settings["llm"].(map[string]any)["providers"].(map[string]any)
	options := providers["local-llama"].(map[string]any)["options"].(map[string]any)
	if options["timeout_seconds"] != float64(300) || options["max_tokens"] != float64(16384) {
		t.Fatalf("新端点 options 应被物化默认值，实际: %v", options)
	}

	// ③ 写回的配置文件包含物化值。
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("读取写回的配置文件失败: %v", err)
	}
	if !strings.Contains(string(data), "timeout_seconds: 300") || !strings.Contains(string(data), "max_tokens: 16384") {
		t.Fatalf("配置文件应包含物化默认 options，实际:\n%s", data)
	}

	// ④ options 非法键在保存时 400（而不是等第一次会话构建模型才失败）。
	code, body = settingsRequest(t, http.MethodGet, settingsURL, token, "")
	if code != http.StatusOK {
		t.Fatalf("GET settings 应返回 200，实际: %d（%s）", code, body)
	}
	if err := json.Unmarshal(body, &current); err != nil || current.BaseHash == "" {
		t.Fatalf("解析 base_hash 失败: body=%s err=%v", body, err)
	}
	badPatch := fmt.Sprintf(`{"base_hash":%q,"patch":{"llm":{"providers":{"local-llama":{"options":{"foo":1}}}}}}`,
		current.BaseHash)
	code, body = settingsRequest(t, http.MethodPatch, settingsURL, token, badPatch)
	if code != http.StatusBadRequest {
		t.Fatalf("非法 options 键的 PATCH 应返回 400，实际: %d（%s）", code, body)
	}
	if !strings.Contains(string(body), "foo") {
		t.Errorf("400 响应应指出非法键，实际: %s", body)
	}
}

// settingsRequest 发送带鉴权的 JSON 请求，返回状态码与原始响应体。
func settingsRequest(t *testing.T, method, url, token, body string) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s 请求失败: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}
	return resp.StatusCode, data
}

// TestSettingsRESTFlow 验证 settings REST 全链路：
// GET 掩码 → 鉴权拦截 → PUT 托管密钥（env 缺失时注册表经 store 兜底）→
// PATCH 切换默认模型（乐观锁 + 写回 + 热应用）→ 409 冲突 → 400 校验失败 →
// DELETE 密钥 → 审计记录齐全。
func TestSettingsRESTFlow(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "semantic-server.yaml")
	if err := os.WriteFile(configPath, []byte(settingsTestYAML), 0o644); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}
	httpBase, app, stop := startSettingsApp(t, configPath)
	defer stop()

	token := login(t, httpBase)
	settingsURL := httpBase + "/api/v1/settings"

	// ① 无 token：401（全部 settings 端点需鉴权）。
	code, _ := settingsRequest(t, http.MethodGet, settingsURL, "", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("无 token 的 GET settings 应返回 401，实际: %d", code)
	}

	// ② GET：base_hash 非空，llm.default 为 mock（掩码行为由 handlers 层
	// TestMaskTree 与下方 ④ 的 keys 端点覆盖——options 白名单不允许密钥类键）。
	code, body := settingsRequest(t, http.MethodGet, settingsURL, token, "")
	if code != http.StatusOK {
		t.Fatalf("GET settings 应返回 200，实际: %d（%s）", code, body)
	}
	var getResp struct {
		BaseHash   string            `json:"base_hash"`
		ConfigPath string            `json:"config_path"`
		KeySources map[string]string `json:"key_sources"`
		Settings   map[string]any    `json:"settings"`
	}
	if err := json.Unmarshal(body, &getResp); err != nil {
		t.Fatalf("解析 GET 响应失败: %v", err)
	}
	if getResp.BaseHash == "" {
		t.Fatal("GET 响应应含非空 base_hash")
	}
	if getResp.ConfigPath != configPath {
		t.Fatalf("设置 API 应指向启动指定配置: got=%q want=%q", getResp.ConfigPath, configPath)
	}
	if getResp.Settings["llm"].(map[string]any)["default"] != "mock" {
		t.Errorf("初始默认模型应为 mock，实际: %v", getResp.Settings["llm"])
	}
	if getResp.KeySources["deepseek-chat"] != "none" || getResp.KeySources["mock"] != "none" {
		t.Errorf("未写入密钥前来源都应为 none，实际: %v", getResp.KeySources)
	}

	// ③ PUT 托管密钥：env 缺失，注册表应经 store 兜底解析到新 key。
	code, body = settingsRequest(t, http.MethodPut, settingsURL+"/keys/deepseek-chat", token,
		`{"key_value":"sk-it-managed-key-1"}`)
	if code != http.StatusOK {
		t.Fatalf("PUT key 应返回 200，实际: %d（%s）", code, body)
	}
	if key := app.LLMRegistry().APIKey("deepseek-chat"); key != "sk-it-managed-key-1" {
		t.Errorf("注册表应经 store 兜底解析到新 key，实际: %q", key)
	}
	if source := app.LLMRegistry().APIKeySource("deepseek-chat"); source != llm.KeySourceStore {
		t.Errorf("密钥来源应为 store，实际: %q", source)
	}
	code, body = settingsRequest(t, http.MethodGet, settingsURL, token, "")
	if code != http.StatusOK || !strings.Contains(string(body), `"deepseek-chat":"store"`) {
		t.Errorf("写入后 settings 应公开 store 来源枚举，status=%d body=%s", code, body)
	}

	// PUT 参数校验：值过短与端点不存在均为 400。
	code, _ = settingsRequest(t, http.MethodPut, settingsURL+"/keys/deepseek-chat", token, `{"key_value":"sk-1"}`)
	if code != http.StatusBadRequest {
		t.Errorf("过短的 key_value 应返回 400，实际: %d", code)
	}
	code, _ = settingsRequest(t, http.MethodPut, settingsURL+"/keys/not-exist", token, `{"key_value":"sk-it-managed-key-1"}`)
	if code != http.StatusBadRequest {
		t.Errorf("不存在的端点应返回 400，实际: %d", code)
	}

	// ④ GET keys：名称 + 掩码值 + updated_at。
	code, body = settingsRequest(t, http.MethodGet, settingsURL+"/keys", token, "")
	if code != http.StatusOK {
		t.Fatalf("GET keys 应返回 200，实际: %d（%s）", code, body)
	}
	if strings.Contains(string(body), "sk-it-managed-key-1") || !strings.Contains(string(body), "sk-it-***") {
		t.Errorf("keys 清单值应掩码为 sk-it-***，实际: %s", body)
	}

	// ⑤ PATCH 切换默认模型：乐观锁通过 → 写回文件 → 热应用生效。
	patchBody := fmt.Sprintf(`{"base_hash":%q,"patch":{"llm":{"default":"deepseek-chat"}}}`, getResp.BaseHash)
	code, body = settingsRequest(t, http.MethodPatch, settingsURL, token, patchBody)
	if code != http.StatusOK {
		t.Fatalf("PATCH 应返回 200，实际: %d（%s）", code, body)
	}
	var patchResp struct {
		BaseHash string   `json:"base_hash"`
		Changed  []string `json:"changed"`
	}
	if err := json.Unmarshal(body, &patchResp); err != nil {
		t.Fatalf("解析 PATCH 响应失败: %v", err)
	}
	if len(patchResp.Changed) != 1 || patchResp.Changed[0] != "llm.default" {
		t.Errorf("changed 应为 [llm.default]，实际: %v", patchResp.Changed)
	}
	if patchResp.BaseHash == "" || patchResp.BaseHash == getResp.BaseHash {
		t.Errorf("PATCH 后 base_hash 应更新，实际: %q", patchResp.BaseHash)
	}
	if got := app.LLMRegistry().Default().Name; got != "deepseek-chat" {
		t.Errorf("热应用后默认模型应为 deepseek-chat，实际: %q", got)
	}

	// ⑥ 删除仍被 leader 引用的 mock 端点：即使它已不是默认端点，也必须
	// 在写回前返回 400，防止 Agent profile 留下悬空模型引用。
	deleteReferenced := fmt.Sprintf(`{"base_hash":%q,"patch":{"llm":{"providers":{"mock":null}}}}`, patchResp.BaseHash)
	code, body = settingsRequest(t, http.MethodPatch, settingsURL, token, deleteReferenced)
	if code != http.StatusBadRequest {
		t.Fatalf("删除 Agent 正在使用的端点应返回 400，实际: %d（%s）", code, body)
	}
	if !strings.Contains(string(body), `Agent \"leader\"`) || !strings.Contains(string(body), `端点 \"mock\"`) {
		t.Errorf("400 响应应指出 Agent 与悬空端点，实际: %s", body)
	}

	// 写回的文件包含新值（机器重写）。
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("读取写回的配置文件失败: %v", err)
	}
	if !strings.Contains(string(data), "default: deepseek-chat") {
		t.Errorf("配置文件应写回新默认值，实际:\n%s", data)
	}

	// ⑦ 409：用旧 base_hash 再 PATCH（乐观锁冲突）。
	code, body = settingsRequest(t, http.MethodPatch, settingsURL, token, patchBody)
	if code != http.StatusConflict {
		t.Fatalf("旧 base_hash 的 PATCH 应返回 409，实际: %d（%s）", code, body)
	}
	var errResp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &errResp); err != nil || errResp.Error.Code != "SETTINGS_CONFLICT" {
		t.Errorf("409 响应应携带 SETTINGS_CONFLICT，实际: %s", body)
	}

	// ⑧ 400：patch 引入未知键（schema 校验失败，带位置）。
	badPatch := fmt.Sprintf(`{"base_hash":%q,"patch":{"server":{"http-addr":":1"}}}`, patchResp.BaseHash)
	code, body = settingsRequest(t, http.MethodPatch, settingsURL, token, badPatch)
	if code != http.StatusBadRequest {
		t.Fatalf("未知键的 PATCH 应返回 400，实际: %d（%s）", code, body)
	}
	if !strings.Contains(string(body), "server.http-addr") {
		t.Errorf("400 响应应含问题位置 server.http-addr，实际: %s", body)
	}

	// ⑨ DELETE 密钥：204，注册表回落为空（env 缺失）。
	code, _ = settingsRequest(t, http.MethodDelete, settingsURL+"/keys/deepseek-chat", token, "")
	if code != http.StatusNoContent {
		t.Fatalf("DELETE key 应返回 204，实际: %d", code)
	}
	if key := app.LLMRegistry().APIKey("deepseek-chat"); key != "" {
		t.Errorf("删除后注册表应读不到密钥，实际: %q", key)
	}

	// ⑩ 审计：key_set / patch / key_delete 齐全，只记键清单不含值。
	entries, err := app.Store().ListAudit(10)
	if err != nil {
		t.Fatalf("ListAudit 不应失败: %v", err)
	}
	actions := map[string]string{}
	for _, e := range entries {
		actions[e.Action] = e.Detail
		if e.UserID == "" {
			t.Errorf("审计应记录操作人 user_id: %+v", e)
		}
	}
	if actions["settings.key_set"] != "deepseek-chat" {
		t.Errorf("应有 settings.key_set(deepseek-chat) 审计，实际: %v", actions)
	}
	if actions["settings.patch"] != "llm.default" {
		t.Errorf("应有 settings.patch(llm.default) 审计，实际: %v", actions)
	}
	if actions["settings.key_delete"] != "deepseek-chat" {
		t.Errorf("应有 settings.key_delete(deepseek-chat) 审计，实际: %v", actions)
	}
	for _, e := range entries {
		if strings.Contains(e.Detail, "sk-") {
			t.Errorf("审计 detail 不应包含密钥值: %+v", e)
		}
	}
}
