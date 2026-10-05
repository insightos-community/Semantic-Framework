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
	"reflect"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

func TestActionAndArtifactSnapshotsCannotReplaceExecutionTerminal(t *testing.T) {
	for _, terminalStatus := range []string{"completed", "failed"} {
		t.Run(terminalStatus, func(t *testing.T) {
			st := openRobotTestStore(t)
			service := NewService(st, nil)
			_, disconnect, err := service.Connect(store.RobotPilot{
				PilotInstanceID: "pilot-evidence", RobotID: "robot-evidence", RobotStatus: "busy",
			})
			if err != nil {
				t.Fatal(err)
			}
			defer disconnect()
			now := time.Now().UTC()
			execution := store.RobotExecution{
				ID: "rex-evidence", ProjectID: "project-evidence", RobotID: "robot-evidence",
				PilotInstanceID: "pilot-evidence", SkillName: "semantic-navigation",
				SkillVersion: "0.4.7", RequestKey: "evidence", Status: "running",
				Input: map[string]any{}, Revision: 1, CreatedAt: now, UpdatedAt: now,
			}
			if err := st.SaveRobotExecution(execution); err != nil {
				t.Fatal(err)
			}
			pilot, _ := st.GetRobotPilot("pilot-evidence")
			service.setPilotExecution(pilot, execution.ID)
			photoResult := map[string]any{"artifact_candidates": []any{"photo-candidate"}}
			for index, eventType := range []string{"action.terminal", "stage.completed", "artifact.summary.announced", "artifact.summary.failed"} {
				if err := service.HandlePilotEvent("pilot-evidence", eventType, int64(index+47), map[string]any{
					"execution_id": execution.ID, "skill_status": terminalStatus,
					"status": "succeeded", "stage": "verify_arrival", "result": photoResult,
					"error": map[string]any{"code": "PHOTO_PUBLICATION_FAILED"},
				}); err != nil {
					t.Fatal(err)
				}
				current, _ := st.GetRobotExecution(execution.ID)
				currentPilot, _ := st.GetRobotPilot("pilot-evidence")
				if current.Status != "running" || len(current.Result) != 0 || len(current.Error) != 0 ||
					currentPilot.CurrentExecutionID != execution.ID || current.Stage != "verify_arrival" {
					t.Fatalf("%s incorrectly changed execution or released Robot: execution=%+v pilot=%+v", eventType, current, currentPilot)
				}
			}
			finalResult := map[string]any{"reached": terminalStatus == "completed", "target_ref": "target-1"}
			var finalError map[string]any
			if terminalStatus == "failed" {
				finalError = map[string]any{"code": "ARRIVAL_NOT_CONFIRMED"}
			}
			if err := service.HandlePilotEvent("pilot-evidence", "execution.terminal", 51, map[string]any{
				"execution_id": execution.ID, "skill_status": terminalStatus,
				"status": terminalStatus, "result": finalResult, "error": finalError,
			}); err != nil {
				t.Fatal(err)
			}
			// Late Action/Worker events retain their own result/error in the event stream only.
			for index, eventType := range []string{"action.terminal", "complete", "stage.evidence_unavailable"} {
				if err := service.HandlePilotEvent("pilot-evidence", eventType, int64(index+52), map[string]any{
					"execution_id": execution.ID, "skill_status": "running", "status": "failed",
					"result": photoResult, "error": map[string]any{"code": "LATE_ACTION_ERROR"},
				}); err != nil {
					t.Fatal(err)
				}
			}
			current, _ := st.GetRobotExecution(execution.ID)
			if current.Status != terminalStatus || !reflect.DeepEqual(current.Result, finalResult) ||
				!reflect.DeepEqual(current.Error, finalError) {
				t.Fatalf("formal Skill terminal was replaced: %+v", current)
			}
			events, err := st.ListRobotExecutionEvents(execution.ID, 0, 100)
			if err != nil || len(events) != 8 || !reflect.DeepEqual(events[0].Payload["result"], photoResult) {
				t.Fatalf("raw Action/Stage evidence was lost: events=%+v err=%v", events, err)
			}
		})
	}
}

func TestLateWorkerCompleteCannotRegressExecutionTerminal(t *testing.T) {
	st := openRobotTestStore(t)
	project, err := st.CreateProject("user-terminal", "terminal")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(st, nil)
	_, disconnect, err := service.Connect(store.RobotPilot{
		PilotInstanceID: "pilot-terminal", RobotID: "robot-terminal", RobotStatus: "busy",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()

	now := time.Now().UTC()
	execution := store.RobotExecution{
		ID: "rex-terminal", ProjectID: project.ID, RobotID: "robot-terminal",
		PilotInstanceID: "pilot-terminal", SkillName: "grasp-object",
		SkillVersion: "0.2.0", RequestKey: "terminal", Status: "running",
		Input: map[string]any{}, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	if err := service.HandlePilotEvent("pilot-terminal", "execution.terminal", 1, map[string]any{
		"execution_id": execution.ID,
		"skill_status": "completed",
		"stage":        "lift_and_verify",
		"result":       map[string]any{"held": true},
	}); err != nil {
		t.Fatal(err)
	}
	// 真实 Worker 在正式 execution.terminal 之后还可能送达进程 complete
	// 通知；其快照是收敛前的 running，不能推翻已经持久化的物理终态。
	if err := service.HandlePilotEvent("pilot-terminal", "complete", 2, map[string]any{
		"execution_id": execution.ID,
		"skill_status": "running",
		"status":       "completed",
		"result":       map[string]any{"held": true},
	}); err != nil {
		t.Fatal(err)
	}
	persisted, err := st.GetRobotExecution(execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != "completed" || persisted.Progress == nil || *persisted.Progress != 1 {
		t.Fatalf("late Worker complete regressed terminal execution: %#v", persisted)
	}
}

func TestReconcileRestoresPersistedTerminalBeforeInterrupting(t *testing.T) {
	st := openRobotTestStore(t)
	service := NewService(st, nil)
	_, disconnect, err := service.Connect(store.RobotPilot{
		PilotInstanceID: "pilot-recover-terminal", RobotID: "robot-recover-terminal",
		RobotStatus: "busy",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()

	now := time.Now().UTC()
	execution := store.RobotExecution{
		ID: "rex-recover-terminal", ProjectID: "project-recover-terminal",
		RobotID: "robot-recover-terminal", PilotInstanceID: "pilot-recover-terminal",
		SkillName: "grasp-object", SkillVersion: "0.2.0", RequestKey: "recover-terminal",
		Status: "running", Input: map[string]any{}, Revision: 3, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendRobotExecutionEvent(store.RobotExecutionEvent{
		ExecutionID: execution.ID,
		Sequence:    294,
		Type:        "execution.terminal",
		Payload: map[string]any{
			"execution_id": execution.ID,
			"skill_status": "completed",
			"stage":        "lift_and_verify",
			"result":       map[string]any{"held": true},
		},
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	pilot, err := st.GetRobotPilot("pilot-recover-terminal")
	if err != nil {
		t.Fatal(err)
	}
	service.setPilotExecution(pilot, execution.ID)

	recoverable, err := service.Reconcile("pilot-recover-terminal", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(recoverable) != 0 {
		t.Fatalf("terminal execution must not be returned as recoverable: %#v", recoverable)
	}
	persisted, err := st.GetRobotExecution(execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != "completed" || persisted.Result["held"] != true {
		t.Fatalf("persisted terminal was not restored: %#v", persisted)
	}
	updatedPilot, err := st.GetRobotPilot("pilot-recover-terminal")
	if err != nil {
		t.Fatal(err)
	}
	if updatedPilot.CurrentExecutionID != "" {
		t.Fatalf("restored terminal still holds Pilot execution: %#v", updatedPilot)
	}
}

func TestReconcileRepairsLegacyStoppedProjectionFromFormalCompletedEvent(t *testing.T) {
	st := openRobotTestStore(t)
	service := NewService(st, nil)
	_, disconnect, err := service.Connect(store.RobotPilot{
		PilotInstanceID: "pilot-repair-terminal", RobotID: "robot-repair-terminal",
		RobotStatus: "idle",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()

	now := time.Now().UTC()
	execution := store.RobotExecution{
		ID: "rex-repair-terminal", ProjectID: "project-repair-terminal",
		RobotID: "robot-repair-terminal", PilotInstanceID: "pilot-repair-terminal",
		SkillName: "grasp-object", SkillVersion: "0.2.0", RequestKey: "repair-terminal",
		// 旧 Server shutdown 曾在正式 completed 之后错误覆盖 stopped。
		Status: "stopped", Input: map[string]any{}, Revision: 5, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendRobotExecutionEvent(store.RobotExecutionEvent{
		ExecutionID: execution.ID,
		Sequence:    294,
		Type:        "execution.terminal",
		Payload: map[string]any{
			"execution_id": execution.ID,
			"skill_status": "completed",
			"stage":        "lift_and_verify",
			"result":       map[string]any{"held": true},
		},
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Reconcile("pilot-repair-terminal", nil); err != nil {
		t.Fatal(err)
	}
	persisted, err := st.GetRobotExecution(execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != "completed" || persisted.Stage != "lift_and_verify" ||
		persisted.Result["held"] != true {
		t.Fatalf("legacy stopped projection was not repaired: %#v", persisted)
	}
}

func TestReconcileReleasesAlreadyCompletedCurrentExecution(t *testing.T) {
	st := openRobotTestStore(t)
	service := NewService(st, nil)
	_, disconnect, err := service.Connect(store.RobotPilot{
		PilotInstanceID: "pilot-release-terminal", RobotID: "robot-release-terminal",
		Status: "online", RobotStatus: "idle", AbilityFrameworkStatus: "ready",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()

	now := time.Now().UTC()
	execution := store.RobotExecution{
		ID: "rex-release-terminal", ProjectID: "project-release-terminal",
		RobotID: "robot-release-terminal", PilotInstanceID: "pilot-release-terminal",
		SkillName: "place-object", SkillVersion: "0.3.0", RequestKey: "release-terminal",
		Status: "completed", Input: map[string]any{}, Result: map[string]any{"stable": true},
		Revision: 4, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	pilot, err := st.GetRobotPilot("pilot-release-terminal")
	if err != nil {
		t.Fatal(err)
	}
	service.setPilotExecution(pilot, execution.ID)
	observer := &recordingAvailabilityObserver{robots: make(chan string, 1)}
	service.SetAvailabilityObserver(observer)

	if _, err := service.Reconcile("pilot-release-terminal", nil); err != nil {
		t.Fatal(err)
	}
	updatedPilot, err := st.GetRobotPilot("pilot-release-terminal")
	if err != nil {
		t.Fatal(err)
	}
	if updatedPilot.CurrentExecutionID != "" || updatedPilot.RobotStatus != "idle" {
		t.Fatalf("terminal execution still holds Robot: %#v", updatedPilot)
	}
	select {
	case robotID := <-observer.robots:
		if robotID != "robot-release-terminal" {
			t.Fatalf("unexpected available Robot: %s", robotID)
		}
	case <-time.After(time.Second):
		t.Fatal("reconcile did not publish Robot availability")
	}
}

func TestReconcileKeepsInterruptedCurrentExecutionLocked(t *testing.T) {
	st := openRobotTestStore(t)
	service := NewService(st, nil)
	_, disconnect, err := service.Connect(store.RobotPilot{
		PilotInstanceID: "pilot-keep-interrupted", RobotID: "robot-keep-interrupted",
		Status: "online", RobotStatus: "idle", AbilityFrameworkStatus: "ready",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()

	now := time.Now().UTC()
	execution := store.RobotExecution{
		ID: "rex-keep-interrupted", ProjectID: "project-keep-interrupted",
		RobotID: "robot-keep-interrupted", PilotInstanceID: "pilot-keep-interrupted",
		SkillName: "grasp-object", SkillVersion: "0.3.0", RequestKey: "keep-interrupted",
		Status: "interrupted", Input: map[string]any{}, Revision: 2, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	pilot, err := st.GetRobotPilot("pilot-keep-interrupted")
	if err != nil {
		t.Fatal(err)
	}
	service.setPilotInterrupted(pilot, execution.ID)

	if _, err := service.Reconcile("pilot-keep-interrupted", nil); err != nil {
		t.Fatal(err)
	}
	updatedPilot, err := st.GetRobotPilot("pilot-keep-interrupted")
	if err != nil {
		t.Fatal(err)
	}
	if updatedPilot.CurrentExecutionID != execution.ID || updatedPilot.RobotStatus != "interrupted" {
		t.Fatalf("interrupted physical execution lock was released: %#v", updatedPilot)
	}
}
