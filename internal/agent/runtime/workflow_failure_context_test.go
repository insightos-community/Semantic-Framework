package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	"insightos.cn/semantic-framework/internal/store"
)

func TestFailureContextRetainsInputSourcesWithoutOldMotionPackets(t *testing.T) {
	fx := newTestFixture(t)
	svc := NewService(Deps{Store: fx.st})
	execution := store.RobotExecution{ID: "failed-nav", TaskID: "current-task", Status: "failed",
		SkillName: "navigation", SkillVersion: "1.0.0", Input: map[string]any{"packet": "OLD_MOTION_PACKET"},
		Error:         map[string]any{"code": "planning_failed", "message": "goal too near", "details": map[string]any{"packet": "OLD_DETAIL_PACKET"}},
		InputBindings: []store.RobotInputBinding{{InputPath: "/goal", ExecutionID: "original-target-plan", ResultPath: "/payload/goal"}},
	}
	if err := fx.st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	items := []store.SubTask{
		{ID: "failed-step", TaskID: "current-task", Status: "failed", Kind: "robot_skill", ExecutionRef: execution.ID},
		{ID: "completed-step", TaskID: "current-task", Status: "completed", Kind: "robot_skill", ExecutionRef: execution.ID},
		{ID: "other-task-step", TaskID: "other-task", Status: "failed", Kind: "robot_skill", ExecutionRef: execution.ID},
	}
	contexts := svc.failedSubTaskContexts(items, "current-task")
	if len(contexts) != 1 {
		t.Fatalf("wrong failure scope: %+v", contexts)
	}
	raw, err := json.Marshal(contexts)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, marker := range []string{"failed-nav", "original-target-plan", "/payload/goal", "goal too near"} {
		if !strings.Contains(text, marker) {
			t.Fatalf("lost failure source %q: %s", marker, text)
		}
	}
	if strings.Contains(text, "OLD_MOTION_PACKET") || strings.Contains(text, "OLD_DETAIL_PACKET") {
		t.Fatal("failure context copied obsolete execution data")
	}
}
