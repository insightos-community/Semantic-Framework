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

package runtime

import (
	"fmt"

	"insightos.cn/semantic-framework/internal/store"
)

// SessionExecutionPolicyView 是会话执行控制面的安全视图。它同时返回服务端
// 是否允许宿主执行，前端无需通过试错判断为什么开关不可用。
type SessionExecutionPolicyView struct {
	Mode                 string `json:"mode"`
	HostExecutionEnabled bool   `json:"host_execution_enabled"`
	HostExecutionAllowed bool   `json:"host_execution_allowed"`
	Busy                 bool   `json:"busy"`
}

// SetHostExecutionAllowed 热应用服务端宿主执行硬开关，并淘汰按旧工具可见性
// 构建的空闲 Runner。运行中的 Runner 完成本轮后淘汰；实际工具调用还会
// 通过单次 Run context 再次校验硬开关。
func (s *Service) SetHostExecutionAllowed(allowed bool) {
	s.mu.Lock()
	s.allowHostExecution = allowed
	s.invalidateModelRuntimesLocked("")
	var runs []activeRun
	if !allowed {
		for _, active := range s.activeRuns {
			runs = append(runs, active)
		}
	}
	s.mu.Unlock()
	// 关闭硬开关时取消正在运行的会话，确保已捕获旧作用域的后续工具步骤
	// 不能继续进入宿主执行。状态迁移与取消在锁外进行，避免收尾反向等待 s.mu。
	for _, active := range runs {
		s.cancelActiveRun(active)
	}
}

// hostExecutionAllowed 并发安全读取服务端宿主执行硬开关。
func (s *Service) hostExecutionAllowed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allowHostExecution
}

// GetSessionExecutionPolicy 返回用户所属会话的执行策略。
func (s *Service) GetSessionExecutionPolicy(userID, sessionID string) (SessionExecutionPolicyView, error) {
	if err := s.requireOwnedSession(userID, sessionID); err != nil {
		return SessionExecutionPolicyView{}, err
	}
	policy, err := s.st.GetSessionExecutionPolicy(sessionID)
	if err != nil {
		return SessionExecutionPolicyView{}, err
	}
	return s.executionPolicyView(policy), nil
}

// SetSessionExecutionPolicy 在会话空闲时更新执行模式和宿主开关。宿主执行
// 被服务端禁用时 fail-closed，不能先写入一个实际不会生效的“已开启”状态。
func (s *Service) SetSessionExecutionPolicy(userID, sessionID, mode string,
	hostEnabled bool) (SessionExecutionPolicyView, error) {
	if _, _, err := s.requireWritableSession(userID, sessionID); err != nil {
		return SessionExecutionPolicyView{}, err
	}
	if !store.ValidExecutionMode(mode) {
		return SessionExecutionPolicyView{}, fmt.Errorf("执行模式仅支持 ask/auto/full")
	}
	if hostEnabled && !s.hostExecutionAllowed() {
		return SessionExecutionPolicyView{}, fmt.Errorf("Server 未启用宿主执行")
	}
	// 与模型切换使用同一会话锁，关闭“检查空闲后、写策略前”新 Run 抢入的
	// 竞态窗口；不能等待当前 Run 完成后暗中改变策略，调用方应收到 409。
	gate := s.sessionLock(sessionID)
	if !gate.TryLock() {
		return SessionExecutionPolicyView{}, ErrSessionBusy
	}
	defer gate.Unlock()
	if s.sessionBusy(sessionID) {
		return SessionExecutionPolicyView{}, ErrSessionBusy
	}
	var policy store.SessionExecutionPolicy
	err := s.st.WithWritableConversation(userID, sessionID,
		func(_ store.ChatSession, _ store.Project) error {
			var writeErr error
			policy, writeErr = s.st.SetSessionExecutionPolicy(store.SessionExecutionPolicy{
				SessionID: sessionID, Mode: mode, HostExecutionEnabled: hostEnabled,
			})
			return writeErr
		})
	if err != nil {
		return SessionExecutionPolicyView{}, mapWritableSessionError(err)
	}
	// 工具可见性和审批门禁在 Runner 构建时固化，策略变化后必须淘汰缓存。
	s.mu.Lock()
	delete(s.sessions, sessionID)
	delete(s.staleSessions, sessionID)
	s.mu.Unlock()
	return s.executionPolicyView(policy), nil
}

// executionPolicyView 把存储记录转换为不泄漏内部状态的 API 视图。
func (s *Service) executionPolicyView(policy store.SessionExecutionPolicy) SessionExecutionPolicyView {
	return SessionExecutionPolicyView{
		Mode: policy.Mode, HostExecutionEnabled: policy.HostExecutionEnabled,
		HostExecutionAllowed: s.hostExecutionAllowed(), Busy: s.sessionBusy(policy.SessionID),
	}
}
