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
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestTerminalSnapshotCannotLoseConcurrentArtifactMappings(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC()
	execution := RobotExecution{
		ID: "rex-artifacts", ProjectID: "project-artifacts", RobotID: "robot-artifacts",
		PilotInstanceID: "pilot-artifacts", RequestKey: "artifacts-request", SubtaskID: "sub-artifacts",
		RunID: "run-artifacts", Status: "running", Revision: 1, CreatedAt: now, UpdatedAt: now,
		ArtifactRefs: []string{"artifact://input-evidence"},
	}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	// A lifecycle handler has read its snapshot, then pauses while two uploads
	// persist their independent mappings before it writes the terminal result.
	snapshotRead := make(chan struct{})
	uploadsDone := make(chan struct{})
	terminalDone := make(chan error, 1)
	go func() {
		terminal, err := st.GetRobotExecution(execution.ID)
		close(snapshotRead)
		if err != nil {
			terminalDone <- err
			return
		}
		<-uploadsDone
		terminal.Status = "completed"
		terminal.Result = map[string]any{"reached": true}
		terminal.Revision++
		terminalDone <- st.SaveRobotExecution(terminal)
	}()
	<-snapshotRead
	var uploads sync.WaitGroup
	for index := range 2 {
		uploads.Add(1)
		go func() {
			defer uploads.Done()
			if err := st.SaveRobotArtifactMapping(RobotArtifactMapping{
				PilotInstanceID: execution.PilotInstanceID, ExecutionID: execution.ID,
				LocalArtifactID: fmt.Sprintf("frame-%d", index), ServerArtifactID: fmt.Sprintf("image-%d", index),
				Status: "synced", MediaType: "image/png", UpdatedAt: now,
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	uploads.Wait()
	close(uploadsDone)
	if err := <-terminalDone; err != nil {
		t.Fatal(err)
	}
	// Pending, failed, another Pilot, and another Execution cannot become refs.
	for index, mapping := range []RobotArtifactMapping{
		{PilotInstanceID: execution.PilotInstanceID, ExecutionID: execution.ID, Status: "pending"},
		{PilotInstanceID: execution.PilotInstanceID, ExecutionID: execution.ID, Status: "failed"},
		{PilotInstanceID: "other-pilot", ExecutionID: execution.ID, Status: "synced"},
		{PilotInstanceID: execution.PilotInstanceID, ExecutionID: "other-execution", Status: "synced"},
	} {
		mapping.LocalArtifactID = fmt.Sprintf("excluded-%d", index)
		mapping.ServerArtifactID = fmt.Sprintf("excluded-image-%d", index)
		if err := st.SaveRobotArtifactMapping(mapping); err != nil {
			t.Fatal(err)
		}
	}
	assertExecution := func(item RobotExecution, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		refs := map[string]bool{}
		for _, ref := range item.ArtifactRefs {
			refs[ref] = true
		}
		if item.Status != "completed" || item.Result["reached"] != true || len(item.ArtifactRefs) != 3 ||
			!reflect.DeepEqual(refs, map[string]bool{"artifact://input-evidence": true, "artifact://image-0": true, "artifact://image-1": true}) {
			t.Fatalf("lifecycle or synced refs lost: %+v", item)
		}
	}
	assertExecution(st.GetRobotExecution(execution.ID))
	assertExecution(st.GetRobotExecutionByRequest(execution.ProjectID, execution.RequestKey))
	assertExecution(st.GetRobotExecutionBySubTask(execution.SubtaskID))
	for _, list := range []func() ([]RobotExecution, error){
		func() ([]RobotExecution, error) {
			return st.ListRobotExecutions(execution.ProjectID, execution.RobotID, 10)
		},
		func() ([]RobotExecution, error) { return st.ListRobotExecutionsByRun(execution.RunID) },
	} {
		items, err := list()
		if err != nil || len(items) != 1 {
			t.Fatalf("execution list: %v / %v", items, err)
		}
		assertExecution(items[0], nil)
	}
}
