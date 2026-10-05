package store

import (
	"fmt"
	"testing"
)

func TestListRunEventsAfterExactIdentityAndPagination(t *testing.T) {
	st := openTestStore(t)
	var expected []Event
	for index, sample := range []struct {
		project, parent, payload, channel string
		wanted                            bool
	}{
		{"project-a", `{"run_id":"run-a"}`, `{"run_id":"run-a","call_id":"call1"}`, "dialogue", true},
		{"project-a", `{"run_id":"run-b"}`, `{"run_id":"run-b"}`, "dialogue", false},
		{"project-b", `{"run_id":"run-a"}`, `{"run_id":"run-a"}`, "dialogue", false},
		{"project-a", `{"run_id":"run-a"}`, `{"call_id":"call1","result":"real result"}`, "dialogue", true},
		{"project-a", `{}`, `{"run_id":"run-a","content":"done"}`, "dialogue", true},
		{"project-a", `{"run_id":"run-a"}`, `{"run_id":"run-b"}`, "dialogue", false},
		{"project-a", `{"run_id":"run-b"}`, `{"run_id":"run-a"}`, "dialogue", false},
		{"project-a", `{}`, `{"run_id":"run-a-other","text":"run-a"}`, "dialogue", false},
		{"project-a", `{"run_id":"run-a"}`, `{"run_id":"run-a"}`, ChannelTrace, false},
	} {
		ev := newTestEvent(fmt.Sprintf("evt-%03d", index), "shared-conversation", sample.channel)
		ev.ProjectID, ev.Parent, ev.Payload = sample.project, sample.parent, sample.payload
		saved, err := st.InsertProjectEvent(ev)
		if err != nil {
			t.Fatal(err)
		}
		if sample.wanted {
			expected = append(expected, saved)
		}
	}
	first, err := st.ListRunEventsAfter("project-a", "run-a", 0, 2)
	if err != nil || len(first) != 2 {
		t.Fatalf("first = %+v, %v", first, err)
	}
	second, err := st.ListRunEventsAfter("project-a", "run-a", first[1].Sequence, 2)
	if err != nil || len(second) != 1 {
		t.Fatalf("second = %+v, %v", second, err)
	}
	for index, got := range append(first, second...) {
		if got != expected[index] {
			t.Fatalf("event changed: got %+v want %+v", got, expected[index])
		}
	}
	empty, err := st.ListRunEventsAfter("project-a", "run-a", 1000000, 100)
	if err != nil || len(empty) != 0 {
		t.Fatalf("tail = %+v, %v", empty, err)
	}
	for _, sample := range []struct {
		project, run string
		after        int64
		limit        int
	}{
		{"", "run-a", 0, 1}, {"project-a", "", 0, 1},
		{"project-a", "run-a", -1, 1}, {"project-a", "run-a", 0, 0},
		{"project-a", "run-a", 0, 502},
	} {
		if _, err := st.ListRunEventsAfter(sample.project, sample.run, sample.after, sample.limit); err == nil {
			t.Fatalf("invalid query accepted: %+v", sample)
		}
	}
}
