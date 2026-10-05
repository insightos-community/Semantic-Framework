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

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWSBaseFromHTTP 验证 WS 地址推导：scheme 映射与端口 +1 约定。
func TestWSBaseFromHTTP(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"http://127.0.0.1:8080", "ws://127.0.0.1:8081"},
		{"https://example.com:8443", "wss://example.com:8444"},
		{"http://example.com", "ws://example.com"},   // 无显式端口则不加
		{"ws://10.0.0.1:9000", "ws://10.0.0.1:9001"}, // ws scheme 直通
		{"http://[::1]:8080", "ws://[::1]:8081"},     // IPv6 主机
	}
	for _, tc := range cases {
		got, err := wsBaseFromHTTP(tc.in)
		if err != nil {
			t.Errorf("wsBaseFromHTTP(%q) 返回错误: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("wsBaseFromHTTP(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
	if _, err := wsBaseFromHTTP("ftp://x"); err == nil {
		t.Error("非法 scheme 应返回错误")
	}
}

// TestCredentialsRoundtrip 验证凭据保存/读取闭环：内容一致、
// 目录 0700、文件 0600（token 的权限语义与 SSH 私钥一致）。
func TestCredentialsRoundtrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	want := credentials{
		Server:    "http://127.0.0.1:8080",
		Token:     "tok-abc",
		ExpiresAt: time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second),
	}
	if err := saveCredentials(want); err != nil {
		t.Fatalf("saveCredentials 失败: %v", err)
	}

	got, err := loadCredentials()
	if err != nil {
		t.Fatalf("loadCredentials 失败: %v", err)
	}
	if got.Server != want.Server || got.Token != want.Token || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("凭据往返不一致: %+v", got)
	}

	path, err := credentialsPath()
	if err != nil {
		t.Fatalf("credentialsPath 失败: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("凭据文件不存在: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("凭据文件权限应为 0600，实际: %o", perm)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("凭据目录不存在: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("凭据目录权限应为 0700，实际: %o", perm)
	}
}

// TestCredentialsMissingAndExpiry 验证无凭据与过期两条防御路径。
func TestCredentialsMissingAndExpiry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// 未登录：errNoCredentials。
	if _, err := loadCredentials(); !errors.Is(err, errNoCredentials) {
		t.Errorf("无凭据应返回 errNoCredentials，实际: %v", err)
	}

	// 过期：loadValidCredentials 拒绝。
	if err := saveCredentials(credentials{
		Server: "http://x", Token: "tok", ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("saveCredentials 失败: %v", err)
	}
	if _, err := loadValidCredentials(); err == nil {
		t.Error("过期凭据应被拒绝")
	}
}

// TestResolveServer 验证服务地址的优先级：flag > 凭据 > 缺省。
func TestResolveServer(t *testing.T) {
	creds := credentials{Server: "http://creds:1"}
	if got := resolveServer("http://flag:2", creds); got != "http://flag:2" {
		t.Errorf("flag 优先，实际: %q", got)
	}
	if got := resolveServer("", creds); got != "http://creds:1" {
		t.Errorf("凭据次之，实际: %q", got)
	}
	if got := resolveServer("", credentials{}); got != defaultServer {
		t.Errorf("缺省回落，实际: %q", got)
	}
}
