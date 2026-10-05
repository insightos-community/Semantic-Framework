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
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"insightos.cn/semantic-framework/pkg/config"
)

// apiKeyEnvPrefix 是 LLM 密钥环境变量的统一前缀（docs/architecture/02 §5）。
const apiKeyEnvPrefix = "SEMANTIC_LLM_API_KEY_"

// Provider 是注册表中一个已解析的模型端点，字段与配置一一对应。
type Provider struct {
	// Name 端点名（providers map 的键），即端点的唯一标识。
	Name string

	// Service 模型服务标识；旧配置未声明时回退 Name。
	Service string

	// Component 驱动（内核组件）：openai | mock。
	Component string

	// BaseURL OpenAI 兼容端点地址；同一 base_url 的条目共享同一把 api_key。
	BaseURL string

	// Model 端点上的模型 ID。
	Model string

	// Capabilities 能力标签：text / image / tool_call / embedding。
	Capabilities []string

	// Options 调用默认参数（直通内核模型配置）。
	Options map[string]any

	// Price 每 1K tokens 单价，计量估算用。
	Price Price
}

// Price 定义每 1K tokens 的单价。
type Price struct {
	// Prompt 输入侧每 1K tokens 单价。
	Prompt float64

	// Completion 输出侧每 1K tokens 单价。
	Completion float64
}

// APIKeyEnv 返回指定端点的密钥环境变量名：
// 名称大写、'-' 转 '_'，如 deepseek-chat → SEMANTIC_LLM_API_KEY_DEEPSEEK_CHAT。
func APIKeyEnv(name string) string {
	return apiKeyEnvPrefix + strings.ReplaceAll(strings.ToUpper(name), "-", "_")
}

// KeySource 标记 API key 的解析来源，供审计与测试断言使用。
type KeySource string

// 密钥解析来源的取值：env（进程环境变量，含 .env 注入）/
// store（服务端托管密钥库）/ none（未解析到）。
const (
	// KeySourceNone 未解析到密钥。
	KeySourceNone KeySource = "none"

	// KeySourceEnv 密钥来自进程环境变量（含 .env 注入）。
	KeySourceEnv KeySource = "env"

	// KeySourceStore 密钥来自服务端托管密钥库。
	KeySourceStore KeySource = "store"
)

// KeyStore 是服务端托管密钥的读取接口，由 internal/store 实现、
// bootstrap 装配时注入（pkg/llm 不反向依赖 internal，保持 pkg 层纯净）。
// 约定：密钥不存在时返回 ("", nil)；其他错误由注册表按"未命中"降级处理。
type KeyStore interface {
	// GetKey 按端点名读取托管密钥。
	GetKey(name string) (string, error)
}

// keyEntry 是密钥缓存条目：按端点名缓存解析结果与来源。
type keyEntry struct {
	// value 解析到的密钥（空串表示未解析到，同样缓存避免重复查询）。
	value string

	// source 解析来源。
	source KeySource
}

// Registry 是 LLM 提供方注册表：Load 时从配置快照端点清单，
// 之后的查询都是只读；密钥按"env > 托管密钥库"顺序解析并按端点名缓存。
// Reload 支持配置热重载：校验通过的新快照原子替换旧快照并清空密钥缓存。
type Registry struct {
	// mu 保护 providers/defaultName/keyCache/keyStore（Reload 会整体替换）。
	mu sync.RWMutex

	// providers 端点清单（名称 → 端点）。
	providers map[string]Provider

	// defaultName 全局默认模型名。
	defaultName string

	// keyStore 服务端托管密钥库（nil 表示仅 env 解析）。
	keyStore KeyStore

	// keyCache 按端点名缓存密钥解析结果（含来源标记）：
	// 避免重复读环境变量与查询密钥库；Reload/InvalidateKeyCache 清空。
	keyCache map[string]keyEntry
}

// Load 从配置构建注册表。校验规则见 buildProviders。
func Load(cfg config.LLMConfig) (*Registry, error) {
	providers, err := buildProviders(cfg)
	if err != nil {
		return nil, err
	}
	return &Registry{
		providers:   providers,
		defaultName: cfg.Default,
		keyCache:    make(map[string]keyEntry),
	}, nil
}

// SetKeyStore 注入服务端托管密钥库并清空密钥缓存（解析链路随之生效）。
// 传 nil 恢复为仅 env 解析。
func (r *Registry) SetKeyStore(ks KeyStore) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keyStore = ks
	r.keyCache = make(map[string]keyEntry)
}

// InvalidateKeyCache 清空密钥缓存：托管密钥写入/删除后调用，
// 下次 APIKey 查询重新走完整解析链。
func (r *Registry) InvalidateKeyCache() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keyCache = make(map[string]keyEntry)
}

// Reload 用新配置原子替换端点快照：校验全部通过才替换并清空密钥缓存
// （API key 可能随 env 变化，必须重读）；校验失败时旧快照继续生效。
func (r *Registry) Reload(cfg config.LLMConfig) error {
	providers, err := buildProviders(cfg)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers = providers
	r.defaultName = cfg.Default
	r.keyCache = make(map[string]keyEntry)
	return nil
}

// buildProviders 校验并构建端点清单：default 必须存在于 providers；
// 每个端点 component 与 model 必填。base_url 不强制（mock 无端点地址），
// 由内核适配层按 component 各自校验。
func buildProviders(cfg config.LLMConfig) (map[string]Provider, error) {
	if len(cfg.Providers) == 0 {
		return nil, fmt.Errorf("llm.providers 为空：至少需要一个模型端点")
	}
	if _, ok := cfg.Providers[cfg.Default]; !ok {
		return nil, fmt.Errorf("llm.default %q 不在 providers 清单中", cfg.Default)
	}

	providers := make(map[string]Provider, len(cfg.Providers))
	for name, p := range cfg.Providers {
		if p.Component == "" {
			return nil, fmt.Errorf("llm.providers.%s.component 不能为空", name)
		}
		if p.Model == "" {
			return nil, fmt.Errorf("llm.providers.%s.model 不能为空", name)
		}
		providers[name] = Provider{
			Name:         name,
			Service:      firstNonEmpty(p.Service, name),
			Component:    p.Component,
			BaseURL:      p.BaseURL,
			Model:        p.Model,
			Capabilities: p.Capabilities,
			Options:      p.Options,
			Price:        Price{Prompt: p.Price.Prompt, Completion: p.Price.Completion},
		}
	}
	return providers, nil
}

func firstNonEmpty(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

// ServiceNames 返回已配置的模型服务 ID（去重升序）。
func (r *Registry) ServiceNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := make(map[string]struct{})
	for _, provider := range r.providers {
		seen[provider.Service] = struct{}{}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// HasService 报告服务 ID 是否存在。
func (r *Registry) HasService(name string) bool {
	for _, service := range r.ServiceNames() {
		if service == name {
			return true
		}
	}
	return false
}

// Get 按名称查询端点，不存在时返回错误。
func (r *Registry) Get(name string) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	if !ok {
		return Provider{}, fmt.Errorf("模型端点 %q 不存在于 llm.providers", name)
	}
	return p, nil
}

// Default 返回全局默认端点（Load 已校验其存在）。
func (r *Registry) Default() Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.providers[r.defaultName]
}

// Names 返回全部端点名（升序），供 doctor 等需要逐项遍历的消费方使用。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sortedNamesLocked()
}

// sortedNamesLocked 返回升序端点名；调用方须已持有 r.mu（读或写）。
func (r *Registry) sortedNamesLocked() []string {
	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// APIKey 读取端点的密钥，解析顺序：进程环境变量（含 .env 注入，
// 同一 base_url 的条目共享一把 key）> 服务端托管密钥库（KeyStore，
// 未注入时跳过）。结果按端点名缓存；端点不存在或未配置密钥时返回空字符串。
func (r *Registry) APIKey(name string) string {
	key, _ := r.apiKey(name)
	return key
}

// APIKeySource 返回端点密钥的解析来源（env/store/none），供审计与测试断言。
func (r *Registry) APIKeySource(name string) KeySource {
	_, source := r.apiKey(name)
	return source
}

// apiKey 是密钥解析的统一入口：先查按端点名的缓存，未命中走完整解析链。
func (r *Registry) apiKey(name string) (string, KeySource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.providers[name]
	if !ok {
		return "", KeySourceNone
	}
	if entry, ok := r.keyCache[name]; ok {
		return entry.value, entry.source
	}

	// ① 进程 env：本端点的环境变量。
	key := os.Getenv(APIKeyEnv(p.Service))
	if key == "" {
		key = os.Getenv(APIKeyEnv(name))
	}
	if key == "" {
		// 同一 base_url 的条目共享一把 key：按名称序取第一个已设置的，
		// 顺序确定，避免 map 遍历的随机性。
		for _, sibling := range r.sortedNamesLocked() {
			if sibling == name || r.providers[sibling].BaseURL != p.BaseURL {
				continue
			}
			if k := os.Getenv(APIKeyEnv(sibling)); k != "" {
				key = k
				break
			}
		}
	}
	if key != "" {
		r.keyCache[name] = keyEntry{value: key, source: KeySourceEnv}
		return key, KeySourceEnv
	}

	// ② 服务端托管密钥库兜底：GetKey 出错按未命中降级（返回空），
	// 存储故障不拖垮模型调用链（缺 key 的 WARN 巡检会暴露）。
	if r.keyStore != nil {
		k, err := r.keyStore.GetKey(p.Service)
		if (err != nil || k == "") && p.Service != name {
			k, err = r.keyStore.GetKey(name)
		}
		if err == nil && k != "" {
			r.keyCache[name] = keyEntry{value: k, source: KeySourceStore}
			return k, KeySourceStore
		}
	}

	r.keyCache[name] = keyEntry{value: "", source: KeySourceNone}
	return "", KeySourceNone
}
