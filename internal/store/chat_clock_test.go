// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
package store

import (
	"sync/atomic"
	"testing"
)

func TestMessageTimestampsSurviveCoarseAndBackwardClocks(t *testing.T) {
	var last atomic.Int64
	for i, now := range []int64{100, 100, 99, 101} {
		if got := monotonicMessageTimestamp(&last, now); got != int64(100+i) {
			t.Fatalf("timestamp %d: got %d", i, got)
		}
	}
}
