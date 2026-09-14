//go:build linux || darwin

// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0

// Package stop separates a graceful application stop request from process death.
package stop

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func NotifyContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	return ctx, cancel, nil
}
func Identity(pid int) (string, error) { return "", nil }
func Request(pid int, identity string) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer process.Release()
	return process.Signal(syscall.SIGTERM)
}
