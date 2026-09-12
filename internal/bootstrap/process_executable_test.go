// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
package bootstrap

import (
	"os"
	"os/exec"
	"testing"
)

func TestManagedExecutableIdentityOnNativeHost(t *testing.T) {
	executable, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	running, err := managedExecutableRunning(command.Process.Pid, executable)
	if err != nil || !running {
		t.Fatalf("owned process identity: running=%v err=%v", running, err)
	}
	other, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := managedExecutableRunning(command.Process.Pid, other); err == nil {
		t.Fatal("must reject a PID belonging to a different executable")
	}
}
