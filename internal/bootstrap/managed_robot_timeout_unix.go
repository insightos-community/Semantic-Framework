//go:build !windows

// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
package bootstrap

import "time"

const managedRobotReadinessTimeout = 2 * time.Minute
