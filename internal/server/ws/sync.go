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
	"insightos.cn/semantic-framework/pkg/log"
)

// handleSync 处理一条 sync 上行（/ws/chat 与 /ws/agent-events 共用）：
// 按 last_event_id 补发连接订阅会话的缺失事件（按 id 升序，trace 不补），
// 补发完回 sync.done；last_event_id 为空表示客户端只要实时流，直接回
// sync.done（count=0）不补发。
// 补发在读泵内同步执行：补发事件与 sync.done 经同一 send 缓冲保序，
// 客户端收到 sync.done 即缺口闭合。
// 注意 at-least-once 边界：重连窗口内（订阅生效 → sync 处理之间）到达的
// 实时事件可能与补发重叠，客户端应按事件 id 去重（见 docs/api/ws.md）。
func handleSync(c *conn, replayer EventReplayer, sessionID, lastEventID string, logger *log.Logger) {
	if lastEventID == "" {
		c.reply(syncDoneReply{Type: "sync.done", Count: 0})
		return
	}
	if replayer == nil {
		c.reply(errorReply{Type: "error", Code: CodeWSSyncFailed, Message: "断连续传服务未装配"})
		return
	}
	envs, err := replayer.ReplayEvents(sessionID, lastEventID)
	if err != nil {
		logger.WithError(err).Warn("断连续传补发失败",
			"conn_id", c.id, "session_id", sessionID, "last_event_id", lastEventID)
		c.reply(errorReply{Type: "error", Code: CodeWSSyncFailed, Message: "补发失败，请重试"})
		return
	}
	for _, env := range envs {
		c.enqueue(env)
	}
	c.reply(syncDoneReply{Type: "sync.done", Count: len(envs)})
	logger.Info("断连续传补发完成",
		"conn_id", c.id, "session_id", sessionID,
		"last_event_id", lastEventID, "count", len(envs))
}
