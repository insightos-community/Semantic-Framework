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

package ws

import (
	"sync"

	"insightos.cn/semantic-framework/pkg/log"
)

// Hub 维护 WS 连接与会话的订阅关系，是事件的投递中枢。
// 语义：Publish 按 env.SessionID 投递给订阅该会话的连接集合；
// SessionID 为空时广播给全部在线连接。所有方法并发安全。
type Hub struct {
	// logger 结构化日志器。
	logger *log.Logger

	// mu 保护 sessions 与 all 的读写锁。
	mu sync.RWMutex

	// sessions 维护 session_id → 连接集合；projects 维护 Studio 的
	// project_id → 连接集合。一个事件同时具有二者时会去重投递。
	sessions map[string]map[*conn]struct{}
	projects map[string]map[*conn]struct{}

	// all 维护全部在线连接，用于广播与连接数统计。
	all map[*conn]struct{}
}

// NewHub 创建连接中枢。
func NewHub(logger *log.Logger) *Hub {
	return &Hub{
		logger:   logger,
		sessions: make(map[string]map[*conn]struct{}),
		projects: make(map[string]map[*conn]struct{}),
		all:      make(map[*conn]struct{}),
	}
}

// Subscribe 注册连接到中枢。sessionID 非空时同时加入该会话的订阅集合，
// 为空表示连接只接收广播。
func (h *Hub) Subscribe(sessionID string, c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.all[c] = struct{}{}
	if sessionID != "" {
		if h.sessions[sessionID] == nil {
			h.sessions[sessionID] = make(map[*conn]struct{})
		}
		h.sessions[sessionID][c] = struct{}{}
	}
}

// SubscribeProject 注册一条 Project Studio 连接。
func (h *Hub) SubscribeProject(projectID string, c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.all[c] = struct{}{}
	if h.projects[projectID] == nil {
		h.projects[projectID] = make(map[*conn]struct{})
	}
	h.projects[projectID][c] = struct{}{}
}

// UnsubscribeProject 注销 Project Studio 连接。
func (h *Hub) UnsubscribeProject(projectID string, c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.all, c)
	if set, ok := h.projects[projectID]; ok {
		delete(set, c)
		if len(set) == 0 {
			delete(h.projects, projectID)
		}
	}
}

// Unsubscribe 注销连接：从广播集合与指定会话的订阅集合中移除。
// 会话集合清空后删除该会话键，避免长运行后积累空集合。
func (h *Hub) Unsubscribe(sessionID string, c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.all, c)
	if set, ok := h.sessions[sessionID]; ok {
		delete(set, c)
		if len(set) == 0 {
			delete(h.sessions, sessionID)
		}
	}
}

// Publish 按 Session 和 Project 投递事件。两种订阅都命中同一连接时只投递
// 一次；二者都为空才是全局广播。
func (h *Hub) Publish(env Envelope) {
	h.mu.RLock()
	targetSet := make(map[*conn]struct{})
	if env.SessionID == "" && env.ProjectID == "" {
		for c := range h.all {
			targetSet[c] = struct{}{}
		}
	} else {
		for c := range h.sessions[env.SessionID] {
			targetSet[c] = struct{}{}
		}
		for c := range h.projects[env.ProjectID] {
			targetSet[c] = struct{}{}
		}
	}
	h.mu.RUnlock()

	for c := range targetSet {
		c.enqueue(env)
	}
}

// ConnCount 返回当前在线连接数，供测试与后续监控端点使用。
func (h *Hub) ConnCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.all)
}

// CloseAll 关闭全部在线连接。
// 为什么需要它：WS 连接经 http.Hijacker 脱离 http.Server 管理，
// Server.Shutdown 不会等待也不会关闭它们，服务退出前必须主动逐条关闭，
// 客户端才能收到 close 帧并及时重连。
func (h *Hub) CloseAll() {
	h.mu.RLock()
	conns := make([]*conn, 0, len(h.all))
	for c := range h.all {
		conns = append(conns, c)
	}
	h.mu.RUnlock()
	for _, c := range conns {
		c.close()
	}
}
