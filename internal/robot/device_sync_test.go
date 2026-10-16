package robot

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

func TestDeviceReplayAcrossSnapshotSubscribeRace(t *testing.T) {
	s := NewService(openRobotTestStore(t), nil)
	snapshot, err := s.DeviceSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	s.publishDevice("robot", "r1", "changed", 1, nil)
	ch, closeSubscription, err := s.SubscribeDevices(snapshot.EventSequence, snapshot.EventEpoch)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSubscription()
	s.publishDevice("robot", "r1", "changed", 2, nil)
	for sequence := int64(1); sequence <= 2; sequence++ {
		select {
		case event := <-ch:
			if event.Sequence != sequence || event.EventEpoch != snapshot.EventEpoch {
				t.Fatalf("event: %+v", event)
			}
		case <-time.After(time.Second):
			t.Fatal("lost event between snapshot and subscription")
		}
	}
	restarted := NewService(s.st, nil)
	if _, _, err := restarted.SubscribeDevices(0, snapshot.EventEpoch); err == nil {
		t.Fatal("accepted previous epoch")
	}
	if restarted.DeviceEventEpoch() == s.DeviceEventEpoch() {
		t.Fatal("epoch reused")
	}
}

func TestDeviceReplayExpiryAndSlowSubscriber(t *testing.T) {
	s := NewService(openRobotTestStore(t), nil)
	ch, closeSubscription, err := s.SubscribeDevices(0, s.DeviceEventEpoch())
	if err != nil {
		t.Fatal(err)
	}
	defer closeSubscription()
	for i := 0; i < deviceReplayLimit*2+1; i++ {
		s.publishDevice("robot", "r1", "changed", int64(i), nil)
	}
	if _, _, err := s.SubscribeDevices(0, s.DeviceEventEpoch()); err == nil {
		t.Fatal("expired replay accepted")
	}
	// A slow client is disconnected rather than left with silently stale state.
	for i := 0; i < deviceReplayLimit*2; i++ {
		<-ch
	}
	if _, ok := <-ch; ok {
		t.Fatal("slow subscription not closed")
	}
}

func TestDeviceConcurrentPublishUnsubscribe(t *testing.T) {
	s := NewService(openRobotTestStore(t), nil)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, stop, err := s.SubscribeDevices(0)
				if err != nil {
					t.Error(err)
					return
				}
				s.publishDevice("robot", "r1", "changed", int64(j), nil)
				stop()
				stop()
			}
		}()
	}
	wg.Wait()
}

func TestDeviceSnapshotSummariesPreserveFullExecutionDetails(t *testing.T) {
	st := openRobotTestStore(t)
	body := strings.Repeat("large-plan", 10000)
	execution := store.RobotExecution{ID: "rex-summary", ProjectID: "p1", RobotID: "r1", Status: "completed", Revision: 3,
		Input: map[string]any{"plan": body}, Result: map[string]any{"result": body}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewService(st, nil).DeviceSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 2048 || strings.Contains(string(encoded), `"input":`) || strings.Contains(string(encoded), `"result":`) {
		t.Fatalf("large payload leaked: %d", len(encoded))
	}
	if len(snapshot.Executions) != 1 || snapshot.Executions[0].ID != execution.ID || !strings.Contains(string(encoded), `"details_included":false`) {
		t.Fatalf("summary metadata lost: %s", encoded)
	}
	full, err := st.GetRobotExecution(execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if full.Input["plan"] != body || full.Result["result"] != body {
		t.Fatal("full detail changed")
	}
}
