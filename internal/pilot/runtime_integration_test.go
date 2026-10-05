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

package pilot

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type scriptedAbilityClient struct {
	mu         sync.Mutex
	tasks      map[string]string
	startCount map[string]int
}

func newScriptedAbilityClient() *scriptedAbilityClient {
	return &scriptedAbilityClient{tasks: map[string]string{}, startCount: map[string]int{}}
}
func (f *scriptedAbilityClient) StartTask(_ context.Context, _ string, taskName string, input map[string]any) (AbilityTask, error) {
	invocation, _ := input["invocation_id"].(string)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks[invocation] = taskName
	f.startCount[taskName]++
	return AbilityTask{TaskID: "task-" + invocation}, nil
}
func (f *scriptedAbilityClient) GetExecution(_ context.Context, _ string, invocation string, _ int64) (AbilityExecution, error) {
	f.mu.Lock()
	task := f.tasks[invocation]
	f.mu.Unlock()
	outputs := map[string]map[string]any{
		"navigation.plan_route":     {"route_ref": "route://mock/1"},
		"navigation.follow_route":   {"final_pose_ref": "pose://mock/final", "distance_to_target_m": 0.1},
		"navigation.verify_arrival": {"verdict": "achieved", "final_pose_ref": "pose://mock/final", "distance_to_target_m": 0.1},
	}
	return AbilityExecution{Status: "succeeded", Result: outputs[task]}, nil
}
func (f *scriptedAbilityClient) StopExecution(_ context.Context, _ string, _ string, _ string) (AbilityExecution, error) {
	return AbilityExecution{Status: "stopped", Result: map[string]any{"safe": true, "physical_state": "base_stopped_and_braked"}}, nil
}

func TestSkillRuntimeRunsRealNavigationWorkerWithoutReplayingActions(t *testing.T) {
	repository := os.Getenv("SEMANTIC_ROBOT_SKILLS_DIR")
	if repository == "" {
		t.Skip("set SEMANTIC_ROBOT_SKILLS_DIR to run cross-process Robot Skill test")
	}
	skills, err := ScanSkillCatalog(filepath.Join(repository, "semantic_robot_skills", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	bindings := []AbilityBinding{
		binding("navigation.plan_route", "navigation-r1", false),
		binding("navigation.follow_route", "navigation-r1", true),
		binding("navigation.verify_arrival", "navigation-r1", false),
	}
	catalog := NewCatalog(profile("robot://r1pro/fake-1", bindings...))
	client := newScriptedAbilityClient()
	journal := NewMemoryActionJournal()
	runner := NewRunner(catalog, client, journal)
	store := NewMemorySkillExecutionStore()
	runtime := NewSkillRuntime(skills, catalog, runner, store, WorkerSupervisor{PythonExecutable: "python", PythonPaths: []string{repository}}, nil, nil, nil)
	input := map[string]any{
		"target": map[string]any{
			"target_ref": "semantic://station/loading-a",
			"pose":       map[string]any{"frame_id": "map", "position_m": []float64{1, 2, 0}, "orientation_xyzw": []float64{0, 0, 0, 1}, "revision": "target-r1"},
			"map_type":   "simulation", "map_generation": "generation-1", "map_revision": 1,
		},
		"require_visual_confirmation": true,
	}
	execution, err := runtime.Start(context.Background(), SkillStartRequest{ProjectID: "project-1", TaskID: "task-1", SubtaskID: "subtask-1", RobotID: "robot://r1pro/fake-1", SkillName: "semantic-navigation", Version: "0.1.0", Input: input})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finished, err := runtime.Wait(ctx, execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != SkillCompleted {
		t.Fatalf("unexpected terminal: %#v", finished)
	}
	if reached, _ := finished.Result["reached"].(bool); !reached {
		t.Fatalf("result missing reached: %#v", finished.Result)
	}
	for _, key := range []string{"plan-route:1", "follow-route:1", "verify-arrival:1"} {
		action, err := journal.GetAction(execution.ID, key)
		if err != nil {
			t.Fatalf("journal %s: %v", key, err)
		}
		if action.Status != ActionSucceeded {
			t.Fatalf("action %s: %#v", key, action)
		}
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	for task, count := range client.startCount {
		if count != 1 {
			t.Fatalf("task %s started %d times", task, count)
		}
	}
}

func TestSkillRuntimePersistsReportedStageBeforeFirstAction(t *testing.T) {
	store := NewMemorySkillExecutionStore()
	execution := SkillExecution{ID: "skill-stage-1", Status: SkillRunning}
	if err := store.SaveSkillExecution(execution); err != nil {
		t.Fatal(err)
	}
	runtime := NewSkillRuntime(nil, nil, nil, store, WorkerSupervisor{}, nil, nil, nil)
	_, err := runtime.handleWorker(context.Background(), execution.ID, nil, "event.report", map[string]any{
		"event": "stage.running",
		"stage": "verify_held_object",
	})
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := store.GetSkillExecution(execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Checkpoint["stage"] != "verify_held_object" {
		t.Fatalf("reported stage was not persisted: %#v", persisted.Checkpoint)
	}
}

func TestSkillRuntimeDoesNotReplayKnownExecutionID(t *testing.T) {
	store := NewMemorySkillExecutionStore()
	completed := SkillExecution{ID: "rex-completed", RobotID: "robot-1", SkillName: "place-object",
		SkillVersion: "0.3.0", Status: SkillCompleted, Result: map[string]any{"posture": "travel"}}
	if err := store.SaveSkillExecution(completed); err != nil {
		t.Fatal(err)
	}
	runtime := NewSkillRuntime(nil, nil, nil, store, WorkerSupervisor{}, nil, nil, nil)

	got, err := runtime.Start(context.Background(), SkillStartRequest{ExecutionID: completed.ID,
		RobotID: completed.RobotID, SkillName: completed.SkillName, Version: completed.SkillVersion})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != SkillCompleted || runtime.RobotInUse(completed.RobotID) {
		t.Fatalf("重复 start 不得重启 Worker 或重新占用 Robot: execution=%#v", got)
	}
}

type recordedRuntimeEvent struct {
	Event  string
	Fields map[string]any
}

type recordingRuntimeEventSink struct {
	events []recordedRuntimeEvent
}

func (s *recordingRuntimeEventSink) Report(_ SkillExecution, event string, fields map[string]any) {
	s.events = append(s.events, recordedRuntimeEvent{Event: event, Fields: fields})
}

func (*recordingRuntimeEventSink) Log(SkillExecution, string, string, map[string]any) {}

func TestSkillRuntimeReportsActionFeedbackIncrementallyByCursor(t *testing.T) {
	store := NewMemorySkillExecutionStore()
	execution := SkillExecution{
		ID: "skill-feedback-1", Status: SkillRunning,
		FeedbackCursors: map[string]int64{},
	}
	if err := store.SaveSkillExecution(execution); err != nil {
		t.Fatal(err)
	}
	events := &recordingRuntimeEventSink{}
	runtime := NewSkillRuntime(nil, nil, nil, store, WorkerSupervisor{}, nil, nil, events)
	action := ActionExecution{
		ID: "action-feedback-1", Key: "move",
		Action: ActionRef{Type: "motion.move_end_effector", SchemaVersion: 2},
		Feedback: []AbilityFeedback{
			{Sequence: 1, Status: "running", Progress: 0.2},
			{Sequence: 2, Status: "running", Progress: 0.4},
		},
	}

	runtime.reportActionFeedback(execution, action)
	persisted, err := store.GetSkillExecution(execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.FeedbackCursors[action.Key] != 2 || len(events.events) != 2 {
		t.Fatalf("feedback must be reported and persisted incrementally: execution=%#v events=%#v", persisted, events.events)
	}

	runtime.reportActionFeedback(persisted, action)
	if len(events.events) != 2 {
		t.Fatalf("persisted cursor must prevent duplicate feedback: %#v", events.events)
	}
}

func TestSkillRuntimeReportsWorkerInterruptionWithoutReleasingRobot(t *testing.T) {
	store := NewMemorySkillExecutionStore()
	execution := SkillExecution{ID: "skill-interrupted-1", RobotID: "robot-1", Status: SkillRunning}
	if err := store.SaveSkillExecution(execution); err != nil {
		t.Fatal(err)
	}
	events := &recordingRuntimeEventSink{}
	runtime := NewSkillRuntime(nil, nil, nil, store, WorkerSupervisor{}, nil, nil, events)
	runtime.activeRobots[execution.RobotID] = execution.ID

	runtime.interruptSkill(execution.ID, "WORKER_EXITED", "invalid skill input")

	persisted, err := store.GetSkillExecution(execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != SkillInterrupted || persisted.Error["code"] != "WORKER_EXITED" {
		t.Fatalf("unexpected interrupted execution: %#v", persisted)
	}
	if runtime.activeRobots[execution.RobotID] != execution.ID {
		t.Fatal("interrupted execution must retain the Robot lock until reconciliation")
	}
	if len(events.events) != 1 || events.events[0].Event != "execution.terminal" ||
		events.events[0].Fields["status"] != SkillInterrupted {
		t.Fatalf("interrupted terminal must be reported to Server: %#v", events.events)
	}
}

func TestSkillRuntimeReleasesRobotWhenWorkerFailsBeforePhysicalAction(t *testing.T) {
	store := NewMemorySkillExecutionStore()
	execution := SkillExecution{ID: "skill-invalid-input", RobotID: "robot-1", Status: SkillRunning}
	if err := store.SaveSkillExecution(execution); err != nil {
		t.Fatal(err)
	}
	events := &recordingRuntimeEventSink{}
	runtime := NewSkillRuntime(nil, nil, nil, store, WorkerSupervisor{}, nil, nil, events)
	runtime.active[execution.ID] = &activeSkill{actions: make(map[string]string), cancel: func() {}}
	runtime.activeRobots[execution.RobotID] = execution.ID

	runtime.failBeforePhysicalAction(execution.ID, "WORKER_EXITED", "invalid skill input")

	persisted, err := store.GetSkillExecution(execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != SkillFailed || persisted.Error["code"] != "WORKER_EXITED" {
		t.Fatalf("unexpected failed execution: %#v", persisted)
	}
	if runtime.activeRobots[execution.RobotID] != "" {
		t.Fatal("zero-action Worker failure must release the Robot")
	}
	if len(events.events) != 1 || events.events[0].Fields["status"] != SkillFailed {
		t.Fatalf("failed terminal must be reported to Server: %#v", events.events)
	}
}

func TestActionResultPayloadUsesEmptyListsInsteadOfNull(t *testing.T) {
	payload := actionResultPayload(ActionExecution{
		Status: ActionFailed,
		Result: map[string]any{"reason": "planning failed"},
	})

	observations, ok := payload["observations"].([]map[string]any)
	if !ok || observations == nil || len(observations) != 0 {
		t.Fatalf("ActionResult observations 必须编码为空数组: %#v", payload["observations"])
	}
	evidenceRefs, ok := payload["evidence_refs"].([]string)
	if !ok || evidenceRefs == nil || len(evidenceRefs) != 0 {
		t.Fatalf("ActionResult evidence_refs 必须编码为空数组: %#v", payload["evidence_refs"])
	}
}
