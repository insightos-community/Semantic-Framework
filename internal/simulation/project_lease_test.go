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
