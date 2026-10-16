package workflow

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

func TestRecoveryUsesActionDiagnosticsWhenSkillSDKHasOnlyCodeAndMessage(t *testing.T) {
	st, project, _, service, _, _ := newWorkflowFixture(t)
	execution := store.RobotExecution{ID: "rex-diagnostics", ProjectID: project.ID,
		Status: "failed", Error: map[string]any{
			"code": "navigation_cancelled", "message": "Native navigation was cancelled"}}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"action_id": "action-original", "status": "failed",
		"error": map[string]any{"code": "navigation_cancelled", "details": map[string]any{
			"cancel_origin": "manual_request", "native_status": 5, "distance_m": .007409}},
		"stop_evidence": map[string]any{"confirmed": true}}
	if err := st.AppendRobotExecutionEvent(store.RobotExecutionEvent{ExecutionID: execution.ID,
		Sequence: 1, Type: "action.terminal", Payload: payload, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	evidence, err := service.robotRecoveryActionEvidence(execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(evidence)
	for _, want := range []string{"action-original", "manual_request", "0.007409", `"confirmed":true`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("lost Action evidence %s: %s", want, raw)
		}
	}
}
