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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// doLogin 通过 HTTP 处理器完成登录，返回响应记录器。
func doLogin(t *testing.T, h *Handlers, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.HandleLogin(rec, req)
	return rec
}

// parseToken 从登录/刷新响应体中解析 token。
func parseToken(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var resp tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析 token 响应失败: %v（body: %s）", err, rec.Body.String())
	}
	if resp.Token == "" {
		t.Fatal("响应体应包含非空 token")
	}
	if resp.ExpiresAt.IsZero() {
		t.Fatal("响应体应包含 expires_at")
	}
	return resp.Token
}

// assertErrorBody 断言响应是统一错误格式且 code 匹配。
func assertErrorBody(t *testing.T, rec *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误响应应为 JSON，实际: %s", rec.Body.String())
	}
	if body.Error.Code != wantCode {
		t.Errorf("错误码应为 %s，实际: %s", wantCode, body.Error.Code)
	}
	if body.Error.Message == "" {
		t.Error("错误响应应包含非空 message")
	}
}

// TestHandleLogin 验证登录端点的成功、凭证错误与请求体错误三类响应。
func TestHandleLogin(t *testing.T) {
	svc := newTestService(t)
	t.Setenv(adminPasswordEnv, "s3cret")
	if err := svc.SeedAdmin(); err != nil {
		t.Fatalf("SeedAdmin 失败: %v", err)
	}
	h := NewHandlers(svc)

	// 成功：200 + token。
	rec := doLogin(t, h, `{"username":"admin","password":"s3cret"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("登录成功应返回 200，实际: %d（body: %s）", rec.Code, rec.Body.String())
	}
	parseToken(t, rec)

	// 凭证错误：401 + AUTH_INVALID_CREDENTIALS。
	rec = doLogin(t, h, `{"username":"admin","password":"wrong"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("凭证错误应返回 401，实际: %d", rec.Code)
	}
	assertErrorBody(t, rec, CodeInvalidCredentials)

	// 非法 JSON：400。
	rec = doLogin(t, h, `{not-json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 应返回 400，实际: %d", rec.Code)
	}
	assertErrorBody(t, rec, "BAD_REQUEST")

	// 缺字段：400。
	rec = doLogin(t, h, `{"username":"admin"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺少 password 应返回 400，实际: %d", rec.Code)
	}
	assertErrorBody(t, rec, "BAD_REQUEST")
}

// TestHandleRefreshLogout 验证刷新与登出端点的 token 流转。
func TestHandleRefreshLogout(t *testing.T) {
	svc := newTestService(t)
	t.Setenv(adminPasswordEnv, "s3cret")
	if err := svc.SeedAdmin(); err != nil {
		t.Fatalf("SeedAdmin 失败: %v", err)
	}
	h := NewHandlers(svc)

	token := parseToken(t, doLogin(t, h, `{"username":"admin","password":"s3cret"}`))

	// 刷新：200 + 新 token，旧 token 失效。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.HandleRefresh(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("刷新应返回 200，实际: %d（body: %s）", rec.Code, rec.Body.String())
	}
	newToken := parseToken(t, rec)
	if newToken == token {
		t.Error("刷新应返回与旧值不同的 token")
	}
	if _, err := svc.ValidateToken(token); err == nil {
		t.Error("旧 token 刷新后应失效")
	}

	// 登出：200 + {"ok":true}，token 失效。
	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.Header.Set("Authorization", "Bearer "+newToken)
	rec = httptest.NewRecorder()
	h.HandleLogout(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("登出应返回 200，实际: %d", rec.Code)
	}
	if _, err := svc.ValidateToken(newToken); err == nil {
		t.Error("登出后 token 应失效")
	}

	// 用已失效 token 刷新：401。
	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	req.Header.Set("Authorization", "Bearer "+newToken)
	rec = httptest.NewRecorder()
	h.HandleRefresh(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("失效 token 刷新应返回 401，实际: %d", rec.Code)
	}
	assertErrorBody(t, rec, CodeTokenInvalid)
}
