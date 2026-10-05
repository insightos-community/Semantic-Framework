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
	"strings"
	"sync"
	"testing"
	"time"
)

type realGateEvent struct {
	name   string
	fields map[string]any
}

type realGateEventSink struct {
	mu     sync.Mutex
	events []realGateEvent
}

func (s *realGateEventSink) Report(_ SkillExecution, event string, fields map[string]any) {
	s.mu.Lock()
	s.events = append(s.events, realGateEvent{name: event, fields: cloneMap(fields)})
	s.mu.Unlock()
}

func (*realGateEventSink) Log(SkillExecution, string, string, map[string]any) {}

// 该测试只在显式提供真实 AbilityFramework 地址时运行。它不会启动 Mock
// Gateway，而是让三个 Python Worker 经 Pilot 调用真实框架代理的七个 Ability。
func TestRealAbilityFrameworkRunsThreeRobotSkills(t *testing.T) {
	endpoint := os.Getenv("SEMANTIC_REAL_ABILITY_FRAMEWORK_ENDPOINT")
	skillsRoot := os.Getenv("SEMANTIC_ROBOT_SKILLS_DIR")
	profilePath := os.Getenv("SEMANTIC_ROBOT_CONFIG")
	if endpoint == "" || skillsRoot == "" || profilePath == "" {
		t.Skip("set SEMANTIC_REAL_ABILITY_FRAMEWORK_ENDPOINT, SEMANTIC_ROBOT_SKILLS_DIR and SEMANTIC_ROBOT_CONFIG")
	}

	deployment, err := LoadRobotDeployment(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	deployment.AbilityFramework.Endpoint = endpoint
	client := NewAbilityFrameworkClient(endpoint, nil)
	catalog := NewCatalog()
	discovery := NewAbilityFrameworkDiscovery(deployment, client, catalog, nil)
	if err := discovery.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snapshot := discovery.Snapshot(); snapshot.Status != "ready" || len(snapshot.Abilities) != 7 {
		t.Fatalf("seven real abilities are not ready: %#v", snapshot)
	}

	python := os.Getenv("SEMANTIC_ROBOT_SKILL_PYTHON")
	if python == "" {
		python = "python"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	events := &realGateEventSink{}
	skillCatalogRoot := os.Getenv("SEMANTIC_ROBOT_SKILL_CATALOG_DIR")
	if skillCatalogRoot == "" {
		skillCatalogRoot = filepath.Join(skillsRoot, "semantic_robot_skills", "skills")
	}
	result, err := RunDepalletizingAbilityStack(
		ctx,
		skillCatalogRoot,
		catalog,
		client,
		python,
		events,
		realMujocoDepalletizingStackConfig(deployment.Robot.ID),
	)
	if err != nil {
		events.mu.Lock()
		recent := append([]realGateEvent(nil), events.events...)
		events.mu.Unlock()
		if len(recent) > 8 {
			recent = recent[len(recent)-8:]
		}
		t.Fatalf("%v; recent events: %#v", err, recent)
	}
	for name, execution := range map[string]SkillExecution{
		"grasp": result.Grasp, "navigation": result.Navigation, "placement": result.Placement,
	} {
		if execution.Status != SkillCompleted {
			t.Fatalf("%s ended as %s: %#v", name, execution.Status, execution.Error)
		}
	}

	started := 0
	for _, event := range events.events {
		if event.name != "action.started" {
			continue
		}
		started++
		instanceID := stringValue(event.fields["ability_instance_id"])
		invocationID := stringValue(event.fields["invocation_id"])
		if instanceID == "" || invocationID == "" || strings.HasPrefix(instanceID, "fake-") {
			t.Fatalf("action lacks a real Ability invocation: %#v", event.fields)
		}
	}
	if started == 0 {
		t.Fatal("skill flow produced no action.started evidence")
	}
}

// TestRealAbilityFrameworkKeepsToteDuringShortCarry 专门隔离“已稳定持物后启动
// 底盘”这一物理边界。完整拆码垛失败时，直接重复整条流程很难区分是抓取预紧、
// 底盘加速度还是长距离转向造成脱钩；这里仍经过真实 Pilot Worker、Robot Skill、
// AbilityFramework、Ability 和 Robot SDK，只把导航目标缩短为无转向的 10cm。
// 测试不会伪造 holding_object，也不会降低 Skill 对双侧 stable_load 的要求。
func TestRealAbilityFrameworkKeepsToteDuringShortCarry(t *testing.T) {
	endpoint := os.Getenv("SEMANTIC_REAL_ABILITY_FRAMEWORK_ENDPOINT")
	skillsRoot := os.Getenv("SEMANTIC_ROBOT_SKILLS_DIR")
	profilePath := os.Getenv("SEMANTIC_ROBOT_CONFIG")
	if endpoint == "" || skillsRoot == "" || profilePath == "" {
		t.Skip("set SEMANTIC_REAL_ABILITY_FRAMEWORK_ENDPOINT, SEMANTIC_ROBOT_SKILLS_DIR and SEMANTIC_ROBOT_CONFIG")
	}

	deployment, err := LoadRobotDeployment(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	deployment.AbilityFramework.Endpoint = endpoint
	client := NewAbilityFrameworkClient(endpoint, nil)
	catalog := NewCatalog()
	discovery := NewAbilityFrameworkDiscovery(deployment, client, catalog, nil)
	if err := discovery.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	python := os.Getenv("SEMANTIC_ROBOT_SKILL_PYTHON")
	if python == "" {
		python = "python"
	}
	skillCatalogRoot := os.Getenv("SEMANTIC_ROBOT_SKILL_CATALOG_DIR")
	if skillCatalogRoot == "" {
		skillCatalogRoot = filepath.Join(skillsRoot, "semantic_robot_skills", "skills")
	}
	skills, err := ScanSkillCatalog(skillCatalogRoot)
	if err != nil {
		t.Fatal(err)
	}
	events := &realGateEventSink{}
	runtime := NewSkillRuntime(
		skills,
		catalog,
		NewRunner(catalog, client, NewMemoryActionJournal()),
		NewMemorySkillExecutionStore(),
		WorkerSupervisor{
			PythonExecutable: python,
			OnStderrLine: func(line string) {
				t.Logf("robot skill worker: %s", line)
			},
		},
		&MockAgentGateway{},
		nil,
		events,
	)
	config := realMujocoDepalletizingStackConfig(deployment.Robot.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if _, err := runMockSkill(ctx, runtime, SkillStartRequest{
		ProjectID: config.ProjectID, TaskID: config.TaskID, SubtaskID: "short-carry-approach",
		RobotID: config.RobotID, SkillName: "semantic-navigation", Version: config.SkillVersion,
		Input: cloneMap(config.ApproachInput),
	}); err != nil {
		t.Fatal(err)
	}
	_, err = runMockSkill(ctx, runtime, SkillStartRequest{
		ProjectID: config.ProjectID, TaskID: config.TaskID, SubtaskID: "short-carry-grasp",
		RobotID: config.RobotID, SkillName: "grasp-object", Version: config.SkillVersion,
		Input: cloneMap(config.GraspInput),
	})
	if err != nil {
		t.Fatal(err)
	}
	carryInput := cloneMap(config.NavigationInput)
	carryInput["target"] = map[string]any{
		"target_ref": "runtime-diagnostic://short-carry",
		"pose": map[string]any{
			"frame_id":         "world",
			"position_m":       []any{0.10, 0.50, 0.02},
			"orientation_xyzw": []any{0.0, 0.0, 0.7071067811865475, 0.7071067811865476},
		},
	}
	carryInput["maximum_speed_mps"] = 0.10
	carried, err := runMockSkill(ctx, runtime, SkillStartRequest{
		ProjectID: config.ProjectID, TaskID: config.TaskID, SubtaskID: "short-carry-navigation",
		RobotID: config.RobotID, SkillName: "semantic-navigation", Version: config.SkillVersion,
		Input: carryInput,
	})
	if err != nil {
		t.Fatal(err)
	}
	if carried.Status != SkillCompleted {
		t.Fatalf("short carry ended as %s: %#v", carried.Status, carried.Error)
	}
}

func realMujocoDepalletizingStackConfig(robotID string) DepalletizingSkillStackConfig {
	return DepalletizingSkillStackConfig{
		RobotID: robotID, SkillVersion: "0.4.0",
		ProjectID: "project-v050-mujoco", TaskID: "task-v050-mujoco-single-tote",
		ApproachInput: map[string]any{
			"target": map[string]any{
				"target_ref": "tote-large-smoke-source-work-pose",
				"pose": map[string]any{
					"frame_id":         "world",
					"position_m":       []any{0.0, 0.50, 0.02},
					"orientation_xyzw": []any{0.0, 0.0, 0.7071067811865475, 0.7071067811865476},
				},
			},
			"navigation_purpose":  "approach_grasp",
			"maximum_speed_mps":   0.08,
			"minimum_clearance_m": 0.05,
		},
		GraspInput: map[string]any{
			"target": map[string]any{
				"object_ref":    "tote-large-smoke",
				"extent_hint_m": []any{0.6, 0.4, 0.34},
				"category_hint": "tote",
			},
			"tool_refs":             []any{"component://tool/left", "component://tool/right"},
			"preferred_strategy":    "auto",
			"minimum_lift_height_m": 0.08,
		},
		NavigationInput: map[string]any{
			"target": map[string]any{
				"target_ref": "pallet-b-slot-r1-c1",
				"pose": map[string]any{
					"frame_id": "world",
					// 目标是一次到达既不侵入托盘占据区、又处于双臂局部放置工作空间的
					// 基座工位。这个数值只属于 smoke Gate 的已知几何；产品规划由 Robot
					// Agent 根据当前占据空间、工具工作距离和目标列实时推导，不能照抄。
					"position_m":       []any{1.19, 0.50, 0.02},
					"orientation_xyzw": []any{0.0, 0.0, 0.7071067811865475, 0.7071067811865476},
				},
			},
			"navigation_purpose":  "carry_to_place",
			"carried_object_ref":  "tote-large-smoke",
			"maximum_speed_mps":   0.05,
			"minimum_clearance_m": 0.05,
		},
		PlacementInput: map[string]any{
			"object_ref": "tote-large-smoke",
			"target": map[string]any{
				"target_ref":            "pallet-b-slot-r1-c1",
				"extent_hint_m":         []any{0.56, 0.36, 0.02},
				"category_hint":         "placement_column",
				"stability_duration_ms": 800,
			},
		},
	}
}
