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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/crypto/bcrypt"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/log"
)

// 认证相关的错误码，与 HTTP 响应体 error.code 一致，前端据此区分处理。
const (
	// CodeInvalidCredentials 用户名或密码错误（统一文案，不暴露具体哪一项错）。
	CodeInvalidCredentials = "AUTH_INVALID_CREDENTIALS"

	// CodeTokenInvalid token 不存在或格式非法。
	CodeTokenInvalid = "AUTH_TOKEN_INVALID"

	// CodeTokenExpired token 已过期（过期记录同时被剔除）。
	CodeTokenExpired = "AUTH_TOKEN_EXPIRED"

	// CodeTokenRequired 请求缺少 Bearer token。
	CodeTokenRequired = "AUTH_TOKEN_REQUIRED"
)

const (
	// tokenTTL 是访问令牌的有效期。
	tokenTTL = 24 * time.Hour

	// defaultAdminUsername 是种子用户的登录名。
	defaultAdminUsername = "admin"

	// defaultAdminPassword 是未设置 SEMANTIC_ADMIN_PASSWORD 时的初始密码，
	// 仅为零配置可启动兜底，首次启动日志会 WARN 提示尽快修改。
	defaultAdminPassword = "admin123"

	// adminPasswordEnv 是种子用户初始密码的环境变量名。
	adminPasswordEnv = "SEMANTIC_ADMIN_PASSWORD"
)

// Error 是认证模块的统一错误类型，携带面向前端的错误码与文案。
type Error struct {
	// Code 机器可读错误码（AUTH_* 前缀）。
	Code string

	// Message 人类可读错误描述。
	Message string
}

// Error 返回错误文案，实现 error 接口。
func (e *Error) Error() string {
	return e.Message
}

// Service 是本地账号认证服务，依赖 store 持久化用户与 token。
type Service struct {
	// store 元数据存储，承载 users/tokens 两张表。
	store *store.Store

	// logger 结构化日志器。
	logger *log.Logger

	// tokenLifetime 在启动时配置，签发和续期使用相同有效期。
	tokenLifetime time.Duration
}

// NewService 创建认证服务。
func NewService(st *store.Store, logger *log.Logger) *Service {
	return &Service{store: st, logger: logger, tokenLifetime: tokenTTL}
}

// SetTokenTTL 设置新令牌的有效期，须在启动接收请求前调用。
// 已签发令牌继续沿用各自持久化的到期时间。
func (s *Service) SetTokenTTL(ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("access_token_ttl 必须大于零")
	}
	s.tokenLifetime = ttl
	return nil
}

// SeedAdmin 在系统尚无 admin 用户时创建种子用户，重复调用幂等。
// 初始密码取环境变量 SEMANTIC_ADMIN_PASSWORD；未设置时使用默认密码
// 并 WARN 提示——保证零配置可启动，同时不静默留下弱口令。
func (s *Service) SeedAdmin() error {
	_, err := s.store.GetByUsername(defaultAdminUsername)
	switch {
	case err == nil:
		s.logger.Debug("种子用户已存在，跳过创建", "username", defaultAdminUsername)
		return nil
	case !errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("查询种子用户失败: %w", err)
	}

	password := os.Getenv(adminPasswordEnv)
	if password == "" {
		password = defaultAdminPassword
		s.logger.Warn("未设置 "+adminPasswordEnv+"，种子用户使用默认初始密码，请尽快修改",
			"username", defaultAdminUsername)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("生成密码哈希失败: %w", err)
	}
	u := store.User{
		ID:           "usr_" + randomHex(8),
		Username:     defaultAdminUsername,
		PasswordHash: string(hash),
		CreatedAt:    time.Now().UTC(),
	}
	if err := s.store.CreateUser(u); err != nil {
		return err
	}
	s.logger.Info("种子用户已创建", "username", defaultAdminUsername, "user_id", u.ID)
	return nil
}

// Login 校验用户名与密码，成功则签发新 token 并返回。
// 用户名不存在与密码错误返回同一错误码，避免暴露账号是否存在。
// 日志只记录用户名，绝不记录密码。
func (s *Service) Login(username, password string) (string, error) {
	u, err := s.store.GetByUsername(username)
	if errors.Is(err, store.ErrNotFound) {
		s.logger.Warn("登录失败：用户不存在", "username", username)
		return "", &Error{Code: CodeInvalidCredentials, Message: "用户名或密码错误"}
	}
	if err != nil {
		return "", fmt.Errorf("登录查询用户失败: %w", err)
	}

	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		s.logger.Warn("登录失败：密码错误", "username", username)
		return "", &Error{Code: CodeInvalidCredentials, Message: "用户名或密码错误"}
	}

	token, err := s.IssueToken(u.ID)
	if err != nil {
		return "", err
	}
	s.logger.Info("用户登录成功", "username", username, "user_id", u.ID)
	return token, nil
}

// IssueToken 为指定用户签发新 token：32 字节随机数的十六进制串，使用当前配置的有效期。
func (s *Service) IssueToken(userID string) (string, error) {
	now := time.Now().UTC()
	t := store.Token{
		Token:     randomHex(32),
		UserID:    userID,
		ExpiresAt: now.Add(s.tokenLifetime),
		CreatedAt: now,
	}
	if err := s.store.CreateToken(t); err != nil {
		return "", err
	}
	s.logger.Info("token 已签发", "user_id", userID, "expires_at", t.ExpiresAt.Format(time.RFC3339))
	return t.Token, nil
}

// ValidateToken 校验 token 有效性并返回所属用户 ID。
// 过期 token 会被立即剔除（惰性清理），并返回 AUTH_TOKEN_EXPIRED；
// 不存在的 token 返回 AUTH_TOKEN_INVALID。
func (s *Service) ValidateToken(token string) (string, error) {
	if token == "" {
		return "", &Error{Code: CodeTokenRequired, Message: "缺少访问令牌"}
	}
	t, err := s.store.GetToken(token)
	if errors.Is(err, store.ErrNotFound) {
		return "", &Error{Code: CodeTokenInvalid, Message: "访问令牌无效"}
	}
	if err != nil {
		return "", fmt.Errorf("校验 token 失败: %w", err)
	}

	if time.Now().UTC().After(t.ExpiresAt) {
		if err := s.store.DeleteToken(token); err != nil {
			s.logger.WithError(err).Error("剔除过期 token 失败", "user_id", t.UserID)
		}
		s.logger.Info("token 已过期并剔除", "user_id", t.UserID)
		return "", &Error{Code: CodeTokenExpired, Message: "访问令牌已过期"}
	}
	return t.UserID, nil
}

// Refresh 换发 token：校验旧 token 后作废，签发新 token 返回。
// 旧 token 立即失效，保证同一时刻一个旧 token 只能换一次。
func (s *Service) Refresh(token string) (string, error) {
	userID, err := s.ValidateToken(token)
	if err != nil {
		return "", err
	}
	if err := s.store.DeleteToken(token); err != nil {
		return "", err
	}
	newToken, err := s.IssueToken(userID)
	if err != nil {
		return "", err
	}
	s.logger.Info("token 已换发", "user_id", userID)
	return newToken, nil
}

// Logout 注销 token，使其立即失效；token 不存在时同样视为成功（登出幂等）。
func (s *Service) Logout(token string) error {
	userID, err := s.store.GetToken(token)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("登出查询 token 失败: %w", err)
	}
	if err := s.store.DeleteToken(token); err != nil {
		return err
	}
	if userID != nil {
		s.logger.Info("用户已登出", "user_id", userID.UserID)
	}
	return nil
}

// randomHex 生成 n 字节随机数的十六进制串（2n 字符），用于 token 与 ID。
// crypto/rand 保证不可预测性，是 token 安全性的根基。
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
