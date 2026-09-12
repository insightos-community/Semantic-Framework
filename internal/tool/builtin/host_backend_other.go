//go:build !darwin

// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
package builtin

import (
	"context"
	"github.com/cloudwego/eino-ext/adk/backend/local"
)

func newPlatformHostBackend(ctx context.Context) (localExecutor, error) {
	return local.NewBackend(ctx, &local.Config{})
}

func hostSessionCommand() string { return "exec setsid /bin/sh -c " }
