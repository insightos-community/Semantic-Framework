//go:build windows

// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
//
package builtin

import (
	"context"
	"errors"
	"insightos.cn/semantic-framework/internal/tool"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWindowsHostPowerShellAndCancellation(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "中文 path's space")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	host := newExecuteHostTool()
	ctx := hostExecutionContext(workspace)
	result, err := host.Run(ctx, `{"command":"[IO.File]::WriteAllText((Join-Path (Get-Location) 'result.txt'), 'host-ok'); Get-Content result.txt","timeout_seconds":10}`)
	if err != nil || !strings.Contains(result, "host-ok") {
		t.Fatalf("result=%s err=%v", result, err)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "result.txt"))
	if err != nil || string(content) != "host-ok" {
		t.Fatalf("file=%q err=%v", content, err)
	}
	cancelCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = host.Run(cancelCtx, `{"command":"Start-Sleep -Seconds 60","timeout_seconds":10}`)
	var toolErr *tool.Error
	if !errors.As(err, &toolErr) || toolErr.Code != "EXECUTION_TIMEOUT" {
		t.Fatalf("unexpected cancellation: %v", err)
	}
	if time.Since(started) > 4*time.Second {
		t.Fatal("cancellation was not bounded")
	}
}
