//go:build linux || darwin

// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
//
package builtin

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

func platformHostCommand(workdir, pidFile, command string) string {
	inner := "echo $$ > " + shellSingleQuote(pidFile) + " && exec /bin/sh -c " + shellSingleQuote(command)
	return "cd -- " + shellSingleQuote(workdir) + " && " + hostSessionCommand() + shellSingleQuote(inner)
}
func killHostProcessGroup(pidFile string) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
