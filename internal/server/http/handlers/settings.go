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
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/llm"
	"insightos.cn/semantic-framework/pkg/log"
)

// 设置域的错误码，与 HTTP 响应体 error.code 一致。
const (
	// CodeSettingsConflict base_hash 与当前快照不一致（乐观锁冲突）。
	CodeSettingsConflict = "SETTINGS_CONFLICT"

	// CodeSettingsKeyNotFound 要删除的托管密钥不存在。
	CodeSettingsKeyNotFound = "SETTINGS_KEY_NOT_FOUND"

	// CodeSettingsUnavailable 配置写回能力未启用（热重载未装配）。
	CodeSettingsUnavailable = "SETTINGS_UNAVAILABLE"
)

// 设置变更审计的动作名（settings_audit.action）。
const (
	auditActionSettingsPatch = "settings.patch"
	auditActionKeySet        = "settings.key_set"
	auditActionKeyDelete     = "settings.key_delete"
)

// minManagedKeyLength 是托管密钥的最小长度（防误写占位符）。
const minManagedKeyLength = 8

// ConfigPatcher 抽象 settings PATCH 的配置读写能力，由 bootstrap 装配时注入
// 实现（乐观锁比对、merge patch、schema/语义校验、写回文件、白名单热应用
// 都在实现侧原子完成）。handler 只负责 HTTP 语义映射，不触碰配置细节。
type ConfigPatcher interface {
	// ConfigPath 返回当前实例启动时解析出的唯一配置读写目标。
	ConfigPath() string

	// CurrentTree 返回当前生效配置树与其 base_hash。
	CurrentTree() (tree map[string]any, hash string, err error)

	// Patch 先比对 baseHash 乐观锁，再合并 patch、校验、写回并热应用，
	// 返回新配置树、新 base_hash 与变更键清单（叶子路径，不含值）。
	// 拒绝时返回 *PatchReject；其他错误为内部错误。
	Patch(baseHash string, patch map[string]any) (tree map[string]any, hash string, changed []string, err error)
}

// PatchReject 描述 PATCH 被乐观锁或校验拒绝的原因：
// handler 按字段直接映射为 HTTP 错误响应（Status/Code/Message）。
type PatchReject struct {
	// Status HTTP 状态码（400 校验失败 / 409 乐观锁冲突 / 503 写回未启用）。
	Status int

	// Code 统一错误码（error.code）。
	Code string

	// Message 可读错误描述（校验失败含全部问题位置）。
	Message string
}

// Error 返回拒绝原因的可读描述。
func (e *PatchReject) Error() string {
	return e.Message
}

// SettingsHandler 是设置域的 REST 处理器：生效配置快照查询（掩码）、
// 配置 PATCH（乐观锁 + 热应用）与托管密钥管理。
// 全部端点在 auth 中间件之后（受保护路由组），审计只记键清单不记值。
type SettingsHandler struct {
	// st 元数据存储（托管密钥与审计表）。
	st *store.Store

	// registry LLM 注册表：校验密钥对应的端点存在、密钥变更后清缓存。
	registry *llm.Registry

	// patcher 配置读写能力（bootstrap 注入）。
	patcher ConfigPatcher

	// logger 结构化日志器。
	logger *log.Logger
}

// NewSettingsHandler 创建设置域处理器。
func NewSettingsHandler(st *store.Store, registry *llm.Registry, patcher ConfigPatcher, logger *log.Logger) *SettingsHandler {
	return &SettingsHandler{st: st, registry: registry, patcher: patcher, logger: logger}
}

// patchSettingsRequest 是 PATCH /settings 的请求体。
type patchSettingsRequest struct {
	// BaseHash 客户端持有的快照哈希（GET 响应的 base_hash），乐观锁。
	BaseHash string `json:"base_hash"`

	// Patch JSON merge patch（RFC 7386），键名与配置文件一致（yaml 键名）。
	Patch map[string]any `json:"patch"`
}

// putKeyRequest 是 PUT /settings/keys/{name} 的请求体。
type putKeyRequest struct {
	// KeyValue 密钥明文（仅在本请求体内出现，不落日志/审计）。
	KeyValue string `json:"key_value"`
}

// keyView 是托管密钥的响应视图（值已掩码）。
type keyView struct {
	Name      string    `json:"name"`
	KeyValue  string    `json:"key_value"`
	UpdatedAt time.Time `json:"updated_at"`
}

// HandleGetSettings 处理 GET /api/v1/settings：返回当前生效配置树
// （敏感值掩码）与 base_hash（PATCH 乐观锁的凭据）。
// runtime_defaults 与配置树平行，只供设置页展示运行时兜底，不进 yaml、
// 也不出现在 PATCH 请求或响应里。
func (h *SettingsHandler) HandleGetSettings(w http.ResponseWriter, r *http.Request) {
	tree, hash, err := h.patcher.CurrentTree()
	if err != nil {
		h.logger.WithError(err).Error("读取配置快照失败")
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	// 读取日志不含任何配置值。
	h.logger.Debug("设置快照已读取", "user_id", auth.UserIDFromContext(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":         MaskTree(tree),
		"base_hash":        hash,
		"config_path":      h.patcher.ConfigPath(),
		"key_sources":      h.keySources(),
		"runtime_defaults": kernel.RuntimeDefaults(),
	})
}

// HandlePatchSettings 处理 PATCH /api/v1/settings：base_hash 乐观锁 +
// merge patch → 校验 → 写回配置文件 → 白名单热应用 → 审计 → 返回新快照。
func (h *SettingsHandler) HandlePatchSettings(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())

	var req patchSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "请求体不是合法 JSON")
		return
	}
	if req.BaseHash == "" {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "base_hash 不能为空")
		return
	}
	if len(req.Patch) == 0 {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "patch 不能为空对象")
		return
	}

	tree, hash, changed, err := h.patcher.Patch(req.BaseHash, req.Patch)
	if err != nil {
		var reject *PatchReject
		if errors.As(err, &reject) {
			writeError(w, reject.Status, reject.Code, reject.Message)
			return
		}
		h.logger.WithError(err).Error("配置 PATCH 内部错误", "user_id", userID)
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}

	// 审计只记变更键清单，绝不记值；审计写入失败不回滚已生效的变更，记 ERROR。
	h.audit(userID, auditActionSettingsPatch, strings.Join(changed, ","))
	h.logger.Info("配置 PATCH 已生效", "user_id", userID, "changed", changed)
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":    MaskTree(tree),
		"base_hash":   hash,
		"config_path": h.patcher.ConfigPath(),
		"key_sources": h.keySources(),
		"changed":     changed,
	})
}

// keySources 返回每个模型端点当前实际采用的密钥来源。这里只返回
// env/store/none 枚举，不返回密钥值；前端据此提示环境变量是否覆盖了托管密钥。
func (h *SettingsHandler) keySources() map[string]llm.KeySource {
	sources := make(map[string]llm.KeySource, len(h.registry.Names()))
	for _, name := range h.registry.Names() {
		sources[name] = h.registry.APIKeySource(name)
	}
	return sources
}

// HandleListKeys 处理 GET /api/v1/settings/keys：列出全部托管密钥
// （名称升序，值掩码，含更新时间）。
func (h *SettingsHandler) HandleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.st.ListKeys()
	if err != nil {
		h.logger.WithError(err).Error("查询托管密钥清单失败")
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	views := make([]keyView, 0, len(keys))
	for _, k := range keys {
		views = append(views, keyView{Name: k.Name, KeyValue: MaskSecret(k.Value), UpdatedAt: k.UpdatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": views})
}

// HandlePutKey 处理 PUT /api/v1/settings/keys/{name}：写入（或轮换）一把
// 托管密钥。name 优先是模型服务 ID；旧端点名仍兼容；写库后清空缓存
// 使新 key 即时生效，并写审计（只记名称）。
func (h *SettingsHandler) HandlePutKey(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	name := chi.URLParam(r, "name")

	var req putKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "请求体不是合法 JSON")
		return
	}
	if len(req.KeyValue) < minManagedKeyLength {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "key_value 不能为空且长度至少 8")
		return
	}
	if !h.registry.HasService(name) {
		if _, err := h.registry.Get(name); err != nil {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "对应模型服务不存在: "+name)
			return
		}
	}

	if err := h.st.SetKey(name, req.KeyValue); err != nil {
		h.logger.WithError(err).Error("写入托管密钥失败", "provider", name)
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	h.registry.InvalidateKeyCache()

	// 审计与日志只记名称，绝不记值。
	h.audit(userID, auditActionKeySet, name)
	h.logger.Info("托管密钥已写入", "provider", name, "user_id", userID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// HandleDeleteKey 处理 DELETE /api/v1/settings/keys/{name}：删除一把托管
// 密钥（不存在返回 404），删库后清注册表缓存并写审计（只记名称）。
func (h *SettingsHandler) HandleDeleteKey(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	name := chi.URLParam(r, "name")

	if err := h.st.DeleteKey(name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, CodeSettingsKeyNotFound, "托管密钥不存在: "+name)
			return
		}
		h.logger.WithError(err).Error("删除托管密钥失败", "provider", name)
		writeError(w, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	h.registry.InvalidateKeyCache()

	h.audit(userID, auditActionKeyDelete, name)
	h.logger.Info("托管密钥已删除", "provider", name, "user_id", userID)
	w.WriteHeader(http.StatusNoContent)
}

// audit 写一条设置审计；失败只记 ERROR（配置/密钥变更已生效，不回滚）。
func (h *SettingsHandler) audit(userID, action, detail string) {
	if err := h.st.InsertAudit(store.AuditEntry{UserID: userID, Action: action, Detail: detail}); err != nil {
		h.logger.WithError(err).Error("设置审计写入失败", "action", action)
	}
}

// sensitiveKeySubstrings 命中即掩码的键名片段（小写包含比对）：
// 任何键名含 key/password/token 的字段都被视为敏感值。
var sensitiveKeySubstrings = []string{"key", "password", "token"}

// MaskTree 返回敏感值已掩码的配置树副本（不修改输入树）：
// 任何键名含 key/password/token（大小写不敏感）的字段值被替换为
// MaskSecret 结果；列表中的 "K=V" 赋值式字符串（mcp_servers[].env）
// 按 K 判定掩码 V；其余值原样保留，嵌套映射与列表递归处理。
// 掩码是纯展示层函数——服务端内存中的配置树始终持有原值。
func MaskTree(tree map[string]any) map[string]any {
	masked := make(map[string]any, len(tree))
	for k, v := range tree {
		masked[k] = maskEntry(k, v)
	}
	return masked
}

// maskEntry 按键名决定单个值的掩码策略。
func maskEntry(key string, value any) any {
	if isSensitiveKey(key) {
		switch v := value.(type) {
		case string:
			return MaskSecret(v)
		case int, int64, float64:
			// 数值不可能是凭据：如 options.max_tokens 键名含 token，
			// 掩码会让正常调用参数在设置 UI 中不可读、不可编辑。
			return v
		default:
			return "***" // 其余非字符串敏感值无"前 6 字符"可取，整体掩码
		}
	}
	switch v := value.(type) {
	case map[string]any:
		return MaskTree(v)
	case []any:
		masked := make([]any, len(v))
		for i, item := range v {
			// 列表元素无键名：嵌套映射递归处理；"K=V" 赋值式字符串
			// （mcp_servers[].env）按 K 判定后掩码 V。
			switch item := item.(type) {
			case map[string]any:
				masked[i] = MaskTree(item)
			case string:
				masked[i] = maskEnvAssignment(item)
			default:
				masked[i] = item
			}
		}
		return masked
	default:
		return value
	}
}

// maskEnvAssignment 掩码 "K=V" 赋值式字符串中的敏感值部分：
// K 命中敏感片段时 V 替换为 MaskSecret 结果，其余原样保留。
// 用于 mcp_servers[].env 这类内嵌赋值的列表元素（元素自身无键名可比对）。
func maskEnvAssignment(s string) string {
	k, v, ok := strings.Cut(s, "=")
	if !ok || v == "" || !isSensitiveKey(k) {
		return s
	}
	return k + "=" + MaskSecret(v)
}

// isSensitiveKey 判定键名是否命中敏感片段（key/password/token）。
func isSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, sub := range sensitiveKeySubstrings {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return false
}

// MaskSecret 掩码敏感值：保留前 6 字符后以 "***" 结尾
// （如 sk-abcdef123 → sk-abc***）；不足 6 字符整体掩码为 "***"。
func MaskSecret(value string) string {
	const keep = 6
	if len(value) <= keep {
		return "***"
	}
	return value[:keep] + "***"
}
