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

package simulation

import (
	"context"
	"sync"
	"time"
)

// ProjectLeaseManager 统计同一 Project 的 Studio 标签页。最后一个连接断开后
// 保留 grace 窗口，刷新或短暂断网在窗口内重连不会停止场景；窗口到期才执行
// ReleaseProject。显式退出仍直接调用 ReleaseProject，不等待计时器。
type ProjectLeaseManager struct {
	mu      sync.Mutex
	service ProjectReleaser
	grace   time.Duration
	counts  map[string]int
	timers  map[string]*time.Timer
	closed  bool
}

// ProjectReleaser 让计时器只依赖 Project 清理能力，便于测试多标签和重连。
type ProjectReleaser interface {
	ReleaseProject(context.Context, string) error
}

func NewProjectLeaseManager(service ProjectReleaser, grace time.Duration) *ProjectLeaseManager {
	if grace <= 0 {
		grace = 60 * time.Second
	}
	return &ProjectLeaseManager{
		service: service,
		grace:   grace,
		counts:  map[string]int{},
		timers:  map[string]*time.Timer{},
	}
}

// Connected 必须在 Studio WS 完成鉴权和 Project 归属检查后调用。
func (m *ProjectLeaseManager) Connected(projectID string) {
	if m == nil || projectID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	if timer := m.timers[projectID]; timer != nil {
		timer.Stop()
		delete(m.timers, projectID)
	}
	m.counts[projectID]++
}

// Disconnected 只在最后一个标签页离开时启动宽限计时器。
func (m *ProjectLeaseManager) Disconnected(projectID string) {
	if m == nil || projectID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	if m.counts[projectID] > 1 {
		m.counts[projectID]--
		return
	}
	delete(m.counts, projectID)
	if old := m.timers[projectID]; old != nil {
		old.Stop()
	}
	m.timers[projectID] = time.AfterFunc(m.grace, func() {
		m.releaseIfStillUnused(projectID)
	})
}

func (m *ProjectLeaseManager) releaseIfStillUnused(projectID string) {
	m.mu.Lock()
	if m.closed || m.counts[projectID] > 0 {
		m.mu.Unlock()
		return
	}
	delete(m.timers, projectID)
	service := m.service
	m.mu.Unlock()
	if service != nil {
		_ = service.ReleaseProject(context.Background(), projectID)
	}
}

// Close 只停止租约计时器；Server 的统一 shutdown 随后负责收敛全部 Runtime。
func (m *ProjectLeaseManager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for _, timer := range m.timers {
		timer.Stop()
	}
	m.timers = map[string]*time.Timer{}
	m.counts = map[string]int{}
}
