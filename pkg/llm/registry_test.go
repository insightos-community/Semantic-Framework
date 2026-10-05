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

package llm

import (
	"errors"
	"testing"

	"insightos.cn/semantic-framework/pkg/config"
)

// testLLMConfig 返回一份含共享 base_url 两个条目的测试配置。
func testLLMConfig() config.LLMConfig {
	return config.LLMConfig{
		Default: "deepseek-chat",
		Providers: map[string]config.LLMProviderConfig{
			"deepseek-chat": {
				Component:    "openai",
				BaseURL:      "https://api.deepseek.com/v1",
				Model:        "deepseek-chat",
				Capabilities: []string{"text", "tool_call"},
				Options:      map[string]any{"temperature": 0.7},
				Price:        config.LLMPriceConfig{Prompt: 0.001, Completion: 0.002},
			},
			"deepseek-reasoner": {
				Component: "openai",
				BaseURL:   "https://api.deepseek.com/v1",
				Model:     "deepseek-reasoner",
			},
			"mock": {
				Component: "mock",
				Model:     "mock",
			},
		},
	}
}

// TestLoadRegistry 验证注册表从配置加载并保留端点字段。
func TestLoadRegistry(t *testing.T) {
	reg, err := Load(testLLMConfig())
	if err != nil {
		t.Fatalf("Load 不应返回错误: %v", err)
	}

	p, err := reg.Get("deepseek-chat")
	if err != nil {
		t.Fatalf("Get(deepseek-chat) 不应返回错误: %v", err)
	}
	if p.Name != "deepseek-chat" || p.Component != "openai" || p.Model != "deepseek-chat" {
		t.Errorf("端点字段不符: %+v", p)
	}
	if p.Price.Prompt != 0.001 || p.Price.Completion != 0.002 {
		t.Errorf("单价不符: %+v", p.Price)
	}
	if len(p.Capabilities) != 2 || p.Capabilities[0] != "text" {
		t.Errorf("能力标签不符: %+v", p.Capabilities)
	}
}

// TestLoadRegistryDefault 验证 Default 返回 llm.default 指定的端点。
func TestLoadRegistryDefault(t *testing.T) {
	reg, err := Load(testLLMConfig())
	if err != nil {
		t.Fatalf("Load 不应返回错误: %v", err)
	}
	if reg.Default().Name != "deepseek-chat" {
		t.Errorf("Default 应为 deepseek-chat，实际: %q", reg.Default().Name)
	}
}

// TestLoadRegistryInvalid 验证非法配置被拒绝。
func TestLoadRegistryInvalid(t *testing.T) {
	cases := map[string]config.LLMConfig{
		"空 providers": {Default: "x"},
		"default 不在清单中": {
			Default:   "not-exist",
			Providers: map[string]config.LLMProviderConfig{"mock": {Component: "mock", Model: "mock"}},
		},
		"缺 component": {
			Default:   "a",
			Providers: map[string]config.LLMProviderConfig{"a": {Model: "m"}},
		},
		"缺 model": {
			Default:   "a",
			Providers: map[string]config.LLMProviderConfig{"a": {Component: "openai"}},
		},
	}
	for name, cfg := range cases {
		if _, err := Load(cfg); err == nil {
			t.Errorf("用例 %q：Load 应返回错误", name)
		}
	}
}

// TestRegistryGetMissing 验证查询不存在的端点返回错误。
func TestRegistryGetMissing(t *testing.T) {
	reg, err := Load(testLLMConfig())
	if err != nil {
		t.Fatalf("Load 不应返回错误: %v", err)
	}
	if _, err := reg.Get("not-exist"); err == nil {
		t.Error("Get(not-exist) 应返回错误")
	}
}

// TestAPIKeyEnv 验证密钥环境变量名的命名规则（大写、'-' 转 '_'）。
func TestAPIKeyEnv(t *testing.T) {
	if got := APIKeyEnv("deepseek-chat"); got != "SEMANTIC_LLM_API_KEY_DEEPSEEK_CHAT" {
		t.Errorf("环境变量名应为 SEMANTIC_LLM_API_KEY_DEEPSEEK_CHAT，实际: %q", got)
	}
}

// TestAPIKeyFromEnv 验证从本端点环境变量读取密钥。
func TestAPIKeyFromEnv(t *testing.T) {
	t.Setenv("SEMANTIC_LLM_API_KEY_DEEPSEEK_CHAT", "sk-chat")
	reg, err := Load(testLLMConfig())
	if err != nil {
		t.Fatalf("Load 不应返回错误: %v", err)
	}
	if key := reg.APIKey("deepseek-chat"); key != "sk-chat" {
		t.Errorf("应读到 sk-chat，实际: %q", key)
	}
}

// TestAPIKeySharedByBaseURL 验证同一 base_url 的条目共享同一把 key：
// 只设置 deepseek-chat 的密钥，deepseek-reasoner 也能读到。
func TestAPIKeySharedByBaseURL(t *testing.T) {
	t.Setenv("SEMANTIC_LLM_API_KEY_DEEPSEEK_CHAT", "sk-shared")
	reg, err := Load(testLLMConfig())
	if err != nil {
		t.Fatalf("Load 不应返回错误: %v", err)
	}
	if key := reg.APIKey("deepseek-reasoner"); key != "sk-shared" {
		t.Errorf("同 base_url 应共享密钥 sk-shared，实际: %q", key)
	}
}

// TestAPIKeyMissing 验证未设置密钥时返回空字符串，且不存在的端点同样返回空。
func TestAPIKeyMissing(t *testing.T) {
	reg, err := Load(testLLMConfig())
	if err != nil {
		t.Fatalf("Load 不应返回错误: %v", err)
	}
	if key := reg.APIKey("deepseek-chat"); key != "" {
		t.Errorf("未设置密钥应返回空，实际: %q", key)
	}
	if key := reg.APIKey("not-exist"); key != "" {
		t.Errorf("不存在的端点应返回空，实际: %q", key)
	}
}

// TestRegistryNames 验证端点名列表升序返回。
func TestRegistryNames(t *testing.T) {
	reg, err := Load(testLLMConfig())
	if err != nil {
		t.Fatalf("Load 不应返回错误: %v", err)
	}
	names := reg.Names()
	want := []string{"deepseek-chat", "deepseek-reasoner", "mock"}
	if len(names) != len(want) {
		t.Fatalf("Names 长度应为 %d，实际: %d", len(want), len(names))
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("Names[%d] 应为 %q，实际: %q", i, want[i], names[i])
		}
	}
}

// TestReloadSwapsSnapshot 验证 Reload 原子替换端点快照：
// 新配置立即生效，被移除的端点不再可查。
func TestReloadSwapsSnapshot(t *testing.T) {
	reg, err := Load(testLLMConfig())
	if err != nil {
		t.Fatalf("Load 不应返回错误: %v", err)
	}

	next := testLLMConfig()
	next.Default = "mock"
	delete(next.Providers, "deepseek-reasoner")
	if err := reg.Reload(next); err != nil {
		t.Fatalf("Reload 不应返回错误: %v", err)
	}

	if got := reg.Default().Name; got != "mock" {
		t.Errorf("Reload 后默认端点应为 mock，实际: %q", got)
	}
	if _, err := reg.Get("deepseek-reasoner"); err == nil {
		t.Error("Reload 后被移除的端点应不可查询")
	}
	if _, err := reg.Get("deepseek-chat"); err != nil {
		t.Errorf("Reload 后保留的端点应可查询: %v", err)
	}
}

// TestReloadInvalidKeepsOld 验证 Reload 校验失败时旧快照原样保留。
func TestReloadInvalidKeepsOld(t *testing.T) {
	reg, err := Load(testLLMConfig())
	if err != nil {
		t.Fatalf("Load 不应返回错误: %v", err)
	}

	bad := testLLMConfig()
	bad.Default = "not-in-providers"
	if err := reg.Reload(bad); err == nil {
		t.Fatal("default 不在 providers 中的配置应被 Reload 拒绝")
	}

	if got := reg.Default().Name; got != "deepseek-chat" {
		t.Errorf("Reload 失败后默认端点应保持 deepseek-chat，实际: %q", got)
	}
	if len(reg.Names()) != 3 {
		t.Errorf("Reload 失败后端点清单应保持 3 个，实际: %v", reg.Names())
	}
}

// TestReloadClearsKeyCache 验证 Reload 清空密钥缓存：
// 缓存的旧 key 不得带入新快照（env 可能已轮换）。
func TestReloadClearsKeyCache(t *testing.T) {
	t.Setenv("SEMANTIC_LLM_API_KEY_DEEPSEEK_CHAT", "sk-old")
	reg, err := Load(testLLMConfig())
	if err != nil {
		t.Fatalf("Load 不应返回错误: %v", err)
	}
	if key := reg.APIKey("deepseek-chat"); key != "sk-old" {
		t.Fatalf("预热线程密钥应为 sk-old，实际: %q", key)
	}

	t.Setenv("SEMANTIC_LLM_API_KEY_DEEPSEEK_CHAT", "sk-new")
	if err := reg.Reload(testLLMConfig()); err != nil {
		t.Fatalf("Reload 不应返回错误: %v", err)
	}
	if key := reg.APIKey("deepseek-chat"); key != "sk-new" {
		t.Errorf("Reload 后应重读环境变量得到 sk-new，实际: %q", key)
	}
}

// TestReloadConcurrentReads 验证 Reload 与并发查询无数据竞争（-race 保障），
// 且并发读期间注册表始终可用。
func TestReloadConcurrentReads(t *testing.T) {
	reg, err := Load(testLLMConfig())
	if err != nil {
		t.Fatalf("Load 不应返回错误: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = reg.Names()
			_, _ = reg.Get("mock")
			_ = reg.Default()
			_ = reg.APIKey("mock")
		}
	}()
	for i := 0; i < 50; i++ {
		if err := reg.Reload(testLLMConfig()); err != nil {
			t.Errorf("Reload 不应返回错误: %v", err)
		}
	}
	<-done
}

// fakeKeyStore 是测试用的内存托管密钥库（GetKey 契约：不存在返回 ("", nil)）。
type fakeKeyStore struct {
	keys map[string]string
	err  error
}

// GetKey 按端点名返回预置的密钥；err 非空时原样返回错误。
func (f *fakeKeyStore) GetKey(name string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.keys[name], nil
}

// TestAPIKeyStoreFallback 验证 env 缺失时回落到服务端托管密钥库，
// 且来源标记为 store。
func TestAPIKeyStoreFallback(t *testing.T) {
	reg, err := Load(testLLMConfig())
	if err != nil {
		t.Fatalf("Load 不应返回错误: %v", err)
	}
	reg.SetKeyStore(&fakeKeyStore{keys: map[string]string{"deepseek-chat": "sk-managed"}})

	if key := reg.APIKey("deepseek-chat"); key != "sk-managed" {
		t.Errorf("env 缺失时应回落到托管密钥 sk-managed，实际: %q", key)
	}
	if source := reg.APIKeySource("deepseek-chat"); source != KeySourceStore {
		t.Errorf("来源应为 store，实际: %q", source)
	}
}

func TestAPIKeySharedByService(t *testing.T) {
	reg, err := Load(config.LLMConfig{
		Default: "deepseek-v4-flash",
		Providers: map[string]config.LLMProviderConfig{
			"deepseek-v4-flash": {Service: "deepseek", Component: "openai", Model: "deepseek-v4-flash"},
			"deepseek-v4-pro":   {Service: "deepseek", Component: "openai", Model: "deepseek-v4-pro"},
		},
	})
	if err != nil {
		t.Fatalf("Load 不应返回错误: %v", err)
	}
	reg.SetKeyStore(&fakeKeyStore{keys: map[string]string{"deepseek": "sk-service"}})
	for _, endpoint := range []string{"deepseek-v4-flash", "deepseek-v4-pro"} {
		if key := reg.APIKey(endpoint); key != "sk-service" {
			t.Errorf("端点 %s 应共享服务密钥，实际: %q", endpoint, key)
		}
	}
	if !reg.HasService("deepseek") {
		t.Error("HasService 应识别 deepseek")
	}
}

// TestAPIKeyEnvPrecedence 验证 env（含 .env 注入）优先于托管密钥库。
func TestAPIKeyEnvPrecedence(t *testing.T) {
	t.Setenv("SEMANTIC_LLM_API_KEY_DEEPSEEK_CHAT", "sk-env")
	reg, err := Load(testLLMConfig())
	if err != nil {
		t.Fatalf("Load 不应返回错误: %v", err)
	}
	reg.SetKeyStore(&fakeKeyStore{keys: map[string]string{"deepseek-chat": "sk-managed"}})

	if key := reg.APIKey("deepseek-chat"); key != "sk-env" {
		t.Errorf("env 应优先于托管密钥，实际: %q", key)
	}
	if source := reg.APIKeySource("deepseek-chat"); source != KeySourceEnv {
		t.Errorf("来源应为 env，实际: %q", source)
	}
}

// TestAPIKeyFallbackAfterEnvRemoved 验证 env 删除并清空缓存后回落到托管密钥库
// （密钥轮换路径：.env 移除 + 热重载/InvalidateKeyCache 触发重读）。
func TestAPIKeyFallbackAfterEnvRemoved(t *testing.T) {
	t.Setenv("SEMANTIC_LLM_API_KEY_DEEPSEEK_CHAT", "sk-env")
	reg, err := Load(testLLMConfig())
	if err != nil {
		t.Fatalf("Load 不应返回错误: %v", err)
	}
	reg.SetKeyStore(&fakeKeyStore{keys: map[string]string{"deepseek-chat": "sk-managed"}})
	if key := reg.APIKey("deepseek-chat"); key != "sk-env" {
		t.Fatalf("预热时应读到 env 密钥 sk-env，实际: %q", key)
	}

	// env 删除（空串等价未设置）+ 缓存失效后，同一端点回落到托管密钥。
	t.Setenv("SEMANTIC_LLM_API_KEY_DEEPSEEK_CHAT", "")
	reg.InvalidateKeyCache()
	if key := reg.APIKey("deepseek-chat"); key != "sk-managed" {
		t.Errorf("env 删除后应回落到托管密钥 sk-managed，实际: %q", key)
	}
	if source := reg.APIKeySource("deepseek-chat"); source != KeySourceStore {
		t.Errorf("回落后来源应为 store，实际: %q", source)
	}
}

// TestAPIKeySourceNone 验证两端都没有密钥时返回空串且来源为 none。
func TestAPIKeySourceNone(t *testing.T) {
	reg, err := Load(testLLMConfig())
	if err != nil {
		t.Fatalf("Load 不应返回错误: %v", err)
	}
	reg.SetKeyStore(&fakeKeyStore{keys: map[string]string{}})

	if key := reg.APIKey("deepseek-chat"); key != "" {
		t.Errorf("无密钥时应返回空，实际: %q", key)
	}
	if source := reg.APIKeySource("deepseek-chat"); source != KeySourceNone {
		t.Errorf("来源应为 none，实际: %q", source)
	}
	if source := reg.APIKeySource("not-exist"); source != KeySourceNone {
		t.Errorf("不存在的端点来源应为 none，实际: %q", source)
	}
}

// TestAPIKeyKeyStoreErrorDegrades 验证托管库查询出错按未命中降级（返回空），
// 不拖垮模型调用链。
func TestAPIKeyKeyStoreErrorDegrades(t *testing.T) {
	reg, err := Load(testLLMConfig())
	if err != nil {
		t.Fatalf("Load 不应返回错误: %v", err)
	}
	reg.SetKeyStore(&fakeKeyStore{err: errors.New("db down")})

	if key := reg.APIKey("deepseek-chat"); key != "" {
		t.Errorf("托管库出错时应降级返回空，实际: %q", key)
	}
}

// TestSetKeyStoreNilRestoresEnvOnly 验证 SetKeyStore(nil) 恢复仅 env 解析，
// 已缓存的托管密钥不再生效。
func TestSetKeyStoreNilRestoresEnvOnly(t *testing.T) {
	reg, err := Load(testLLMConfig())
	if err != nil {
		t.Fatalf("Load 不应返回错误: %v", err)
	}
	reg.SetKeyStore(&fakeKeyStore{keys: map[string]string{"deepseek-chat": "sk-managed"}})
	if key := reg.APIKey("deepseek-chat"); key != "sk-managed" {
		t.Fatalf("注入后应读到 sk-managed，实际: %q", key)
	}

	reg.SetKeyStore(nil)
	if key := reg.APIKey("deepseek-chat"); key != "" {
		t.Errorf("移除 KeyStore 后应仅 env 解析（返回空），实际: %q", key)
	}
}

// TestInvalidateKeyCache 验证托管密钥变更经 InvalidateKeyCache 后可见
// （settings REST 写入/删除 key 后调用同一入口）。
func TestInvalidateKeyCache(t *testing.T) {
	ks := &fakeKeyStore{keys: map[string]string{"deepseek-chat": "sk-old"}}
	reg, err := Load(testLLMConfig())
	if err != nil {
		t.Fatalf("Load 不应返回错误: %v", err)
	}
	reg.SetKeyStore(ks)
	if key := reg.APIKey("deepseek-chat"); key != "sk-old" {
		t.Fatalf("预热时应读到 sk-old，实际: %q", key)
	}

	ks.keys["deepseek-chat"] = "sk-new"
	if key := reg.APIKey("deepseek-chat"); key != "sk-old" {
		t.Fatalf("缓存未失效前应仍读 sk-old，实际: %q", key)
	}
	reg.InvalidateKeyCache()
	if key := reg.APIKey("deepseek-chat"); key != "sk-new" {
		t.Errorf("缓存失效后应读到 sk-new，实际: %q", key)
	}
}
