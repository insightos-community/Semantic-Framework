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

package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"insightos.cn/semantic-framework/internal/tool"
)

// TestArtifactRegisterTool 验证 Agent 可把 Project workspace 既有文件显式
// 登记为带用户归属的 Artifact，而不是由 execute 自动登记。
func TestArtifactRegisterTool(t *testing.T) {
	reg, st := newTestRegistry(t)
	registered, ok := reg.Get(nameArtifactRegister)
	if !ok {
		t.Fatal("artifact.register 未注册")
	}
	workspace := t.TempDir()
	file := filepath.Join(workspace, "reports", "profile.json")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{"rows":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := tool.WithExecutionScope(context.Background(), tool.ExecutionScope{
		SessionID: "session-1", ProjectID: "project-1", OwnerID: "user-1", WorkspaceRoot: workspace,
	})
	result, err := registered.Run(ctx,
		`{"path":"reports/profile.json","media_type":"application/json","summary":"数据画像"}`)
	if err != nil {
		t.Fatalf("artifact.register 失败: %v", err)
	}
	var envelope struct {
		Data struct {
			ArtifactID string `json:"artifact_id"`
			URI        string `json:"uri"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(result), &envelope); err != nil || envelope.Data.ArtifactID == "" {
		t.Fatalf("登记结果不完整: result=%s err=%v", result, err)
	}
	artifact, content, err := st.GetArtifact(envelope.Data.ArtifactID)
	if err != nil || artifact.OwnerID != "user-1" || string(content) != `{"rows":2}` {
		t.Fatalf("Artifact 内容或归属不一致: artifact=%+v content=%q err=%v", artifact, content, err)
	}
}
