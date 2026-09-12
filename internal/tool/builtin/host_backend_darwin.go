// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
package builtin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/cloudwego/eino/adk/filesystem"
)

// macOS has no setsid executable. Setpgid establishes the command's group using
// the native syscall, so cancellation also terminates background children.
type darwinHostBackend struct{}

func newPlatformHostBackend(context.Context) (localExecutor, error) {
	return darwinHostBackend{}, nil
}

func hostSessionCommand() string { return "exec /bin/sh -c " }

func (darwinHostBackend) Execute(ctx context.Context, input *filesystem.ExecuteRequest) (*filesystem.ExecuteResponse, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", input.Command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			return nil, err
		}
		code = exitError.ExitCode()
		parts := []string{fmt.Sprintf("command exited with non-zero code %d", code)}
		if stdout.Len() != 0 {
			parts = append(parts, "[stdout]:\n"+stdout.String())
		}
		if stderr.Len() != 0 {
			parts = append(parts, "[stderr]:\n"+stderr.String())
		}
		return &filesystem.ExecuteResponse{Output: strings.Join(parts, "\n"), ExitCode: &code}, nil
	}
	return &filesystem.ExecuteResponse{Output: stdout.String(), ExitCode: &code}, nil
}
