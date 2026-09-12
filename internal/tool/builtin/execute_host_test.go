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
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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
	if !strings.HasPrefix(fake.command, "cd -- "+shellSingleQuote(wantDir)+" && "+hostSessionCommand()) ||
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
func TestExecuteHostLocalBackendIntegration(t *testing.T) {
	workspace := t.TempDir()
	host := newExecuteHostTool()
	ctx := hostExecutionContext(workspace)
	result, err := host.Run(ctx, `{"command":"printf 'host-ok' > result.txt && cat result.txt","timeout_seconds":5}`)
	if err != nil || !strings.Contains(result, "host-ok") {
		t.Fatalf("真实 Local Backend 执行失败: result=%s err=%v", result, err)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "result.txt"))
	if err != nil || string(content) != "host-ok" {
		t.Fatalf("宿主输出未写入 Project workspace: content=%q err=%v", content, err)
	}
	skillsRoot, err := filepath.Abs(filepath.Join("..", "..", "..", "configs", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	diagnosticsScript := filepath.Join(skillsRoot, "system", "semantic-diagnostics", "scripts", "diagnose.py")
	diagnosticsCommand := "python3 " + shellSingleQuote(diagnosticsScript) +
		" --server-url http://127.0.0.1:1 --json-output diagnostics.json --markdown-output diagnostics.md"
	diagnosticsResult, err := host.Run(ctx, `{"command":`+strconv.Quote(diagnosticsCommand)+`,"timeout_seconds":10}`)
	if err != nil || !strings.Contains(diagnosticsResult, "diagnostics.json") {
		t.Fatalf("semantic-diagnostics 未在真实 Local Backend 中成功运行: result=%s err=%v", diagnosticsResult, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "diagnostics.json")); err != nil {
		t.Fatalf("Local Backend 未写回诊断报告: %v", err)
	}

	cancelCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = host.Run(cancelCtx, `{"command":"sleep 10","timeout_seconds":5}`)
	var toolErr *tool.Error
	if !errors.As(err, &toolErr) || toolErr.Code != "EXECUTION_TIMEOUT" {
		t.Fatalf("父 context 超时应取消直接 Shell: %v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("取消响应过慢: %s", time.Since(started))
	}

	// 后台子进程仍属于本次 setsid 进程组，取消后也必须消失，不能只杀
	// Local Backend 启动的外层 Shell。
	groupCtx, groupCancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer groupCancel()
	_, err = host.Run(groupCtx,
		`{"command":"sleep 10 & echo $! > child.pid; wait","timeout_seconds":5}`)
	if !errors.As(err, &toolErr) || toolErr.Code != "EXECUTION_TIMEOUT" {
		t.Fatalf("后台子进程场景应按超时取消: %v", err)
	}
	pidText, err := os.ReadFile(filepath.Join(workspace, "child.pid"))
	if err != nil {
		t.Fatalf("读取子进程 PID 失败: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidText)))
	if err != nil {
		t.Fatalf("子进程 PID 非法: %q", pidText)
	}
	deadline := time.Now().Add(time.Second)
	for {
		probeErr := syscall.Kill(pid, 0)
		if errors.Is(probeErr, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("取消后不应残留宿主子进程 pid=%d err=%v", pid, probeErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
