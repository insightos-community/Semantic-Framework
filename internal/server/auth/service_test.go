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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/log"
)

// newTestService 创建基于临时库、已完成迁移的认证服务。
func newTestService(t *testing.T) *Service {
	t.Helper()
	st := storetest.OpenMigrated(t, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	return NewService(st, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
}

// assertAuthError 断言错误是指定 code 的 *Error。
func assertAuthError(t *testing.T, err error, code string) {
	t.Helper()
	var aerr *Error
	if !errors.As(err, &aerr) {
		t.Fatalf("错误应为 *Error(%s)，实际: %v", code, err)
	}
	if aerr.Code != code {
		t.Fatalf("错误码应为 %s，实际: %s", code, aerr.Code)
	}
}

// TestSeedAdmin 验证种子用户创建与幂等性：可登录、重复调用不报错。
func TestSeedAdmin(t *testing.T) {
	svc := newTestService(t)
	t.Setenv(adminPasswordEnv, "s3cret")

	if err := svc.SeedAdmin(); err != nil {
		t.Fatalf("SeedAdmin 失败: %v", err)
	}
	if _, err := svc.Login(defaultAdminUsername, "s3cret"); err != nil {
		t.Errorf("种子用户应能以环境变量密码登录，实际: %v", err)
	}
	// 幂等：再次执行不报错，且原密码仍有效（未被重置）。
	if err := svc.SeedAdmin(); err != nil {
		t.Errorf("重复 SeedAdmin 应幂等成功，实际: %v", err)
	}
	if _, err := svc.Login(defaultAdminUsername, "s3cret"); err != nil {
		t.Errorf("重复 SeedAdmin 后密码不应变化，实际: %v", err)
	}
}

// TestLogin 验证登录成功签发 token、失败返回统一错误码。
func TestLogin(t *testing.T) {
	svc := newTestService(t)
	t.Setenv(adminPasswordEnv, "s3cret")
	if err := svc.SeedAdmin(); err != nil {
		t.Fatalf("SeedAdmin 失败: %v", err)
	}

	token, err := svc.Login(defaultAdminUsername, "s3cret")
	if err != nil {
		t.Fatalf("登录应成功，实际: %v", err)
	}
	if len(token) != 64 {
		t.Errorf("token 应为 64 字符十六进制串，实际长度: %d", len(token))
	}

	// 密码错误与用户名不存在返回同一错误码（不暴露账号是否存在）。
	if _, err := svc.Login(defaultAdminUsername, "wrong"); err == nil {
		t.Error("密码错误应登录失败")
	} else {
		assertAuthError(t, err, CodeInvalidCredentials)
	}
	if _, err := svc.Login("nobody", "s3cret"); err == nil {
		t.Error("用户不存在应登录失败")
	} else {
		assertAuthError(t, err, CodeInvalidCredentials)
	}
}

// TestTokenLifecycle 验证 token 的签发→校验→换发→登出全生命周期。
func TestTokenLifecycle(t *testing.T) {
	svc := newTestService(t)
	t.Setenv(adminPasswordEnv, "s3cret")
	if err := svc.SeedAdmin(); err != nil {
		t.Fatalf("SeedAdmin 失败: %v", err)
	}
	token, err := svc.Login(defaultAdminUsername, "s3cret")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}

	userID, err := svc.ValidateToken(token)
	if err != nil {
		t.Fatalf("ValidateToken 应通过，实际: %v", err)
	}
	if userID == "" {
		t.Error("ValidateToken 应返回非空 user_id")
	}

	// 换发：新 token 可用，旧 token 立即失效。
	newToken, err := svc.Refresh(token)
	if err != nil {
		t.Fatalf("Refresh 失败: %v", err)
	}
	if newToken == token {
		t.Error("Refresh 应签发不同的新 token")
	}
	if _, err := svc.ValidateToken(token); err == nil {
		t.Error("旧 token 换发后应失效")
	} else {
		assertAuthError(t, err, CodeTokenInvalid)
	}

	// 登出：token 失效，重复登出幂等。
	if err := svc.Logout(newToken); err != nil {
		t.Fatalf("Logout 失败: %v", err)
	}
	if _, err := svc.ValidateToken(newToken); err == nil {
		t.Error("登出后 token 应失效")
	}
	if err := svc.Logout(newToken); err != nil {
		t.Errorf("重复 Logout 应幂等成功，实际: %v", err)
	}
}

// TestValidateTokenExpired 验证过期 token 被剔除并返回 AUTH_TOKEN_EXPIRED。
func TestValidateTokenExpired(t *testing.T) {
	svc := newTestService(t)
	now := time.Now().UTC()
	expired := store.Token{
		Token:     "expired-token",
		UserID:    "usr_x",
		ExpiresAt: now.Add(-time.Minute),
		CreatedAt: now.Add(-25 * time.Hour),
	}
	if err := svc.store.CreateToken(expired); err != nil {
		t.Fatalf("写入过期 token 失败: %v", err)
	}

	_, err := svc.ValidateToken("expired-token")
	assertAuthError(t, err, CodeTokenExpired)

	// 惰性清理：过期 token 已被删除，再次校验按“无效”处理。
	_, err = svc.ValidateToken("expired-token")
	assertAuthError(t, err, CodeTokenInvalid)
}

// TestMiddleware 验证中间件的白名单放行与 Bearer 鉴权拦截。
func TestMiddleware(t *testing.T) {
	svc := newTestService(t)
	t.Setenv(adminPasswordEnv, "s3cret")
	if err := svc.SeedAdmin(); err != nil {
		t.Fatalf("SeedAdmin 失败: %v", err)
	}
	token, err := svc.Login(defaultAdminUsername, "s3cret")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}

	// 下游 handler：回显 context 中的 user_id，证明注入生效。
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(UserIDFromContext(r.Context())))
	})
	handler := svc.Middleware(next)

	cases := []struct {
		name       string
		path       string
		authHeader string
		wantStatus int
		wantBody   string
	}{
		{"白名单登录端点放行", "/api/v1/auth/login", "", http.StatusOK, ""},
		{"白名单健康检查放行", "/api/v1/system/healthz", "", http.StatusOK, ""},
		{"缺少 token 拦截", "/api/v1/system/ping", "", http.StatusUnauthorized, CodeTokenRequired},
		{"非法 token 拦截", "/api/v1/system/ping", "Bearer bad-token", http.StatusUnauthorized, CodeTokenInvalid},
		{"合法 token 放行并注入 user_id", "/api/v1/system/ping", "Bearer " + token, http.StatusOK, "usr_"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("状态码应为 %d，实际: %d（body: %s）", tc.wantStatus, rec.Code, rec.Body.String())
			}
			if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("响应体应包含 %q，实际: %s", tc.wantBody, rec.Body.String())
			}
		})
	}
}
