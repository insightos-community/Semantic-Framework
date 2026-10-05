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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk/filesystem"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
)

// fakeLocalExecutor 模拟 Eino-ext Local Backend 的 Execute 调用。
type fakeLocalExecutor struct {
	command  string
	response *filesystem.ExecuteResponse
	err      error
}

func (f *fakeLocalExecutor) Execute(_ context.Context,
	input *filesystem.ExecuteRequest) (*filesystem.ExecuteResponse, error) {
	f.command = input.Command
	return f.response, f.err
}

// hostExecutionContext 构造已通过 Server 与会话双开关的执行作用域。
func hostExecutionContext(workspace string) context.Context {
	return tool.WithExecutionScope(context.Background(), tool.ExecutionScope{
		SessionID: "session-host", ProjectID: "project-host", WorkspaceRoot: workspace,
		ExecutionMode:        store.ExecutionModeFull,
		HostExecutionEnabled: true, HostExecutionAllowed: true,
	})
}

// TestExecuteHostTool 验证工具只给 Local Backend 增加安全的工作目录前缀，
// 并保留上游输出、退出码与截断状态。
func TestExecuteHostTool(t *testing.T) {
	exitCode := 3
	fake := &fakeLocalExecutor{response: &filesystem.ExecuteResponse{
		Output: "诊断结果", ExitCode: &exitCode, Truncated: true,
	}}
	host := &executeHostTool{newBackend: func(context.Context) (localExecutor, error) {
		return fake, nil
	}}
	workspace := t.TempDir()
	result, err := host.Run(hostExecutionContext(workspace),
		`{"command":"semantic-server --check","workdir":"diagnostics","timeout_seconds":9}`)
	if err != nil {
		t.Fatalf("execute_host 失败: %v", err)
	}
	wantDir, err := filepath.EvalSymlinks(filepath.Join(workspace, "diagnostics"))
	if err != nil {
		t.Fatal(err)
	}
	prefix := "cd -- " + shellSingleQuote(wantDir) + " && "
	if runtime.GOOS == "windows" {
		prefix = "$ErrorActionPreference = 'Stop'; $global:LASTEXITCODE = 0; Set-Location -LiteralPath '" + strings.ReplaceAll(wantDir, "'", "''") + "';"
	}
	if !strings.HasPrefix(fake.command, prefix) ||
		!strings.Contains(fake.command, "semantic-server --check") {
		t.Fatalf("Local Backend 命令边界不一致: %q", fake.command)
	}
	if !strings.Contains(result, `"output":"诊断结果"`) ||
		!strings.Contains(result, `"exit_code":3`) || !strings.Contains(result, `"truncated":true`) {
		t.Fatalf("结构化结果不一致: %s", result)
	}
}

// TestExecuteHostToolRequiresBothSwitches 验证 Server 或会话任一开关关闭时，
// Local Backend 都不会被创建。
func TestExecuteHostToolRequiresBothSwitches(t *testing.T) {
	created := false
	host := &executeHostTool{newBackend: func(context.Context) (localExecutor, error) {
		created = true
		return &fakeLocalExecutor{}, nil
	}}
	for _, scope := range []tool.ExecutionScope{
		{SessionID: "s", ProjectID: "p", WorkspaceRoot: t.TempDir(), HostExecutionAllowed: true},
		{SessionID: "s", ProjectID: "p", WorkspaceRoot: t.TempDir(), HostExecutionEnabled: true},
	} {
		_, err := host.Run(tool.WithExecutionScope(context.Background(), scope), `{"command":"pwd"}`)
		assertToolErrorCode(t, err, "HOST_EXECUTION_DISABLED")
	}
	if created {
		t.Fatal("权限不完整时不应创建 Local Backend")
	}
}

// TestResolveHostWorkdirRejectsSymlinkEscape 验证 workspace 内指向外部目录的
// 符号链接不能被用作宿主工作目录。
func TestResolveHostWorkdirRejectsSymlinkEscape(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Fatalf("创建测试符号链接失败: %v", err)
	}
	_, err := resolveHostWorkdir(workspace, "escape")
	assertToolErrorCode(t, err, "WORKDIR_OUTSIDE_PROJECT")
}

// TestExecuteHostLocalBackendIntegration 使用真实 Eino-ext Local Backend 验证
// 工作区文件回写，以及父 context 超时时直接 Shell 能被取消。
