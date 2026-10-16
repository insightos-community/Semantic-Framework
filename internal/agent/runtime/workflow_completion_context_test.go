package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	"insightos.cn/semantic-framework/internal/store"
)

func TestCompletedPlanContextNeverClaimsTheMotionGoalWasReached(t *testing.T) {
	value := completedSubTaskContext(store.SubTask{
		ID: "restore-arms", Kind: "robot_skill", Status: "completed",
		Goal: "规划并执行收臂到运输位", ExecutionRef: "rex-planned",
		Spec:   json.RawMessage(`{"skill_name":"r1pro-manipulation","skill_version":"test"}`),
		Result: json.RawMessage(`{"result":{"task":"plan","completion":"planned","summary":"规划完成；尚未执行运动","next_step":"执行 packet/digest","payload":{"packet":{"private_large_route":true}}}}`),
	})
	if value["completion"] != "planned" || value["result_summary"] != "规划完成；尚未执行运动" || value["skill_task"] != "plan" {
		t.Fatalf("planning evidence lost: %+v", value)
	}
	wire, _ := json.Marshal(value)
	for _, text := range []string{"运输位已完成", "private_large_route"} {
		if strings.Contains(string(wire), text) {
			t.Fatalf("invented effect or copied payload: %s", wire)
		}
	}
	if value["result_lookup"] == nil {
		t.Fatal("original result no longer retrievable")
	}
}

func TestLegacyCompletedContextDoesNotInventPhysicalEvidence(t *testing.T) {
	value := completedSubTaskContext(store.SubTask{Goal: "抓取可乐", Kind: "robot_skill", Status: "completed", Result: json.RawMessage(`{}`)})
	if _, exists := value["completion"]; exists {
		t.Fatalf("invented completion: %+v", value)
	}
	if strings.Contains(value["result_summary"].(string), "抓取可乐已完成") {
		t.Fatal("goal reported as evidence")
	}
}
