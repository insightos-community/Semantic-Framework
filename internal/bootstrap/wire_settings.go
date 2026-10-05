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

package bootstrap

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/server/http/handlers"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/llm"
	"insightos.cn/semantic-framework/pkg/log"
)

// configFileHeader 是 PATCH 写回配置文件时写入的头部注释：
// 文件自此由 settings API 机器重写，原手写注释不保留（见配置参考文档）。
const configFileHeader = "# 本文件由 settings API（PATCH /api/v1/settings）重写，原有手写注释不保留。\n"

// keyStoreAdapter 把 store 的 ErrNotFound 语义适配为 llm.KeyStore 约定的
// ("", nil)，使注册表的"env > 托管密钥库"解析链接管 store 实现。
type keyStoreAdapter struct {
	// st 元数据存储（settings_keys 表）。
	st *store.Store
}

// GetKey 按端点名读取托管密钥，不存在时返回 ("", nil)。
func (a keyStoreAdapter) GetKey(name string) (string, error) {
	value, err := a.st.GetKey(name)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	return value, err
}

// settingsController 实现 handlers.ConfigPatcher：GET 快照取自"当前生效
// 配置"（热重载快照，未启用热重载时为启动配置）；PATCH 在单把互斥锁内
// 完成乐观锁比对 → merge patch → schema/语义校验 → 写回文件 → 白名单
// 热应用，保证并发 PATCH 只有一个生效（另一个必然 409）。
type settingsController struct {
	// app 持有 reloader/cfg 等运行态（currentConfig 的唯一事实源）。
	app *App

	// logger 结构化日志器。
	logger *log.Logger

	// mu 串行化 Patch（check+apply 原子），同时保护 configPath。
	mu sync.Mutex

	// configPath 启动加载的配置文件路径（PATCH 写回目标），由
	// EnableConfigReload 在启动期单线程装配；空表示写回能力未启用。
	configPath string
}

// newSettingsController 创建设置控制器。
func newSettingsController(app *App, logger *log.Logger) *settingsController {
	return &settingsController{app: app, logger: logger}
}

// ConfigPath 返回 Server 启动时传给 EnableConfigReload 的配置文件路径。
// 路径在开始服务前固定，设置 API 只读展示并始终向同一文件写回。
func (c *settingsController) ConfigPath() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.configPath
}

// CurrentTree 返回当前生效配置树与其 base_hash。
func (c *settingsController) CurrentTree() (map[string]any, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return treeAndHash(c.app.currentConfig())
}

// Patch 对当前生效快照应用 merge patch：baseHash 乐观锁不一致拒绝（409）；
// schema 校验复用 R1 validate（400 带位置）；语义校验（llm 结构/component
// 白名单/profiles_dir 预加载）与热应用钩子同源，保证"校验通过即热应用成功"。
// 通过后先写回配置文件（临时文件 + rename 原子替换），再经 R1 白名单路径
// 热应用（ApplyExternal），返回新快照树、新 base_hash 与变更键清单。
func (c *settingsController) Patch(baseHash string, patch map[string]any) (map[string]any, string, []string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.configPath == "" {
		return nil, "", nil, &handlers.PatchReject{
			Status: http.StatusServiceUnavailable, Code: handlers.CodeSettingsUnavailable,
			Message: "配置写回未启用（热重载未装配），仅支持 GET",
		}
	}

	cur := c.app.currentConfig()
	tree, hash, err := treeAndHash(cur)
	if err != nil {
		return nil, "", nil, err
	}
	if hash != baseHash {
		return nil, "", nil, &handlers.PatchReject{
			Status: http.StatusConflict, Code: handlers.CodeSettingsConflict,
			Message: "配置快照已被并发修改，请重新 GET 获取最新 base_hash 后重试",
		}
	}

	merged := config.MergeTree(tree, patch)
	materializeProviderDefaults(tree, merged)
	next, err := config.DecodeTree(merged)
	if err != nil {
		var verr *config.ValidationError
		if errors.As(err, &verr) {
			return nil, "", nil, &handlers.PatchReject{
				Status: http.StatusBadRequest, Code: handlers.CodeBadRequest, Message: verr.Error(),
			}
		}
		return nil, "", nil, fmt.Errorf("解码合并后的配置失败: %w", err)
	}

	if reject := c.validateSemantics(cur, next); reject != nil {
		return nil, "", nil, reject
	}

	// 写回采用"重写生成"策略：整树重新序列化覆盖，原手写注释不保留
	// （代价换取可靠——yaml 注释级编辑极易踩坑，见 changelog 说明）。
	data, err := nextYAML(merged)
	if err != nil {
		return nil, "", nil, err
	}
	if err := writeConfigFile(c.configPath, data); err != nil {
		return nil, "", nil, fmt.Errorf("写回配置文件失败: %w", err)
	}

	if err := c.app.reloader.ApplyExternal(next); err != nil {
		// 文件已写回但热应用失败：reloader 已尝试回滚运行态，如实返回错误。
		return nil, "", nil, fmt.Errorf("配置已写回 %s，但热应用失败: %w", c.configPath, err)
	}

	newTree, newHash, err := treeAndHash(c.app.currentConfig())
	if err != nil {
		return nil, "", nil, err
	}
	return newTree, newHash, config.PatchPaths(patch), nil
}

// validateSemantics 做与热应用钩子同源的语义预校验（失败返回 400）：
// llm 结构（default 在清单/component/model 必填）+ component 白名单 +
// MCP server 结构校验。只要 LLM 清单或 profiles_dir 发生变化，就扫描
// 全部 Agent profile 的主模型和深度模型引用，防止前端删除一个仍被 Agent
// 使用的端点，留下“设置页保存成功、下一次启动 Agent 才失败”的悬空引用。
func (c *settingsController) validateSemantics(cur, next *config.Config) *handlers.PatchReject {
	reject := func(err error) *handlers.PatchReject {
		return &handlers.PatchReject{
			Status: http.StatusBadRequest, Code: handlers.CodeBadRequest, Message: err.Error(),
		}
	}

	nextReg, err := llm.Load(next.LLM)
	if err != nil {
		return reject(fmt.Errorf("llm 配置校验失败: %w", err))
	}
	if err := checkLLMComponents(next.LLM); err != nil {
		return reject(err)
	}
	// options 键级预校验：非法键在保存时 400，而不是等第一次会话构建模型才失败。
	for name, p := range next.LLM.Providers {
		if err := kernel.ValidateOptions(p.Component, name, p.Options); err != nil {
			return reject(err)
		}
	}
	// mcp_servers 结构校验与热应用钩子同源（重名/非法 risk 拒绝为 400，
	// 而不是写回后热应用失败报 500）。
	if err := validateMCPServers(next.MCPServers); err != nil {
		return reject(fmt.Errorf("mcp_servers 配置校验失败: %w", err))
	}

	if next.Agents.ProfilesDir != cur.Agents.ProfilesDir || !reflect.DeepEqual(next.LLM, cur.LLM) {
		if err := validateAgentModelReferences(next.Agents.ProfilesDir, nextReg); err != nil {
			return reject(err)
		}
	}
	return nil
}

// validateAgentModelReferences 校验 profilesDir 下所有包含 role.yaml 的 Agent。
// 目录中的 SAFETY.md、teams 等辅助资源没有 role.yaml，会被明确跳过；这样既
// 覆盖当前可创建的全部 Agent，又不会把非 profile 子目录误判为损坏配置。
func validateAgentModelReferences(profilesDir string, registry *llm.Registry) error {
	entries, err := os.ReadDir(profilesDir)
	if err != nil {
		return fmt.Errorf("读取 Agent profile 目录失败: %w", err)
	}

	loader := profile.NewLoader(profilesDir)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		rolePath := filepath.Join(profilesDir, entry.Name(), "role.yaml")
		if _, err := os.Stat(rolePath); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("检查 Agent %q 的 role.yaml 失败: %w", entry.Name(), err)
		}

		prof, err := loader.Load(entry.Name())
		if err != nil {
			return fmt.Errorf("加载 Agent %q 的 profile 失败: %w", entry.Name(), err)
		}
		if err := validateModelReference(registry, prof.Name, "model", prof.Model); err != nil {
			return err
		}
	}
	return nil
}

// validateModelReference 校验 Agent 的模型引用，保证端点删除不会留下
// 无法装配的角色配置。
func validateModelReference(registry *llm.Registry, agentName, field, modelName string) error {
	// 空 model 是 Agent Profile 的正式继承语义，不是名为 "" 的端点。
	// llm.Load 已保证系统 Default 存在，因此这里无需再次校验空引用；后续
	// Runtime 会在创建会话快照时把当时的 Default 固化为实际 endpoint。
	if strings.TrimSpace(modelName) == "" {
		return nil
	}
	if _, err := registry.Get(modelName); err != nil {
		return fmt.Errorf(
			"Agent %q 的 %s 引用了不存在的 LLM 端点 %q，请先调整 Agent 配置再删除端点: %w",
			agentName, field, modelName, err,
		)
	}
	return nil
}

// materializeProviderDefaults 为本次 PATCH 新增的 OpenAI 兼容端点物化默认
// options（timeout_seconds/max_tokens）。为什么在生成时物化：让默认保护在
// 配置文件和设置 UI 中可见、可按端点覆盖，而不是隐式藏在模型构建层。
// 规则：
//   - 只处理新增端点（cur 树中不存在），已有端点的 options 不被触碰；
//   - patch 中显式携带 options 键（即使为空 map）时完全尊重用户，不注入；
//   - 只物化 openai 驱动：claude 的 options 白名单不含 timeout_seconds，
//     注入反而会打挂模型构建；mock 不发起真实请求，无需保护。
func materializeProviderDefaults(cur, merged map[string]any) {
	for name, raw := range treeProviders(merged) {
		provider, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, exists := treeProviders(cur)[name]; exists {
			continue
		}
		if component, _ := provider["component"].(string); component != kernel.ComponentOpenAI {
			continue
		}
		if _, explicit := provider["options"]; explicit {
			continue
		}
		provider["options"] = map[string]any{
			"timeout_seconds": kernel.DefaultModelRequestTimeoutSeconds,
			"max_tokens":      kernel.DefaultMaxOutputTokens,
		}
	}
}

// treeProviders 安全取出配置树中的 llm.providers 映射；结构不存在时返回空表。
func treeProviders(tree map[string]any) map[string]any {
	llmSection, ok := tree["llm"].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	providers, ok := llmSection["providers"].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return providers
}

// treeAndHash 从配置快照导出键值树并计算 base_hash。
func treeAndHash(cfg *config.Config) (map[string]any, string, error) {
	tree, err := config.Tree(cfg)
	if err != nil {
		return nil, "", err
	}
	hash, err := config.TreeHash(tree)
	if err != nil {
		return nil, "", err
	}
	return tree, hash, nil
}

// nextYAML 序列化合并后的配置树为写回内容（头部注释 + yaml 正文）。
func nextYAML(merged map[string]any) ([]byte, error) {
	body, err := yaml.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("序列化合并后的配置失败: %w", err)
	}
	return append([]byte(configFileHeader), body...), nil
}

// writeConfigFile 原子写回配置文件：先写同目录临时文件再 rename，
// 避免进程崩溃留下半写状态的配置文件（热重载/重启都会 fail-closed 拒绝）。
func writeConfigFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
