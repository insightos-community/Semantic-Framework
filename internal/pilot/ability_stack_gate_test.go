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
	"fmt"
	"os"
	"path/filepath"
)

// DepalletizingSkillStackConfig 描述三个 Robot Skill 的业务输入。测试入口只负责
// 串联真实 Skill 结果，不在这里复制 Stage、Action 或 Ability 的执行逻辑。
// 0.3.0 起 Navigation 与 Place 会自行通过 RobotState读取实时持物事实。
// Gate只传业务对象引用，不能再通过复制上一项结果伪造跨 Skill 状态。
type DepalletizingSkillStackConfig struct {
	RobotID         string
	ProjectID       string
	TaskID          string
	SkillVersion    string
	PythonPaths     []string
	ApproachInput   map[string]any
	GraspInput      map[string]any
	NavigationInput map[string]any
	PlacementInput  map[string]any
}

// R1ProMockAbilityBindings 使用真实 Ability Task 名称和按逻辑接口划分的精确实例。
// 这里的实例 ID 与测试进程一致，生产部署由 Robot profile 填入实际实例 ID。
func R1ProMockAbilityBindings() []AbilityBinding {
	return []AbilityBinding{
		abilityBinding("perception.locate_object", "R1ProObjectPerception.V2", "LocateObject", "fake-r1-object-perception", false),
		abilityBinding("grasp.generate_candidates", "R1ProGraspPlanning.V2", "GenerateCandidates", "fake-r1-grasp-planning", false),
		abilityBinding("motion.move_end_effector", "R1ProManipulatorMotion.V2", "MoveEndEffector", "fake-r1-manipulator-motion", true),
		abilityBinding("motion.move_to_posture", "R1ProManipulatorMotion.V2", "MoveToPosture", "fake-r1-manipulator-motion", true),
		abilityBinding("perception.verify_pregrasp", "R1ProObjectPerception.V2", "VerifyPregrasp", "fake-r1-object-perception", false),
		abilityBinding("gripper.set_opening", "R1ProEndEffector.V2", "SetOpening", "fake-r1-end-effector", true),
		abilityBinding("gripper.close", "R1ProEndEffector.V2", "CloseUntilContact", "fake-r1-end-effector", true),
		abilityBinding("motion.lift_held_object", "R1ProManipulatorMotion.V2", "LiftHeldObject", "fake-r1-manipulator-motion", true),
		abilityBinding("perception.verify_grasp", "R1ProObjectPerception.V2", "VerifyGrasp", "fake-r1-object-perception", false),
		abilityBinding("navigation.plan_route", "R1ProNavigation.V2", "PlanRoute", "fake-r1-navigation", false),
		abilityBinding("navigation.follow_route", "R1ProNavigation.V2", "FollowRoute", "fake-r1-navigation", true),
		abilityBinding("navigation.verify_arrival", "R1ProNavigation.V2", "VerifyArrival", "fake-r1-navigation", false),
		abilityBinding("robot.get_held_object", "R1ProRobotState.V2", "GetHeldObjectState", "fake-r1-robot-state", false),
		abilityBinding("robot.get_state", "R1ProRobotState.V2", "GetRobotState", "fake-r1-robot-state", false),
		abilityBinding("perception.observe_placement_target", "R1ProObjectPerception.V2", "ObservePlacementTarget", "fake-r1-object-perception", false),
		abilityBinding("gripper.release", "R1ProEndEffector.V2", "Release", "fake-r1-end-effector", true),
		abilityBinding("perception.verify_placement", "R1ProObjectPerception.V2", "VerifyPlacement", "fake-r1-object-perception", false),
		abilityBinding("gripper.hold_object", "R1ProEndEffector.V2", "HoldObject", "fake-r1-end-effector", true),
	}
}

func abilityBinding(actionType, abilityName, taskName, instanceID string, physical bool) AbilityBinding {
	return AbilityBinding{
		Action:      ActionRef{Type: actionType, SchemaVersion: 2},
		AbilityName: abilityName,
		TaskName:    taskName,
		InstanceID:  instanceID,
		Physical:    physical,
	}
}

// RunDepalletizingAbilityStackDemo 让三个真实 Skill Worker 经过 Pilot，调用测试进程中的
// ability_py.task_manager、正式 Ability Service 和 Fake Robot SDK。
func RunDepalletizingAbilityStackDemo(ctx context.Context, skillCatalogRoot string, client *AbilityProcessClient) (MockDemoResult, error) {
	return RunDepalletizingAbilityStackDemoWithEvents(ctx, skillCatalogRoot, client, nil)
}

// RunDepalletizingAbilityStackDemoWithEvents 在完整 Mock 闭环中同步输出结构化过程证据。
func RunDepalletizingAbilityStackDemoWithEvents(ctx context.Context, skillCatalogRoot string, client *AbilityProcessClient, events RuntimeEventSink) (MockDemoResult, error) {
	const robotID = "r1pro-fake-001"
	bindings := R1ProMockAbilityBindings()
	profile := RobotProfile{RobotID: robotID, Bindings: make(map[string]AbilityBinding, len(bindings))}
	for _, item := range bindings {
		profile.Bindings[item.Action.Key()] = item
	}
	result, err := RunDepalletizingAbilityStack(
		ctx, skillCatalogRoot, NewCatalog(profile), client, "python", events,
		func() DepalletizingSkillStackConfig {
			config := defaultFakeDepalletizingStackConfig(robotID)
			config.PythonPaths = []string{filepath.Dir(filepath.Dir(skillCatalogRoot))}
			return config
		}(),
	)
	if err != nil {
		return MockDemoResult{}, err
	}
	counts, err := client.StartCounts(ctx)
	if err != nil {
		return MockDemoResult{}, err
	}
	result.TaskStarts = counts
	return result, nil
}

// RunDepalletizingAbilityStack 是真实 AbilityFramework Gate 与 Mock Gate 共用的
// Skill 流程入口。调用方负责提供已经按 Robot ID 精确绑定的 Ability 目录；这里
// 只运行 Worker 和三个 Skill，不为测试伪造任何 Ability 结果。
func RunDepalletizingAbilityStack(ctx context.Context, skillCatalogRoot string, catalog *Catalog, client AbilityClient, python string, events RuntimeEventSink, config DepalletizingSkillStackConfig) (MockDemoResult, error) {
	if config.RobotID == "" || config.SkillVersion == "" {
		return MockDemoResult{}, fmt.Errorf("robot ID and skill version are required")
	}
	skills, err := ScanSkillCatalog(skillCatalogRoot)
	if err != nil {
		return MockDemoResult{}, err
	}
	if python == "" {
		python = "python"
	}
	projectID := config.ProjectID
	if projectID == "" {
		projectID = "project-depalletizing"
	}
	taskID := config.TaskID
	if taskID == "" {
		taskID = "task-depalletizing"
	}
	runtime := NewSkillRuntime(
		skills,
		catalog,
		NewRunner(catalog, client, NewMemoryActionJournal()),
		NewMemorySkillExecutionStore(),
		WorkerSupervisor{
			PythonExecutable: python,
			PythonPaths:      append([]string(nil), config.PythonPaths...),
			OnStderrLine: func(line string) {
				fmt.Fprintf(os.Stderr, "skill worker: %s\n", line)
			},
		},
		&MockAgentGateway{},
		nil,
		events,
	)
	var approach *SkillExecution
	if len(config.ApproachInput) != 0 {
		execution, approachErr := runMockSkill(ctx, runtime, SkillStartRequest{
			ProjectID: projectID, TaskID: taskID, SubtaskID: "approach-source",
			RobotID: config.RobotID, SkillName: "semantic-navigation", Version: config.SkillVersion,
			Input: cloneMap(config.ApproachInput),
		})
		if approachErr != nil {
			return MockDemoResult{}, approachErr
		}
		approach = &execution
	}
	grasp, err := runMockSkill(ctx, runtime, SkillStartRequest{
		ProjectID: projectID, TaskID: taskID, SubtaskID: "grasp",
		RobotID: config.RobotID, SkillName: "grasp-object", Version: config.SkillVersion,
		Input: cloneMap(config.GraspInput),
	})
	if err != nil {
		return MockDemoResult{}, err
	}
	navigation, err := runMockSkill(ctx, runtime, SkillStartRequest{
		ProjectID: projectID, TaskID: taskID, SubtaskID: "navigate",
		RobotID: config.RobotID, SkillName: "semantic-navigation", Version: config.SkillVersion,
		Input: cloneMap(config.NavigationInput),
	})
	if err != nil {
		return MockDemoResult{}, err
	}
	placement, err := runMockSkill(ctx, runtime, SkillStartRequest{
		ProjectID: projectID, TaskID: taskID, SubtaskID: "place",
		RobotID: config.RobotID, SkillName: "place-object", Version: config.SkillVersion,
		Input: cloneMap(config.PlacementInput),
	})
	if err != nil {
		return MockDemoResult{}, err
	}
	return MockDemoResult{
		RobotID: config.RobotID, Approach: approach, Grasp: grasp, Navigation: navigation, Placement: placement,
	}, nil
}

func defaultFakeDepalletizingStackConfig(robotID string) DepalletizingSkillStackConfig {
	// Fake 抓取后的携物中心位于底盘前方约 0.55m；这里直接规划到目标列
	// 的可操作基座位姿，不再保留旧的“托盘前 + 槽位前”两段式导航。
	pose := MockPose(0.25, 0, 0, "slot-cell-07-work-pose")
	pose["frame_id"] = "world"
	return DepalletizingSkillStackConfig{
		RobotID: robotID, SkillVersion: "0.3.0",
		GraspInput: map[string]any{
			"target": map[string]any{
				"object_ref":    "object://pallet-a/box-17",
				"extent_hint_m": []any{0.6, 0.4, 0.34},
				"category_hint": "tote",
			},
			"tool_refs":          []any{"component://tool/left", "component://tool/right"},
			"preferred_strategy": "direct_bilateral",
		},
		NavigationInput: map[string]any{
			"target": map[string]any{
				"target_ref": "slot://pallet-b/cell-07",
				"pose":       pose,
			},
			"navigation_purpose": "carry_to_place",
			"carried_object_ref": "object://pallet-a/box-17",
		},
		PlacementInput: map[string]any{
			"object_ref": "object://pallet-a/box-17",
			"target": map[string]any{
				"target_ref":            "slot://pallet-b/cell-07",
				"stability_duration_ms": 800,
			},
		},
	}
}
