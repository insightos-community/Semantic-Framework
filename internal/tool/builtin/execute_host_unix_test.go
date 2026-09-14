//go:build linux || darwin

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
	"insightos.cn/semantic-framework/internal/tool"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

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
