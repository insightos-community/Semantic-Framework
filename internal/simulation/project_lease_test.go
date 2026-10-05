// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package simulation

import (
	"context"
	"sync"
	"testing"
	"time"
)

type recordingProjectReleaser struct {
	mu       sync.Mutex
	released []string
	events   chan string
}

func newRecordingProjectReleaser() *recordingProjectReleaser {
	return &recordingProjectReleaser{events: make(chan string, 8)}
}

func (r *recordingProjectReleaser) ReleaseProject(_ context.Context, projectID string) error {
	r.mu.Lock()
	r.released = append(r.released, projectID)
	r.mu.Unlock()
	r.events <- projectID
	return nil
}

func expectNoLeaseRelease(t *testing.T, events <-chan string, wait time.Duration) {
	t.Helper()
	select {
	case projectID := <-events:
		t.Fatalf("不应释放 Project，实际释放 %q", projectID)
	case <-time.After(wait):
	}
}

func expectLeaseRelease(t *testing.T, events <-chan string, projectID string) {
	t.Helper()
	select {
	case actual := <-events:
		if actual != projectID {
			t.Fatalf("释放 Project=%q，期望 %q", actual, projectID)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("等待释放 Project %q 超时", projectID)
	}
}

func TestProjectLeaseReleasesOnlyAfterLastTabLeaves(t *testing.T) {
	releaser := newRecordingProjectReleaser()
	manager := NewProjectLeaseManager(releaser, 15*time.Millisecond)
	defer manager.Close()

	manager.Connected("project-a")
	manager.Connected("project-a")
	manager.Disconnected("project-a")
	expectNoLeaseRelease(t, releaser.events, 30*time.Millisecond)

	manager.Disconnected("project-a")
	expectLeaseRelease(t, releaser.events, "project-a")
}

func TestProjectLeaseReconnectCancelsPendingRelease(t *testing.T) {
	releaser := newRecordingProjectReleaser()
	manager := NewProjectLeaseManager(releaser, 30*time.Millisecond)
	defer manager.Close()

	manager.Connected("project-a")
	manager.Disconnected("project-a")
	time.Sleep(5 * time.Millisecond)
	manager.Connected("project-a")
	expectNoLeaseRelease(t, releaser.events, 50*time.Millisecond)

	manager.Disconnected("project-a")
	expectLeaseRelease(t, releaser.events, "project-a")
}

func TestProjectLeaseCloseCancelsPendingTimers(t *testing.T) {
	releaser := newRecordingProjectReleaser()
	manager := NewProjectLeaseManager(releaser, 15*time.Millisecond)

	manager.Connected("project-a")
	manager.Disconnected("project-a")
	manager.Close()
	expectNoLeaseRelease(t, releaser.events, 30*time.Millisecond)
}
