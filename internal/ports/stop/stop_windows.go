//go:build windows

// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0

package stop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

func Identity(pid int) (string, error) {
	if pid <= 0 {
		return "", errors.New("invalid process id")
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)
	var created, exited, kernel, user windows.Filetime
	if err = windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return "", err
	}
	return fmt.Sprintf("%08x%08x", created.HighDateTime, created.LowDateTime), nil
}
func eventName(pid int, identity string) (*uint16, error) {
	return windows.UTF16PtrFromString(fmt.Sprintf(`Local\InsightOS.Semantic.Stop.%d.%s`, pid, identity))
}
func NotifyContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	identity, err := Identity(os.Getpid())
	if err != nil {
		return nil, nil, err
	}
	name, err := eventName(os.Getpid(), identity)
	if err != nil {
		return nil, nil, err
	}
	// Default process-token DACL; no broad Everyone access. A reused PID has a
	// different creation-time name. Reject an already-existing endpoint.
	event, err := windows.CreateEvent(nil, 1, 0, name)
	if err != nil {
		if event != 0 {
			windows.CloseHandle(event)
		}
		return nil, nil, err
	}
	ctx, cancel := signal.NotifyContext(parent, os.Interrupt)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer windows.CloseHandle(event)
		for ctx.Err() == nil {
			state, waitErr := windows.WaitForSingleObject(event, 50)
			if waitErr != nil || state == windows.WAIT_OBJECT_0 {
				cancel()
				return
			}
		}
	}()
	var once sync.Once
	return ctx, func() { once.Do(func() { cancel(); <-done }) }, nil
}
func Request(pid int, identity string) error {
	if identity == "" {
		return errors.New("missing Windows process creation identity; refusing stop")
	}
	actual, err := Identity(pid)
	if err != nil {
		return err
	}
	if actual != identity {
		return errors.New("process identity changed; refusing stop")
	}
	name, err := eventName(pid, identity)
	if err != nil {
		return err
	}
	// A child may not yet have entered main. Wait briefly for its stop endpoint,
	// never fall back to killing the process if it does not support this protocol.
	deadline := time.Now().Add(2 * time.Second)
	for {
		event, openErr := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
		if openErr == nil {
			defer windows.CloseHandle(event)
			return windows.SetEvent(event)
		}
		if !errors.Is(openErr, windows.ERROR_FILE_NOT_FOUND) || time.Now().After(deadline) {
			return fmt.Errorf("graceful stop endpoint unavailable: %w", openErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
