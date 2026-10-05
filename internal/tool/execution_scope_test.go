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

package tool

import (
	"context"
	"testing"
)

// TestExecutionScope 验证执行边界只随当前 context 传播，空 context 不会
// 意外获得其他会话的工作区权限。
func TestExecutionScope(t *testing.T) {
	if _, ok := ExecutionScopeFromContext(context.Background()); ok {
		t.Fatal("空 context 不应包含执行边界")
	}

	want := ExecutionScope{SessionID: "session-1", ProjectID: "project-1", WorkspaceRoot: "/tmp/project-1"}
	ctx := WithExecutionScope(context.Background(), want)
	got, ok := ExecutionScopeFromContext(ctx)
	if !ok || got != want {
		t.Fatalf("执行边界读取不一致: got=%+v ok=%v want=%+v", got, ok, want)
	}
}
