//go:build windows

// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
//
package builtin

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/cloudwego/eino/adk/filesystem"
	processport "insightos.cn/semantic-framework/internal/ports/process"
	"os/exec"
	"strings"
	"time"
	"unicode/utf16"
)

type windowsHostBackend struct{}

func newPlatformHostBackend(context.Context) (localExecutor, error) { return windowsHostBackend{}, nil }
func platformHostCommand(workdir, pidFile, command string) string {
	return "$ErrorActionPreference = 'Stop'; $global:LASTEXITCODE = 0; Set-Location -LiteralPath '" + strings.ReplaceAll(workdir, "'", "''") + "'; & { " + command + " }; if (-not $?) { exit 1 }; exit $LASTEXITCODE"
}

// Windows cancellation is handled by the backend's owned Job, not a PID file.
func killHostProcessGroup(string) {}
func (windowsHostBackend) Execute(ctx context.Context, input *filesystem.ExecuteRequest) (*filesystem.ExecuteResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	text := utf16.Encode([]rune(input.Command))
	bytes := make([]byte, len(text)*2)
	for i, c := range text {
		binary.LittleEndian.PutUint16(bytes[2*i:], c)
	}
	command := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(bytes))
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	command.WaitDelay = 2 * time.Second
	tree, err := processport.Start(command)
	if err != nil {
		return nil, err
	}
	defer tree.Close()
	defer tree.Kill()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = tree.Kill()
		<-done
		return nil, ctx.Err()
	}
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return nil, err
		}
		code = exit.ExitCode()
	}
	output := stdout.String()
	if code != 0 {
		output = fmt.Sprintf("command exited with non-zero code %d\n[stdout]:\n%s\n[stderr]:\n%s", code, output, stderr.String())
	}
	return &filesystem.ExecuteResponse{Output: output, ExitCode: &code}, nil
}
