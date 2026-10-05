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

package robot

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

type gatedArtifactReader struct {
	reader  io.Reader
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (r *gatedArtifactReader) Read(buffer []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return r.reader.Read(buffer)
}

type concurrentArtifactEvents struct {
	mu         sync.Mutex
	executions []store.RobotExecution
}

func (s *concurrentArtifactEvents) PublishRobotEvent(_, _, _, eventType string, _ int64, payload any) {
	if eventType != "robot.artifact.synced" {
		return
	}
	execution := payload.(map[string]any)["execution"].(store.RobotExecution)
	s.mu.Lock()
	s.executions = append(s.executions, execution)
	s.mu.Unlock()
}

func TestParallelArtifactStreamsCannotRegressCompletedExecution(t *testing.T) {
	st := openRobotTestStore(t)
	project, err := st.CreateProject("user-stream-race", "stream-race")
	if err != nil {
		t.Fatal(err)
	}
	events := &concurrentArtifactEvents{}
	service := NewService(st, events)
	commands, disconnect, err := service.Connect(store.RobotPilot{
		PilotInstanceID: "pilot-stream-race", RobotID: "robot-stream-race", RobotStatus: "idle",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	now := time.Now().UTC()
	execution := store.RobotExecution{
		ID: "rex-stream-race", ProjectID: project.ID, RobotID: "robot-stream-race",
		PilotInstanceID: "pilot-stream-race", SkillName: "semantic-navigation", SkillVersion: "0.4.7",
		RequestKey: "stream-race", Status: "running", Input: map[string]any{},
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	pilot, _ := st.GetRobotPilot(execution.PilotInstanceID)
	service.setPilotExecution(pilot, execution.ID)
	if err := st.CheckRobotAdmission(execution.RobotID, "", ""); err == nil {
		t.Fatal("running Skill must retain Robot reservation")
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	responses := make(chan *httptest.ResponseRecorder, 2)
	for _, localID := range []string{"rgb-first", "rgb-second"} {
		if err := service.HandlePilotEvent(execution.PilotInstanceID, "artifact.announce", 0, map[string]any{
			"execution_id": execution.ID, "local_artifact_id": localID,
			"media_type": "image/png", "summary": "Stage RGB", "size_bytes": 8,
		}); err != nil {
			t.Fatal(err)
		}
		upload := receiveCommand(t, commands, "artifact.upload")
		body := &gatedArtifactReader{
			reader: strings.NewReader("PNGFRAME"), started: make(chan struct{}), release: release,
		}
		request := httptest.NewRequest(http.MethodPut, upload.Payload.(map[string]any)["upload_url"].(string), body)
		go func() {
			recorder := httptest.NewRecorder()
			service.TransferHandler(recorder, request, execution.PilotInstanceID)
			responses <- recorder
		}()
		select {
		case <-body.started: // TransferHandler has already loaded the old running snapshot.
		case <-time.After(3 * time.Second):
			t.Fatal("upload did not reach the gated stream")
		}
	}
	finalResult := map[string]any{"reached": true, "target_ref": "placement-station"}
	if err := service.HandlePilotEvent(execution.PilotInstanceID, "execution.terminal", 113, map[string]any{
		"execution_id": execution.ID, "skill_status": "completed", "status": "completed", "result": finalResult,
	}); err != nil {
		t.Fatal(err)
	}
	terminal, _ := st.GetRobotExecution(execution.ID)
	releaseOnce.Do(func() { close(release) })
	for range 2 {
		select {
		case response := <-responses:
			if response.Code != http.StatusOK {
				t.Fatalf("upload failed: status=%d body=%s", response.Code, response.Body.String())
			}
		case <-time.After(3 * time.Second):
			t.Fatal("upload did not finish")
		}
	}
	current, err := st.GetRobotExecution(execution.ID)
	if err != nil || current.Status != "completed" || !reflect.DeepEqual(current.Result, finalResult) ||
		current.Revision != terminal.Revision || !current.UpdatedAt.Equal(terminal.UpdatedAt) {
		t.Fatalf("uploads overwrote the lifecycle snapshot: current=%+v terminal=%+v err=%v", current, terminal, err)
	}
	if len(current.ArtifactRefs) != 2 || len(current.ArtifactSync) != 2 || current.ArtifactRefs[0] == current.ArtifactRefs[1] {
		t.Fatalf("parallel uploads lost a reference: %+v", current)
	}
	for _, mapping := range current.ArtifactSync {
		if mapping.Status != "synced" || mapping.ServerArtifactID == "" {
			t.Fatalf("incomplete artifact mapping: %+v", mapping)
		}
	}
	events.mu.Lock()
	for _, emitted := range events.executions {
		if emitted.Status != "completed" || !reflect.DeepEqual(emitted.Result, finalResult) {
			t.Errorf("sync event published the pre-upload running snapshot: %+v", emitted)
		}
	}
	if len(events.executions) != 2 {
		t.Errorf("missing sync execution events: %d", len(events.executions))
	}
	events.mu.Unlock()
	if err := st.CheckRobotAdmission(execution.RobotID, "", ""); err != nil {
		t.Fatalf("completed Skill was made busy again by image uploads: %v", err)
	}
	next := execution
	next.ID, next.RequestKey, next.Status = "rex-next-skill", "next-skill", "queued"
	if _, created, err := st.AdmitRobotExecution(next, ""); err != nil || !created {
		t.Fatalf("next Skill could not be admitted after capture sync: created=%v err=%v", created, err)
	}
}
