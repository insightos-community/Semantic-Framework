package pilot

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"insightos.cn/semantic-framework/internal/store"
)

func TestOperatorStopReconcilesPersistedPhysicalOwnerWithoutReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pilot.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatal(err)
	}
	e := SkillExecution{ID: "old", RobotID: "r1", Status: SkillInterrupted, Error: map[string]any{"code": "ABILITY_STOP_UNCONFIRMED"}}
	a := ActionExecution{ID: "old-action", SkillExecutionID: e.ID, RobotID: e.RobotID, Key: "invoke", ExecutionProtocol: SDKTaskProtocol, Physical: true, Status: ActionInterrupted, Error: map[string]any{"code": "STOP_UNCONFIRMED"}}
	if err := st.SaveSkillExecution(e); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAction(a); err != nil {
		t.Fatal(err)
	}
	client := &fakeAbilityClient{}
	runner := NewRunner(testCatalog(), client, st)
	runtime := NewSkillRuntime(nil, nil, runner, st, WorkerSupervisor{}, nil, nil, nil)
	runtime.activeRobots[e.RobotID] = e.ID
	snapshots, err := runtime.ReconciliationSnapshots()
	if err != nil || len(snapshots) != 1 || snapshots[0]["execution_id"] != e.ID {
		t.Fatalf("missing unknown owner: %v %v", snapshots, err)
	}
	confirmation := map[string]any{"confirmation_source": "operator", "safe": true, "hold_confirmed": true, "confirmed_by": "user", "confirmation_reason": "现场已停止、空夹"}
	if err := runtime.ConfirmOperatorStop(e.ID, "wrong-robot", confirmation); err == nil {
		t.Fatal("wrong robot accepted")
	}
	if runner.physicalOwner(e.RobotID) == "" {
		t.Fatal("wrong robot released lock")
	}
	remote := &RemoteClient{config: RemoteClientConfig{Pilot: store.RobotPilot{RobotID: e.RobotID, PilotInstanceID: "pilot"}}, runtime: runtime}
	body, _ := json.Marshal(map[string]any{"executions": []store.RobotExecution{{ID: e.ID, RobotID: e.RobotID, PilotInstanceID: "pilot", Status: "stopped", Result: confirmation}}})
	for i := 0; i < 2; i++ {
		if err := remote.handleCommand(context.Background(), serverCommand{Type: "reconcile.result", Payload: body}); err != nil {
			t.Fatal(err)
		}
	}
	if runtime.RobotInUse(e.RobotID) || runner.physicalOwner(e.RobotID) != "" {
		t.Fatal("old lock remains")
	}
	if client.startCalls != 0 {
		t.Fatal("replayed a physical action")
	}
	got, _ := st.GetSkillExecution(e.ID)
	if got.Status != SkillInterrupted || got.Error["code"] != "ABILITY_STOP_UNCONFIRMED" || got.StopOutcome["confirmation_source"] != "operator" {
		t.Fatalf("lost original diagnostic or operator evidence: %#v", got)
	}
	db.Close()
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st, err = NewSQLiteStore(db)
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewRunner(testCatalog(), client, st)
	if restarted.physicalOwner(e.RobotID) != "" {
		t.Fatal("restart resurrected owner")
	}
	gotAction, _ := st.GetActionByID(a.ID)
	if gotAction.Status != ActionStopped || gotAction.StopEvidence["confirmed"] != true || gotAction.Result["pre_stop_error"] == nil {
		t.Fatalf("stop not durable: %#v", gotAction)
	}
}

func TestOperatorStopCannotReleaseLiveActionOrAnotherOwner(t *testing.T) {
	st := NewMemorySkillExecutionStore()
	journal := NewMemoryActionJournal()
	runner := NewRunner(testCatalog(), &fakeAbilityClient{}, journal)
	runtime := NewSkillRuntime(nil, nil, runner, st, WorkerSupervisor{}, nil, nil, nil)
	_ = st.SaveSkillExecution(SkillExecution{ID: "old", RobotID: "r1", Status: SkillInterrupted})
	a := ActionExecution{ID: "a", SkillExecutionID: "old", RobotID: "r1", Physical: true, Status: ActionRunning}
	_ = journal.SaveAction(a)
	runner.activePhysical["r1"] = "a"
	confirmation := map[string]any{"confirmation_source": "operator", "safe": true, "hold_confirmed": true, "confirmed_by": "user", "confirmation_reason": "stopped"}
	if err := runtime.ConfirmOperatorStop("old", "r1", confirmation); err == nil {
		t.Fatal("active action released")
	}
	a.Status = ActionInterrupted
	_ = journal.SaveAction(a)
	runner.activePhysical["r1"] = "new-action"
	runtime.activeRobots["r1"] = "new-execution"
	if err := runtime.ConfirmOperatorStop("old", "r1", confirmation); err != nil {
		t.Fatal(err)
	}
	if runner.physicalOwner("r1") != "new-action" || runtime.activeRobots["r1"] != "new-execution" {
		t.Fatal("released another owner")
	}
}
