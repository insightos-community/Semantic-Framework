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

package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// loginRequest 是登录接口的请求体。
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// tokenResponse 是登录/刷新接口的响应体。
type tokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Handlers 提供认证相关的 HTTP 处理器，只做协议适配：
// 解析请求、调用 Service、写统一格式响应，不含认证逻辑本身。
type Handlers struct {
	// svc 认证服务。
	svc *Service
}

// NewHandlers 创建认证 HTTP 处理器。
func NewHandlers(svc *Service) *Handlers {
	return &Handlers{svc: svc}
}

// HandleLogin 处理 POST /api/v1/auth/login：校验账号密码，签发 token。
func (h *Handlers) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体必须为合法 JSON")
		return
	}
	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "username 与 password 不能为空")
		return
	}

	token, err := h.svc.Login(req.Username, req.Password)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	h.writeToken(w, token)
}

// HandleRefresh 处理 POST /api/v1/auth/refresh：旧 token（Bearer）换新 token。
// 该端点受鉴权中间件保护，到达此处时 token 已通过一次校验。
func (h *Handlers) HandleRefresh(w http.ResponseWriter, r *http.Request) {
	newToken, err := h.svc.Refresh(bearerToken(r))
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	h.writeToken(w, newToken)
}

// HandleLogout 处理 POST /api/v1/auth/logout：注销当前 Bearer token。
func (h *Handlers) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Logout(bearerToken(r)); err != nil {
		h.svc.logger.WithError(err).Error("登出处理失败")
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// writeToken 查询 token 的过期时间并写出统一令牌响应。
func (h *Handlers) writeToken(w http.ResponseWriter, token string) {
	t, err := h.svc.store.GetToken(token)
	if err != nil {
		h.svc.logger.WithError(err).Error("回读 token 失败")
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务内部错误")
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse{Token: token, ExpiresAt: t.ExpiresAt})
}

// writeServiceError 将 Service 错误转换为 HTTP 响应：
// 认证错误（*Error）按 code 透传为 401，其余内部错误统一 500。
func (h *Handlers) writeServiceError(w http.ResponseWriter, err error) {
	var aerr *Error
	if errors.As(err, &aerr) {
		writeError(w, http.StatusUnauthorized, aerr.Code, aerr.Message)
		return
	}
	h.svc.logger.WithError(err).Error("认证处理内部错误")
	writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务内部错误")
}

// writeJSON 以 JSON 格式写出响应体。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
