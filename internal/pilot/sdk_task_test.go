package pilot

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestSDKTaskStartsWithoutPollingAndRestoresExactTask(t *testing.T) {
	starts, polls, cancels := 0, 0, 0
	status := "running"
	stopped := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/ability/exact-instance/") {
			t.Errorf("wrong routing: %s", r.URL.Path)
		}
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		switch {
		case strings.HasSuffix(r.URL.Path, "/start"):
			starts++
			input := request["input"].(map[string]any)
			if len(input) != 1 || input["goal"] != "target" {
				t.Errorf("business input was changed: %v", input)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "original-task"})
		case strings.HasSuffix(r.URL.Path, "/status"):
			polls++
			if request["task_id"] != "original-task" {
				t.Errorf("lost task: %v", request)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "original-task", "status": status, "payload": map[string]any{"ok": true}, "stop_evidence": map[string]any{"confirmed": stopped}})
		case strings.HasSuffix(r.URL.Path, "/cancel"):
			cancels++
			status = "cancel_requested"
			_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client := NewAbilityFrameworkClient(server.URL, []AbilityTaskCatalog{{InstanceID: "exact-instance", TaskTypes: map[string]int{"move": 7}}})
	catalog := NewCatalog()
	ref := ActionRef{Type: "r1pro.navigate.navigate", SchemaVersion: 1}
	catalog.Replace(RobotProfile{RobotID: "robot", Bindings: map[string]AbilityBinding{ref.Key(): {Action: ref, AbilityName: "R1Pro.Navigate", InstanceID: "exact-instance", TaskName: "move", Physical: true, ExecutionProtocol: SDKTaskProtocol}}})
	path := filepath.Join(t.TempDir(), "pilot.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(catalog, client, journal)
	request := ActionRequest{RobotID: "robot", SkillExecutionID: "skill", Key: "once", Action: ref, Input: map[string]any{"goal": "target"}}
	execution, err := runner.StartAction(context.Background(), request)
	if err != nil || starts != 1 || polls != 0 {
		t.Fatalf("start blocked or failed: %+v %v %d %d", execution, err, starts, polls)
	}
	// A fresh Runner uses the persisted protocol + exact task ID, not a fresh start.
	_ = db.Close()
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	journal, err = NewSQLiteStore(db)
	if err != nil {
		t.Fatal(err)
	}
	runner = NewRunner(catalog, client, journal)
	if _, err = runner.StartAction(context.Background(), request); err != nil || starts != 1 {
		t.Fatalf("replayed task: %v", err)
	}
	state, err := runner.Stop(context.Background(), execution.ID, "user")
	if err != nil || state.Status != ActionStopping || cancels != 1 {
		t.Fatalf("cancel acknowledgement became terminal: %+v %v", state, err)
	}
	status = "cancelled"
	state, err = runner.Refresh(context.Background(), execution.ID)
	if err != nil || state.Status != ActionInterrupted {
		t.Fatalf("missing stop evidence released motion: %+v %v", state, err)
	}
	stopped = true
	state, err = runner.Stop(context.Background(), execution.ID, "reconcile")
	if err != nil || state.Status != ActionStopped || starts != 1 {
		t.Fatalf("failed to reconcile stop: %+v %v", state, err)
	}
}

func TestSDKReadTaskNeedsNoPhysicalStopEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "capture", "status": "completed", "payload": map[string]any{"frame_id": "frame"}})
	}))
	defer server.Close()
	result, err := NewAbilityFrameworkClient(server.URL, nil).QuerySDKTask(context.Background(), "camera", "capture", false)
	if err != nil || result.Status != "succeeded" || result.Result["frame_id"] != "frame" {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestSDKFailureStopEvidenceSurvivesJournalAndActionEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/start") {
			_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "original-task"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"task_id": "original-task", "status": "failed",
			"error": map[string]any{"code": "command_rejected", "message": "尚未进入已配置的直立运输姿态"},
			"stop_evidence": map[string]any{"confirmed": true, "reason": "feedback confirms hold",
				"observed_at": map[string]any{"clock": "unix", "seconds": 1791879881, "nanoseconds": 880902767}},
		})
	}))
	defer server.Close()
	client := NewAbilityFrameworkClient(server.URL, []AbilityTaskCatalog{{InstanceID: "exact-instance", TaskTypes: map[string]int{"move": 0}}})
	ref := ActionRef{Type: "r1pro.navigate.navigate", SchemaVersion: 1}
	catalog := NewCatalog(RobotProfile{RobotID: "robot", Bindings: map[string]AbilityBinding{ref.Key(): {
		Action: ref, InstanceID: "exact-instance", TaskName: "move", Physical: true, ExecutionProtocol: SDKTaskProtocol}}})
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "pilot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	journal, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(catalog, client, journal)
	execution, err := runner.StartAction(context.Background(), ActionRequest{RobotID: "robot", SkillExecutionID: "skill", Key: "invoke", Action: ref})
	if err != nil {
		t.Fatal(err)
	}
	skills := NewMemorySkillExecutionStore()
	_ = skills.SaveSkillExecution(SkillExecution{ID: "skill", RobotID: "robot"})
	events := &recordingRuntimeEventSink{}
	runtime := NewSkillRuntime(nil, catalog, runner, skills, WorkerSupervisor{}, nil, nil, events)
	active := &activeSkill{actions: map[string]string{"invoke": execution.ID}}
	payload, err := runtime.waitActionResult(context.Background(), "skill", active, execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if payload["status"] != "failed" || payload["physical_effect"] != "unknown" || payload["error_code"] != "command_rejected" {
		t.Fatalf("failure or physical effects were misrepresented: %+v", payload)
	}
	persisted, err := journal.GetActionByID(execution.ID)
	if err != nil || persisted.StopEvidence["confirmed"] != true || persisted.Result != nil {
		t.Fatalf("evidence lost or business payload changed: %+v %v", persisted, err)
	}
	var terminal map[string]any
	for _, e := range events.events {
		if e.Event == "action.terminal" {
			terminal = e.Fields
		}
	}
	evidence, _ := terminal["stop_evidence"].(map[string]any)
	if evidence["confirmed"] != true || evidence["observed_at"] == nil || terminal["framework_task_id"] != "original-task" || terminal["ability_instance_id"] != "exact-instance" || terminal["execution_protocol"] != SDKTaskProtocol {
		t.Fatalf("Action receipt lost evidence or identity: %+v", terminal)
	}
}

func TestSDKStopEvidenceCannotConfirmAnotherTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "different-task", "status": "failed", "stop_evidence": map[string]any{"confirmed": true}})
	}))
	defer server.Close()
	_, err := NewAbilityFrameworkClient(server.URL, nil).QuerySDKTask(context.Background(), "instance", "original-task", true)
	if err == nil {
		t.Fatal("accepted stop evidence belonging to another task")
	}
}
