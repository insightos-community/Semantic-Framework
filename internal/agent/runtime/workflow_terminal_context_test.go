package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"insightos.cn/semantic-framework/internal/store"
)

func TestTerminalAcceptanceNeverLeaksBetweenWorkflows(t *testing.T) {
	answers := []store.Interaction{
		{ID: "old", WorkflowID: "other", Status: "answered", Reply: "可以放宽"},
		{ID: "current", WorkflowID: "this", Status: "answered", Reply: "本轮接受当前残差"},
		{ID: "pending", WorkflowID: "this", Status: "pending", Reply: ""},
	}
	result := terminalInteractionContext("this", answers)
	if len(result) != 1 || result[0]["interaction_id"] != "current" {
		t.Fatal(result)
	}
}

// Optional private corpus replay; no robot or model calls. The source archive
// is kept outside Git, while this test also runs on synthetic data in CI.
func TestRecordedWorkflowTerminalProjection(t *testing.T) {
	path := os.Getenv("R1PRO_REVIEW_WORKFLOW_VIEW")
	if path == "" {
		t.Skip("private workflow export not supplied")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var view store.WorkflowView
	if err = json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	compact, err := json.Marshal(workflowTerminalContext(view))
	if err != nil {
		t.Fatal(err)
	}
	if len(view.SubTasks) != 83 || view.Workflow.Status != "completed" {
		t.Fatal("wrong replay corpus")
	}
	if len(compact) >= len(raw)/5 {
		t.Fatalf("projection still too large: %d -> %d", len(raw), len(compact))
	}
	t.Logf("Recorded workflow: %d -> %d JSON bytes, %d steps retained", len(raw), len(compact), len(view.SubTasks))
}

func TestTerminalContextKeeps83OutcomesWithoutTrajectoryExpansion(t *testing.T) {
	view := store.WorkflowView{Workflow: store.Workflow{ID: "wf-completed", Goal: "取放后返程", Status: "completed"},
		Tasks: []store.Task{{ID: "task", Status: "completed", ResultSummary: "用户本轮接受 7.409 mm / 1.803°；已停稳空夹"}}}
	for i := 0; i < 83; i++ {
		status := "completed"
		if i < 14 {
			status = "failed"
		}
		result, _ := json.Marshal(map[string]any{"result": map[string]any{
			"task": "plan", "completion": "planned", "summary": "只有计划，没有运动",
			"payload": map[string]any{"packet": strings.Repeat("DO_NOT_INLINE_TRAJECTORY", 100000)}}})
		view.SubTasks = append(view.SubTasks, store.SubTask{ID: fmt.Sprint("st-", i), TaskID: "task", Kind: "robot_skill",
			Status: status, ExecutionRef: fmt.Sprint("rex-", i), Result: result})
	}
	context := workflowTerminalContext(view)
	raw, err := json.Marshal(context)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 100000 || strings.Contains(string(raw), "DO_NOT_INLINE_TRAJECTORY") {
		t.Fatalf("terminal prompt grew with geometry: %d bytes", len(raw))
	}
	counts := context["subtask_counts"].(map[string]int)
	if counts["failed"] != 14 || counts["completed"] != 69 {
		t.Fatal(counts)
	}
	for _, want := range []string{"rex-82", "planned", "7.409 mm / 1.803°", "只有计划，没有运动"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing outcome %s", want)
		}
	}
}

func TestFailureProjectionRetainsRecoveryDiagnosisWithoutPacket(t *testing.T) {
	execution := store.RobotExecution{ID: "rex-original", Error: map[string]any{
		"code": "navigation_failed", "details": map[string]any{
			"native_detail": "goal pose too near", "manual_cancel": false,
			"distance_m": .007409, "yaw_error_rad": .031469,
			"readiness": map[string]any{"confirmed": false, "pose_fresh": true},
			"packet":    "NEVER_REPLAY_THIS"}}}
	value := failedSubTaskContext(store.SubTask{}, execution)
	raw, _ := json.Marshal(value)
	for _, want := range []string{"goal pose too near", `"manual_cancel":false`, `"confirmed":false`, `"pose_fresh":true`, "0.007409"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("lost diagnosis %s: %s", want, raw)
		}
	}
	if strings.Contains(string(raw), "NEVER_REPLAY_THIS") {
		t.Fatal("copied packet")
	}
}
