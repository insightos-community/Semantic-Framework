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
