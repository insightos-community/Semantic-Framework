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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/coder/websocket"
)

// errNoCredentials 表示本地没有登录凭据（未执行过 semantic login）。
var errNoCredentials = errors.New("未找到登录凭据，请先执行 semantic login")

// credentials 是本地保存的登录凭据（~/.semantic/credentials.json，0600）。
type credentials struct {
	// Server 登录时的服务地址（后续命令的默认 --server）。
	Server string `json:"server"`

	// Token 访问令牌。
	Token string `json:"token"`

	// ExpiresAt 令牌过期时间。
	ExpiresAt time.Time `json:"expires_at"`
}

// credentialsPath 返回凭据文件路径（$HOME/.semantic/credentials.json）。
func credentialsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("定位用户目录失败: %w", err)
	}
	return filepath.Join(home, ".semantic", "credentials.json"), nil
}

// Expired 报告 token 是否已过期。
func (c credentials) Expired() bool {
	return !c.ExpiresAt.IsZero() && time.Now().After(c.ExpiresAt)
}

// saveCredentials 写入凭据：目录 0700、文件 0600（token 等同口令，
// 权限语义与 SSH 私钥一致）。
func saveCredentials(c credentials) error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("创建凭据目录失败: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化凭据失败: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("写入凭据失败: %w", err)
	}
	return nil
}

// loadCredentials 读取凭据；文件不存在返回 errNoCredentials。
func loadCredentials() (credentials, error) {
	path, err := credentialsPath()
	if err != nil {
		return credentials{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return credentials{}, errNoCredentials
	}
	if err != nil {
		return credentials{}, fmt.Errorf("读取凭据失败: %w", err)
	}
	var c credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return credentials{}, fmt.Errorf("凭据文件损坏（%s）: %w", path, err)
	}
	if c.Token == "" {
		return credentials{}, errNoCredentials
	}
	return c, nil
}

// Client 是 semantic-server 的客户端：REST（标准库）+ WS（coder/websocket）。
type Client struct {
	// httpBase REST 基地址（如 http://127.0.0.1:8080）。
	httpBase string

	// wsBase WS 基地址（默认由 httpBase 推导，见 wsBaseFromHTTP）。
	wsBase string

	// token 访问令牌（REST 走 Bearer 头，WS 走 query 参数）。
	token string

	// hc HTTP 客户端。
	hc *http.Client
}

// newClient 创建客户端并推导 WS 地址。
func newClient(httpBase, token string) (*Client, error) {
	wsBase, err := wsBaseFromHTTP(httpBase)
	if err != nil {
		return nil, err
	}
	return &Client{httpBase: httpBase, wsBase: wsBase, token: token,
		hc: &http.Client{Timeout: 15 * time.Second}}, nil
}

// wsBaseFromHTTP 由 REST 地址推导 WS 地址：scheme http→ws、https→wss，
// 显式端口 +1（默认部署 8080/8081 的约定；非标准端口部署用 --ws 显式指定）。
func wsBaseFromHTTP(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("服务地址 %q 非法: %w", base, err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss": // 调用方直接给了 WS 地址，原样使用
	default:
		return "", fmt.Errorf("无法从 %q 推导 WS 地址：scheme 须为 http(s)", base)
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil {
			return "", fmt.Errorf("服务地址 %q 端口非法: %w", base, err)
		}
		u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(n+1))
	}
	return u.String(), nil
}

// sessionView 是会话的响应视图（与服务端 handlers.sessionView 一致）。
type sessionView struct {
	// ID 会话 ID。
	ID string `json:"id"`

	// Title 会话标题。
	Title string `json:"title"`

	// CreatedAt 创建时间。
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt 最近活跃时间。
	UpdatedAt time.Time `json:"updated_at"`
}

// apiError 是服务端统一错误响应的解析形态。
type apiError struct {
	// Code 机器可读错误码。
	Code string `json:"code"`

	// Message 人类可读错误描述。
	Message string `json:"message"`
}

// doJSON 发起一次 JSON REST 调用：method/path/请求体/响应体指针。
// 非 2xx 统一解析 {"error":{"code","message"}} 后返回错误。
func (c *Client) doJSON(method, path string, reqBody, respBody any) error {
	var reader *bytes.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("序列化请求体失败: %w", err)
		}
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, c.httpBase+path, reader)
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("请求 %s %s 失败: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var body struct {
			Error apiError `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err == nil && body.Error.Code != "" {
			return fmt.Errorf("%s %s 返回 %d: [%s] %s",
				method, path, resp.StatusCode, body.Error.Code, body.Error.Message)
		}
		return fmt.Errorf("%s %s 返回 %d", method, path, resp.StatusCode)
	}
	if respBody == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(respBody); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}
	return nil
}

// login 调用 POST /api/v1/auth/login，返回 token 与过期时间。
func (c *Client) login(username, password string) (string, time.Time, error) {
	var body struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	err := c.doJSON(http.MethodPost, "/api/v1/auth/login",
		map[string]string{"username": username, "password": password}, &body)
	if err != nil {
		return "", time.Time{}, err
	}
	return body.Token, body.ExpiresAt, nil
}

// createSession 调用 POST /api/v1/chat/sessions 创建会话。
func (c *Client) createSession(title string) (sessionView, error) {
	var body struct {
		Session sessionView `json:"session"`
	}
	reqBody := map[string]string{}
	if title != "" {
		reqBody["title"] = title
	}
	if err := c.doJSON(http.MethodPost, "/api/v1/chat/sessions", reqBody, &body); err != nil {
		return sessionView{}, err
	}
	return body.Session, nil
}

// listSessions 调用 GET /api/v1/chat/sessions 列出本人的会话。
func (c *Client) listSessions() ([]sessionView, error) {
	var body struct {
		Sessions []sessionView `json:"sessions"`
	}
	if err := c.doJSON(http.MethodGet, "/api/v1/chat/sessions", nil, &body); err != nil {
		return nil, err
	}
	return body.Sessions, nil
}

// dialChat 建立 /ws/chat 连接并订阅会话（token 经 query 参数携带，
// 与浏览器 WebSocket 无法自定义头的约定一致，见 docs/api/ws.md）。
func (c *Client) dialChat(sessionID string) (*websocket.Conn, error) {
	u := fmt.Sprintf("%s/ws/chat?token=%s&session_id=%s",
		c.wsBase, url.QueryEscape(c.token), url.QueryEscape(sessionID))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		return nil, fmt.Errorf("连接 %s 失败: %w", u, err)
	}
	return conn, nil
}
