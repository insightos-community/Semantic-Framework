//go:build linux

// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
package bootstrap

import (
	"fmt"
	"os"
)

func managedProcessExecutable(pid int) (string, error) {
	return os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
}
