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
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

// RecoverInterruptedRuns 在 Server 完成 Store/事件总线装配、开始监听请求之前
// 收敛旧进程留下的非终态 Run。模型与工具不会恢复；pending Interaction
// 保留在 Store，由 Studio Snapshot 继续展示。
func (s *Service) RecoverInterruptedRuns(now time.Time) error {
	runs, _, err := s.st.ListRunSessions(store.RunFilter{}, 0, 0)
	if err != nil {
		return err
	}
	candidates := make([]store.RunSession, 0)
	for _, run := range runs {
		switch run.Status {
		case store.RunStatusQueued, store.RunStatusRunning,
			store.RunStatusWaitingInput, store.RunStatusCancelling:
			candidates = append(candidates, run)
		}
	}
	failed, cancelled, err := s.st.MarkInterruptedRuns(now)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		updated, getErr := s.st.GetRunSession(candidate.ID)
		if getErr != nil {
			return fmt.Errorf("读取重启收敛后的 Run %q 失败: %w", candidate.ID, getErr)
		}
		switch updated.Status {
		case store.RunStatusFailed:
			s.publishRunState(updated, nil, EventTypeRunFailed)
		case store.RunStatusCancelled:
			s.publishRunState(updated, nil, EventTypeRunCancelled)
		}
	}
	if failed > 0 || cancelled > 0 {
		s.logger.Warn("Server 启动时已收敛中断 Run",
			"failed", failed, "cancelled", cancelled)
	}
	return nil
}
