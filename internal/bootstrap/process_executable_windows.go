//go:build windows

// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
//
package bootstrap

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func managedProcessExecutable(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return "", os.ErrNotExist
	}
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if err = windows.QueryFullProcessImageName(h, 0, &buffer[0], &size); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buffer[:size]), nil
}
