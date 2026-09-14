//go:build linux || darwin

// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"
)

func terminateManagedProcessGroup(ctx context.Context, pid int, expectedExecutable string) error {
	running, err := managedExecutableRunning(pid, expectedExecutable)
	if err != nil || !running {
		return err
	}
	processGroupID, err := syscall.Getpgid(pid)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	if processGroupID != pid {
		return fmt.Errorf("PID %d 不是独立受管进程组", pid)
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		running, err = managedExecutableRunning(pid, expectedExecutable)
		if err != nil || !running {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
