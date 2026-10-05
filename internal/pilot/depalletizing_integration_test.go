package pilot

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPilotCompletesThreeSkillDepalletizingFlowWithLocalRecovery(t *testing.T) {
	repository := os.Getenv("SEMANTIC_ROBOT_SKILLS_DIR")
	if repository == "" {
		t.Skip("set SEMANTIC_ROBOT_SKILLS_DIR")
	}
	ctx, cancel := DefaultMockDemoContext(context.Background())
	defer cancel()
	result, err := RunDepalletizingMockDemo(
		ctx,
		filepath.Join(repository, "semantic_robot_skills", "skills"),
	)
	if err != nil {
		t.Fatal(err)
	}
	held, _ := result.Grasp.Result["held_object"].(map[string]any)
	if held["grasp_candidate_id"] != "candidate-b" {
		t.Fatalf("first failed candidate was not replaced: %#v", held)
	}
	if result.TaskStarts["navigation.plan_route"] != 2 {
		t.Fatalf("route was not replanned: %#v", result.TaskStarts)
	}
	if result.TaskStarts["perception.observe_placement_target"] != 2 {
		t.Fatalf("occupied slot was not refreshed: %#v", result.TaskStarts)
	}
	if stable, _ := result.Placement.Result["stable"].(bool); !stable {
		t.Fatalf("placement not stable: %#v", result.Placement.Result)
	}
}
