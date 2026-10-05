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
