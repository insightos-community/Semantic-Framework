//go:build linux || darwin

// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0

// Package process owns child process trees. Closing a tree never terminates it;
// callers must confirm application safety before explicitly terminating a tree.
package process

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

type Tree struct{ pid int }

func Start(command *exec.Cmd) (*Tree, error) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return nil, err
	}
	return &Tree{pid: command.Process.Pid}, nil
}
func (tree *Tree) Close() error     { return nil }
func (tree *Tree) Terminate() error { return tree.signal(syscall.SIGTERM) }
func (tree *Tree) Kill() error      { return tree.signal(syscall.SIGKILL) }
func (tree *Tree) signal(signal syscall.Signal) error {
	err := syscall.Kill(-tree.pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
func (tree *Tree) Alive() (bool, error) {
	err := syscall.Kill(-tree.pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return err == nil, err
}
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, os.ErrPermission)
}

func (tree *Tree) Interrupt() error { return tree.signal(syscall.SIGINT) }
