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
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
)

// TestFilterSessionExecutionTools 验证 execute_host 只有在 Server 与当前会话
// 双开关同时开启时可见；普通 Docker execute 始终保留。
func TestFilterSessionExecutionTools(t *testing.T) {
	fx := newTestFixture(t)
	svc := NewService(Deps{Store: fx.st, Logger: fx.logger, AllowHostExecution: true})
	now := time.Now().UTC()
	if err := fx.st.CreateChatSession(store.ChatSession{ID: "cs-tool-visible", UserID: "usr-1",
		Title: "执行工具可见性", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	defs := []tool.Definition{{Name: tool.ExecuteToolName}, {Name: tool.ExecuteHostToolName}}

	filtered, err := svc.filterSessionExecutionTools("cs-tool-visible", defs)
	if err != nil || len(filtered) != 1 || filtered[0].Name != tool.ExecuteToolName {
		t.Fatalf("会话未开启时只应保留 Docker execute: defs=%+v err=%v", filtered, err)
	}
	if _, err := fx.st.SetSessionExecutionPolicy(store.SessionExecutionPolicy{
		SessionID: "cs-tool-visible", Mode: store.ExecutionModeAsk, HostExecutionEnabled: true,
	}); err != nil {
		t.Fatalf("开启会话宿主执行失败: %v", err)
	}
	filtered, err = svc.filterSessionExecutionTools("cs-tool-visible", defs)
	if err != nil || len(filtered) != 2 {
		t.Fatalf("双开关开启时两个工具均应可见: defs=%+v err=%v", filtered, err)
	}
	svc.SetHostExecutionAllowed(false)
	filtered, err = svc.filterSessionExecutionTools("cs-tool-visible", defs)
	if err != nil || len(filtered) != 1 || filtered[0].Name != tool.ExecuteToolName {
		t.Fatalf("Server 关闭后应立即移除宿主工具: defs=%+v err=%v", filtered, err)
	}
}
