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

package kernel

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/cloudwego/eino/adk/middlewares/summarization"
	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/log"
)

// SummaryPersistence 描述本次 Leader Run 可以推进的 Context 摘要边界。
// CoveredThroughMessageID 必须是模型调用前已经持久化的用户消息，确保摘要
// 保存后下一轮能从严格晚于该 ID 的消息继续装配。
type SummaryPersistence struct {
	ContextID               string
	ProjectID               string
	SessionID               string
	TaskID                  string
	CoveredThroughMessageID string
	ExpectedRevision        int64
}

type summaryPersistenceKey struct{}

type summaryPersistenceState struct {
	value SummaryPersistence
	mu    sync.Mutex
}

// WithSummaryPersistence 把一次 Run 的 Context 保存信息放入执行 context。
// 状态只在本次 Run 及其 Resume 之间存在，缓存 Runner 不持有会话信息。
func WithSummaryPersistence(ctx context.Context, value SummaryPersistence) context.Context {
	return context.WithValue(ctx, summaryPersistenceKey{}, &summaryPersistenceState{value: value})
}

// persistentSummaryFinalizer 沿用 Eino 的默认摘要消息格式，并把最终摘要及
// 覆盖边界条件保存到 SQLite。保存失败不阻断 Agent：下一轮继续使用 Store
// 中上一份有效摘要，避免观测/优化链路反向破坏用户当前请求。
func persistentSummaryFinalizer(st *store.Store, logger *log.Logger) summarization.FinalizeFunc {
	return func(ctx context.Context, originalMessages []*schema.Message,
		rawSummary *schema.Message) ([]*schema.Message, error) {
		finalMessages, err := summarization.DefaultFinalize(ctx, originalMessages, rawSummary)
		if err != nil {
			return nil, err
		}
		state, ok := ctx.Value(summaryPersistenceKey{}).(*summaryPersistenceState)
		if !ok || state == nil || len(finalMessages) == 0 {
			return finalMessages, nil
		}
		last := finalMessages[len(finalMessages)-1]
		if last == nil || strings.TrimSpace(last.Content) == "" {
			return finalMessages, nil
		}

		state.mu.Lock()
		defer state.mu.Unlock()
		value := state.value
		if value.SessionID == "" || value.CoveredThroughMessageID == "" {
			return finalMessages, nil
		}
		saved, saveErr := st.SaveContextSummary(store.ContextSummary{
			ContextID: value.ContextID, ProjectID: value.ProjectID,
			SessionID: value.SessionID, TaskID: value.TaskID, Summary: last.Content,
			CoveredThroughMessageID: value.CoveredThroughMessageID,
		}, value.ExpectedRevision)
		if saveErr != nil {
			if errors.Is(saveErr, store.ErrRevisionConflict) {
				logger.Info("Context 摘要已有更新，本轮沿用已保存版本",
					"context_id", value.ContextID, "conversation_id", value.SessionID)
			} else {
				logger.WithError(saveErr).Warn("Context 摘要保存失败，本轮继续执行",
					"context_id", value.ContextID, "conversation_id", value.SessionID)
			}
			return finalMessages, nil
		}
		// 同一 Run 若再次触发摘要，使用刚保存的 revision 继续条件更新。
		state.value.ExpectedRevision = saved.Revision
		return finalMessages, nil
	}
}
