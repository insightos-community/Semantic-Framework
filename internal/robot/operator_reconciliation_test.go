package robot

import (
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

func TestReconcileDeliversOperatorStopAndPreservesItAfterLateEvents(t *testing.T) {
	st := openRobotTestStore(t)
	now := time.Now().UTC()
	pilot := store.RobotPilot{PilotInstanceID: "pilot", RobotID: "r1", Status: "online", Revision: 1, LastSeenAt: now}
	if err := st.SaveRobotPilot(pilot); err != nil {
		t.Fatal(err)
	}
	confirmation := map[string]any{"confirmation_source": "operator", "safe": true, "hold_confirmed": true, "confirmed_by": "user", "confirmation_reason": "stopped"}
	e := store.RobotExecution{ID: "old", ProjectID: "project", PilotInstanceID: pilot.PilotInstanceID, RobotID: pilot.RobotID, SkillName: "navigate", SkillVersion: "1", RequestKey: "old", Status: "interrupted", Result: confirmation, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := st.SaveRobotExecution(e); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendRobotExecutionEvent(store.RobotExecutionEvent{ExecutionID: e.ID, Sequence: 1, Type: "execution.terminal", Payload: map[string]any{"status": "interrupted"}, CreatedAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	s := NewService(st, nil)
	result, err := s.Reconcile(pilot.PilotInstanceID, []map[string]any{{"execution_id": e.ID, "status": "interrupted"}})
	if err != nil || len(result) != 1 || result[0].Status != "stopped" || !operatorStopConfirmed(result[0]) {
		t.Fatalf("confirmation not delivered: %#v %v", result, err)
	}
	if err := s.HandlePilotEvent(pilot.PilotInstanceID, "execution.terminal", 2, map[string]any{"execution_id": e.ID, "status": "interrupted", "error": map[string]any{"code": "late"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetRobotExecution(e.ID)
	if got.Status != "stopped" || !operatorStopConfirmed(got) {
		t.Fatalf("late interruption erased confirmation: %#v", got)
	}
	result, err = s.Reconcile(pilot.PilotInstanceID, nil)
	if err != nil || len(result) != 0 {
		t.Fatalf("unrequested terminal history sent: %#v %v", result, err)
	}
}
