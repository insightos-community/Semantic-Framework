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
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// publicPaths 是免鉴权路径白名单：登录端点与系统只读探活端点。
// 采用精确匹配而非前缀匹配，避免误放行（如 /api/v1/auth/login/xxx）。
var publicPaths = map[string]struct{}{
	"/api/v1/auth/login":              {},
	"/api/v1/pilot-enrollments/claim": {},
	"/api/v1/system/healthz":          {},
	"/api/v1/system/version":          {},
}

// userIDKey 是 user_id 在 request context 中的键类型，未导出以防冲突。
type userIDKey struct{}
type pilotIDKey struct{}

// ContextWithUserID 将用户 ID 注入 context，供下游 handler 提取。
func ContextWithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userIDKey{}, userID)
}

// UserIDFromContext 从 context 提取用户 ID，不存在时返回空字符串。
func UserIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(userIDKey{}).(string); ok {
		return id
	}
	return ""
}

func PilotIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(pilotIDKey{}).(string); ok {
		return id
	}
	return ""
}

// Middleware 返回 HTTP 鉴权中间件：白名单路径直接放行，
// 其余路径校验 Authorization: Bearer <token>，通过后将 user_id 注入
// request context；失败返回 401 与统一错误格式。
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/pilot/v1/transfers/") {
			credential, err := s.store.GetPilotCredential(bearerToken(r))
			if err != nil || credential.RevokedAt != nil {
				writeError(w, http.StatusUnauthorized, "PILOT_CREDENTIAL_INVALID", "Pilot credential 无效")
				return
			}
			ctx := context.WithValue(r.Context(), pilotIDKey{}, credential.PilotInstanceID)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		if _, ok := publicPaths[r.URL.Path]; ok {
			next.ServeHTTP(w, r)
			return
		}

		token := bearerToken(r)
		userID, err := s.ValidateToken(token)
		if err != nil {
			if aerr, ok := err.(*Error); ok {
				writeError(w, http.StatusUnauthorized, aerr.Code, aerr.Message)
				return
			}
			s.logger.WithError(err).Error("token 校验内部错误")
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务内部错误")
			return
		}

		next.ServeHTTP(w, r.WithContext(ContextWithUserID(r.Context(), userID)))
	})
}

// bearerToken 从 Authorization 头提取 Bearer token，无有效头时返回空串。
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, prefix))
}

// writeError 以统一格式 {"error":{"code","message"}} 写出错误响应。
// 与 internal/server/http.WriteError 的线上格式保持一致；auth 不引用
// http 包是为了避免 http 路由装配依赖 auth 时形成包循环。
func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": msg},
	})
}
