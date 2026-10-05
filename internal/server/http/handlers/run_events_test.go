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

package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

func TestRunEventsHTTPExactRunPaginationAndOwnership(t *testing.T) {
	st, _, router, _ := newProjectsTestRouter(t)
	for _, user := range []string{"usr-project", "other-user"} {
		sessionID := "session-" + user
		if err := st.CreateChatSession(store.ChatSession{
			ID: sessionID, UserID: user, Title: "Run events", CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{"one", "two"} {
			if err := st.CreateRunSession(store.RunSession{
				ID: "run-" + user + "-" + suffix, ChatSessionID: sessionID,
				AgentName: "leader", Status: store.RunStatusCompleted, StartedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	run, err := st.GetRunSession("run-usr-project-one")
	if err != nil {
		t.Fatal(err)
	}
	for index, name := range []string{"tool.call", "tool.result", "message.done"} {
		payload, _ := json.Marshal(map[string]any{"run_id": run.ID, "call_id": "call-real", "value": "真实工具结果"})
		if err := st.InsertEvent(store.Event{
			ID: fmt.Sprintf("evt-%03d", index), ProjectID: run.ProjectID,
			SessionID: run.ChatSessionID, Channel: "dialogue", Type: name,
			AgentID: "leader", AgentRole: "coordinator", Parent: `{"run_id":"` + run.ID + `","trace_id":"trace-a"}`,
			Payload: string(payload), Ts: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.InsertEvent(store.Event{
		ID: "evt-other-run", ProjectID: run.ProjectID, SessionID: run.ChatSessionID,
		Channel: "dialogue", Type: "tool.result", Parent: `{"run_id":"run-usr-project-two"}`,
		Payload: `{"run_id":"run-usr-project-two","result":"must not leak"}`, Ts: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/runs/" + run.ID + "/events"
	status, body := doJSON(t, router, http.MethodGet, path+"?limit=2", "", "")
	if status != http.StatusOK || body["has_more"] != true {
		t.Fatalf("first page: %d %+v", status, body)
	}
	page := body["events"].([]any)
	if len(page) != 2 {
		t.Fatalf("first page size: %+v", body)
	}
	first := page[0].(map[string]any)
	if first["project_id"] != run.ProjectID || first["type"] != "tool.call" ||
		first["parent"].(map[string]any)["run_id"] != run.ID ||
		first["payload"].(map[string]any)["call_id"] != "call-real" ||
		first["agent"].(map[string]any)["id"] != "leader" {
		t.Fatalf("envelope mismatch: %+v", first)
	}
	status, body = doJSON(t, router, http.MethodGet,
		fmt.Sprintf("%s?limit=2&after_sequence=%.0f", path, body["next_sequence"]), "", "")
	if status != http.StatusOK || body["has_more"] != false || len(body["events"].([]any)) != 1 {
		t.Fatalf("last page: %d %+v", status, body)
	}
	status, body = doJSON(t, router, http.MethodGet, path+"?after_sequence=999999", "", "")
	if status != http.StatusOK || len(body["events"].([]any)) != 0 || body["next_sequence"] != float64(999999) {
		t.Fatalf("empty page: %d %+v", status, body)
	}
	for _, query := range []string{"after_sequence=-1", "after_sequence=x", "limit=0", "limit=501", "limit=x"} {
		status, _ := doJSON(t, router, http.MethodGet, path+"?"+query, "", "")
		if status != http.StatusBadRequest {
			t.Fatalf("query %s: status %d", query, status)
		}
	}
	for _, id := range []string{"run-missing", "run-other-user-one"} {
		status, body := doJSON(t, router, http.MethodGet, "/api/v1/runs/"+id+"/events", "", "")
		if status != http.StatusNotFound || body["error"].(map[string]any)["code"] != "RUN_NOT_FOUND" {
			t.Fatalf("ownership leak for %s: %d %+v", id, status, body)
		}
	}
}
