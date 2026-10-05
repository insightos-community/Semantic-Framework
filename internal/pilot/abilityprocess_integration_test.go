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
	"time"
)

func TestPilotRunsRealAbilityFrameworkAndSDKDepalletizingFlow(t *testing.T) {
	skillsRoot := os.Getenv("SEMANTIC_ROBOT_SKILLS_DIR")
	abilityRoot := os.Getenv("SEMANTIC_ABILITY_DIR")
	sdkRoot := os.Getenv("R1PRO_SDK_DIR")
	if skillsRoot == "" || abilityRoot == "" || sdkRoot == "" {
		t.Skip("set SEMANTIC_ROBOT_SKILLS_DIR, SEMANTIC_ABILITY_DIR and R1PRO_SDK_DIR")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := StartAbilityMockProcess(ctx, AbilityMockProcessConfig{
		PythonExecutable: abilityTestPython(),
		AbilityRoot:      abilityRoot,
		RobotSDKRoot:     sdkRoot,
		RobotProfilePath: filepath.Join(abilityRoot, "configs", "robot-deployment.fake.yaml"),
		ModelProfilePath: filepath.Join(abilityRoot, "configs", "models.example.json"),
		ExecutionStore:   t.TempDir(),
		OnStderrLine: func(line string) {
			t.Logf("ability process: %s", line)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		if err := client.Close(closeCtx); err != nil {
			t.Logf("close ability process: %v", err)
		}
	}()

	events := &realGateEventSink{}
	result, err := RunDepalletizingAbilityStackDemoWithEvents(
		ctx,
		filepath.Join(skillsRoot, "semantic_robot_skills", "skills"),
		client,
		events,
	)
	if err != nil {
		for _, item := range events.events {
			t.Logf("runtime event %s: %#v", item.name, item.fields)
		}
		t.Fatal(err)
	}
	for name, execution := range map[string]SkillExecution{
		"grasp": result.Grasp, "navigation": result.Navigation, "placement": result.Placement,
	} {
		if execution.Status != SkillCompleted {
			t.Fatalf("%s ended as %s: %#v", name, execution.Status, execution.Error)
		}
	}
	for task, expected := range map[string]int{
		"PlanRoute":              2,
		"FollowRoute":            2,
		"ObservePlacementTarget": 2,
		"GetHeldObjectState":     3,
		"Release":                2,
		"VerifyPlacement":        1,
		"MoveToPosture":          1,
	} {
		if actual := result.TaskStarts[task]; actual != expected {
			t.Fatalf("%s starts=%d, want %d; all=%#v", task, actual, expected, result.TaskStarts)
		}
	}
	if stable, _ := result.Placement.Result["stable"].(bool); !stable {
		t.Fatalf("placement lacks independent stability evidence: %#v", result.Placement.Result)
	}
}

func TestAbilityProcessRejectsWrongInstance(t *testing.T) {
	abilityRoot := os.Getenv("SEMANTIC_ABILITY_DIR")
	sdkRoot := os.Getenv("R1PRO_SDK_DIR")
	if abilityRoot == "" || sdkRoot == "" {
		t.Skip("set SEMANTIC_ABILITY_DIR and R1PRO_SDK_DIR")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := StartAbilityMockProcess(ctx, AbilityMockProcessConfig{
		PythonExecutable: abilityTestPython(),
		AbilityRoot:      abilityRoot,
		RobotSDKRoot:     sdkRoot,
		RobotProfilePath: filepath.Join(abilityRoot, "configs", "robot-deployment.fake.yaml"),
		ModelProfilePath: filepath.Join(abilityRoot, "configs", "models.example.json"),
		ExecutionStore:   t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if killErr := client.Kill(); killErr != nil {
			t.Errorf("停止测试 Ability 进程: %v", killErr)
		}
	}()

	_, err = client.StartTask(ctx, "fake-r1-object-perception", "PlanRoute", map[string]any{
		"invocation_id": "wrong-binding",
	})
	if err == nil {
		t.Fatal("wrong Ability instance was accepted")
	}
}

func abilityTestPython() string {
	if value := os.Getenv("SEMANTIC_ABILITY_PYTHON"); value != "" {
		return value
	}
	return "python"
}
