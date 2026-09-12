//go:build darwin && !cgo

// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
package bootstrap

import "errors"

func managedProcessExecutable(int) (string, error) {
	return "", errors.New("macOS managed process identity requires a CGO-enabled build")
}
