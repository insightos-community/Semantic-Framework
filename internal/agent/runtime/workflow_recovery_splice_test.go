package runtime

import (
	"encoding/json"
	"insightos.cn/semantic-framework/internal/store"
	"testing"
)

func TestPlanValidatesPublishedPublicTasksIncludingCombo(t *testing.T) {
	catalog := robotTaskPlanningView{Skills: []robotTaskSkillView{{Name: "target-search", Version: "9", InputTasks: []string{"sweep", "plan_next"}}}}
	for _, task := range []string{"sweep", "search"} {
		plan := map[string]any{"summary": "search", "subtasks": []any{map[string]any{"id": "sweep", "kind": "robot_skill", "goal": "observe", "spec": map[string]any{"skill_name": "target-search", "skill_version": "9", "intent": map[string]any{"task": task}}}}}
		raw, _ := json.Marshal(plan)
		_, err := decodeAndValidateTaskPlanResult(string(raw), store.Task{RequiredRole: "robot"}, catalog)
		if (err == nil) != (task == "sweep") {
			t.Fatalf("task=%s error=%v", task, err)
		}
	}
}

func TestRecoveryRetainsOnlyUniqueUnstartedSteps(t *testing.T) {
	pending := []store.SubTask{{ID: "a", Status: store.TaskStatusPending}, {ID: "b", Status: store.TaskStatusPending, ExecutionRef: "old"}, {ID: "c", Status: store.TaskStatusCompleted}}
	if err := validateRetainedPending([]string{"a"}, pending); err != nil {
		t.Fatal(err)
	}
	for _, ids := range [][]string{{"a", "a"}, {"b"}, {"c"}, {"foreign"}} {
		if err := validateRetainedPending(ids, pending); err == nil {
			t.Fatalf("accepted %v", ids)
		}
	}
	catalog := robotTaskPlanningView{Skills: []robotTaskSkillView{{Name: "skill", Version: "1"}}}
	text := `{"decision":"revise_pending","summary":"repair one step","replacements":[{"id":"repair","kind":"robot_skill","goal":"repair","spec":{"skill_name":"skill","skill_version":"1","intent":{"task":"state"}}}],"retain_pending_ids":["a"]}`
	decision, err := decodeTaskRecoveryDecision(text, store.Task{RequiredRole: "robot"}, catalog)
	if err != nil || len(decision.RetainPendingIDs) != 1 {
		t.Fatalf("%+v %v", decision, err)
	}
	if _, err := decodeTaskRecoveryDecision(`{"decision":"fail_task","summary":"stop","retain_pending_ids":["a"]}`, store.Task{}, catalog); err == nil {
		t.Fatal("fail_task accepted retained steps")
	}
}
