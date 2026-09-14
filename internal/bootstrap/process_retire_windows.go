//go:build windows

// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
//
package bootstrap

import (
	"context"
	"fmt"
)

func terminateManagedProcessGroup(ctx context.Context, pid int, expectedExecutable string) error {
	running, err := managedExecutableRunning(pid, expectedExecutable)
	if err != nil || !running {
		return err
	}
	// No live owning Job handle after an interrupted supervisor exits. Do not
	// claim arbitrary same-executable processes or use taskkill as a substitute.
	return fmt.Errorf("Windows interrupted PID %d requires reconciliation; automatic orphan retirement is unavailable", pid)
}
