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
	"path/filepath"
	"sync"
	"time"
)

// DepalletizingMockInvocation 是 Fake AbilityFramework 保存的确定性调用记录。
type DepalletizingMockInvocation struct {
	Task    string
	Input   map[string]any
	Ordinal int
}

// DepalletizingMockAbilityClient 模拟 AbilityFramework 和设备结果，但仍经过 Pilot 的
// 精确实例路由、Action Journal、反馈游标、停止和 Worker 进程边界。
type DepalletizingMockAbilityClient struct {
	mu     sync.Mutex
	items  map[string]DepalletizingMockInvocation
	counts map[string]int
}

func NewDepalletizingMockAbilityClient() *DepalletizingMockAbilityClient {
	return &DepalletizingMockAbilityClient{
		items:  make(map[string]DepalletizingMockInvocation),
		counts: make(map[string]int),
	}
}

func (f *DepalletizingMockAbilityClient) StartTask(_ context.Context, _ string, task string, input map[string]any) (AbilityTask, error) {
	invocationID := stringValue(input["invocation_id"])
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[task]++
	f.items[invocationID] = DepalletizingMockInvocation{
		Task: task, Input: cloneMap(input), Ordinal: f.counts[task],
	}
	return AbilityTask{TaskID: "task-" + invocationID}, nil
}

func (f *DepalletizingMockAbilityClient) GetExecution(_ context.Context, _ string, invocationID string, _ int64) (AbilityExecution, error) {
	f.mu.Lock()
	invocation, ok := f.items[invocationID]
	f.mu.Unlock()
	if !ok {
		return AbilityExecution{}, fmt.Errorf("mock invocation %s not found", invocationID)
	}
	return f.result(invocation)
}

func (f *DepalletizingMockAbilityClient) StopExecution(_ context.Context, _ string, _ string, _ string) (AbilityExecution, error) {
	return AbilityExecution{
		Status: "stopped",
		Result: map[string]any{
			"safe": true, "physical_state": "held", "evidence_refs": []string{"evidence://mock/stop"},
		},
	}, nil
}

func (f *DepalletizingMockAbilityClient) StartCounts() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneStringIntMap(f.counts)
}

func MockPose(x, y, z float64, revision string) map[string]any {
	return map[string]any{
		"frame_id": "map", "position_m": []float64{x, y, z},
		"orientation_xyzw": []float64{0, 0, 0, 1}, "revision": revision,
	}
}

func mockObservation(kind, subject, revision string, value map[string]any) map[string]any {
	source := "fake-ability://r1"
	if kind == "placement.object_stability" {
		source = "monitor://independent-placement-verifier"
	}
	return map[string]any{
		"id": "obs-" + kind + "-" + revision, "kind": kind, "source": source,
		"subject_ref": subject, "revision": revision, "frame_id": "map", "confidence": .98,
		"value": value, "evidence_refs": []string{"artifact://" + kind + "/" + revision},
	}
}

func mockFeedback(sequence int64, message string, observations ...map[string]any) AbilityFeedback {
	return AbilityFeedback{
		Sequence: sequence, Status: "running", Phase: "running", Progress: .5,
		Message: message, Severity: "info", Observations: observations,
		EvidenceRefs: []string{"artifact://feedback"},
	}
}

func (f *DepalletizingMockAbilityClient) result(item DepalletizingMockInvocation) (AbilityExecution, error) {
	const objectRef = "object://pallet-a/box-17"
	succeeded := func(output map[string]any, observations ...map[string]any) AbilityExecution {
		return AbilityExecution{Status: "succeeded", Result: output, Observations: observations}
	}
	switch item.Task {
	case "perception.locate_object":
		return succeeded(nil, mockObservation("target_pose", objectRef, "scene-grasp-7", map[string]any{
			"object_ref": objectRef, "pose": MockPose(.82, .27, .12, "scene-grasp-7"), "identity_confidence": .98,
		})), nil
	case "grasp.generate_candidates":
		candidate := func(id, strategy string, x, score float64) map[string]any {
			return map[string]any{
				"candidate_id": id, "strategy": strategy,
				"approach_pose": MockPose(x, .27, .24, "scene-grasp-7"),
				"grasp_pose":    MockPose(x, .27, .12, "scene-grasp-7"),
				"score":         score, "required_gripper_width_m": .09,
				"evidence_refs": []string{"artifact://candidate/" + id},
			}
		}
		value := map[string]any{
			"target_revision": "scene-grasp-7",
			"candidates": []any{
				candidate("candidate-a", "top", .82, .94),
				candidate("candidate-b", "side", .91, .85),
			},
		}
		return succeeded(nil, mockObservation("grasp_candidates", objectRef, "scene-grasp-7", value)), nil
	case "motion.move_end_effector":
		if stringValue(item.Input["candidate_id"]) == "candidate-a" {
			return AbilityExecution{Status: "failed", Error: map[string]any{
				"code": "CANDIDATE_UNREACHABLE", "message": "first candidate blocked",
			}}, nil
		}
		return succeeded(nil), nil
	case "perception.verify_pregrasp":
		candidate := stringValue(item.Input["candidate_id"])
		return succeeded(nil, mockObservation("pregrasp_state", objectRef, "scene-grasp-7", map[string]any{
			"candidate_id": candidate, "target_revision": "scene-grasp-7", "reached": true, "position_error_m": .005,
		})), nil
	case "gripper.close":
		candidate := stringValue(item.Input["candidate_id"])
		contact := mockObservation("grasp_contact", objectRef, "robot-grasp-20", map[string]any{
			"candidate_id": candidate, "contact_detected": true, "object_attached": true,
			"grip_force_n": 18.0, "slip_detected": false,
		})
		return AbilityExecution{Status: "succeeded", Feedback: []AbilityFeedback{mockFeedback(1, "形成稳定接触", contact)}, Observations: []map[string]any{contact}}, nil
	case "motion.lift_held_object":
		candidate := stringValue(item.Input["candidate_id"])
		lift := mockObservation("lift_progress", objectRef, "robot-lift-21", map[string]any{
			"candidate_id": candidate, "lift_height_m": .1, "object_follows_gripper": true, "slip_detected": false,
		})
		return AbilityExecution{Status: "succeeded", Feedback: []AbilityFeedback{mockFeedback(1, "稳定抬升", lift)}}, nil
	case "perception.verify_grasp":
		candidate := stringValue(item.Input["candidate_id"])
		value := map[string]any{
			"candidate_id": candidate, "held": true, "object_follows_gripper": true,
			"lift_height_m": .1, "stable_duration_ms": 700, "confidence": .96,
			"object_pose":   MockPose(.91, .27, .22, "scene-grasp-8"),
			"object_size_m": []float64{.8, .4, .3}, "estimated_mass_kg": 5.2,
			"robot_state_revision": "robot-state-grasped-21",
		}
		return succeeded(nil, mockObservation("grasp_verification", objectRef, "scene-grasp-8", value)), nil
	case "navigation.plan_route":
		return succeeded(map[string]any{"route_ref": fmt.Sprintf("route://carry/%d", item.Ordinal)}), nil
	case "navigation.follow_route":
		if _, stopping := item.Input["reason"]; stopping {
			return succeeded(map[string]any{"safe": true, "physical_state": "base_held"}), nil
		}
		if item.Ordinal == 1 {
			blocked := mockObservation("route_blocked", "semantic://pallet-b/preplace", "route-block-1", map[string]any{"blocked": true})
			return AbilityExecution{Status: "running", Feedback: []AbilityFeedback{mockFeedback(1, "路线阻塞", blocked)}}, nil
		}
		return succeeded(map[string]any{"final_pose_ref": "pose://robot/pallet-b", "distance_to_target_m": .16}), nil
	case "navigation.verify_arrival":
		held, _ := item.Input["carrying_object"].(map[string]any)
		held = cloneMap(held)
		held["robot_state_revision"] = "robot-state-arrived-37"
		held["object_pose"] = MockPose(2.2, 1, .85, "scene-navigation-31")
		return succeeded(map[string]any{
			"verdict": "achieved", "final_pose_ref": "pose://robot/pallet-b",
			"distance_to_target_m": .16, "carrying_object": held,
		}), nil
	case "robot.get_held_object":
		held := map[string]any{
			"object_ref": objectRef, "robot_ref": "robot://r1pro/fake-1",
			"gripper_ref":   "component://gripper/main",
			"grasp_pose":    MockPose(.91, .27, .12, "scene-grasp-7"),
			"object_pose":   MockPose(2.2, 1, .85, "scene-navigation-31"),
			"object_size_m": []float64{.8, .4, .3}, "grasp_candidate_id": "candidate-b",
			"grasp_confidence": .96, "robot_state_revision": "robot-state-arrived-37",
			"evidence_refs": []string{"artifact://held"},
		}
		return succeeded(nil, mockObservation("manipulation.held_object", objectRef, "robot-state-arrived-37", map[string]any{
			"state": held, "held": true, "stable": true,
		})), nil
	case "perception.observe_placement_target":
		free := item.Ordinal > 1
		revision := fmt.Sprintf("scene-place-%d", 41+item.Ordinal)
		value := map[string]any{
			"schema_version": 1, "target_ref": "slot://pallet-b/cell-07",
			"placement_pose": MockPose(2.4, 1.1, .72, revision), "approach_vector": []float64{0, 0, 1},
			"free": free, "reachable": true, "support_surface_ref": "surface://pallet-b",
			"revision": revision, "confidence": .98,
			"evidence_refs": []string{"artifact://slot/" + revision},
		}
		return succeeded(nil, mockObservation("placement.target_slot", "slot://pallet-b/cell-07", revision, value)), nil
	case "gripper.release":
		released := mockObservation("manipulation.object_released", objectRef, "robot-released-38", map[string]any{
			"released": true, "gripper_empty": true,
		})
		return AbilityExecution{Status: "succeeded", Feedback: []AbilityFeedback{mockFeedback(1, "物体已释放", released)}, Observations: []map[string]any{released}}, nil
	case "perception.verify_placement":
		state := map[string]any{
			"object_ref": objectRef, "target_ref": "slot://pallet-b/cell-07",
			"final_pose":          MockPose(2.4, 1.1, .72, "scene-place-43"),
			"support_surface_ref": "surface://pallet-b", "position_error_m": .01,
			"orientation_error_rad": .04, "stable": true, "gripper_empty": true,
			"verification_source":  "monitor://independent-placement-verifier",
			"observed_duration_ms": 800, "scene_revision": "scene-place-43",
			"evidence_refs": []string{"artifact://place/stability"},
		}
		value := map[string]any{
			"state": state, "within_target": true, "gripper_empty": true, "independent_verification": true,
		}
		return succeeded(nil, mockObservation("placement.object_stability", objectRef, "scene-place-43", value)), nil
	case "gripper.hold_object":
		return succeeded(map[string]any{"safe": true, "physical_state": "holding_object"}), nil
	default:
		return AbilityExecution{}, fmt.Errorf("no fake result for %s", item.Task)
	}
}

func DepalletizingMockBindings() []AbilityBinding {
	physical := map[string]bool{
		"navigation.follow_route": true, "motion.move_end_effector": true,
		"gripper.close": true, "motion.lift_held_object": true,
		"gripper.release": true, "gripper.hold_object": true,
	}
	names := []string{
		"perception.locate_object", "grasp.generate_candidates", "motion.move_end_effector",
		"perception.verify_pregrasp", "gripper.close", "motion.lift_held_object",
		"perception.verify_grasp", "navigation.plan_route", "navigation.follow_route",
		"navigation.verify_arrival", "robot.get_held_object", "perception.observe_placement_target",
		"gripper.release", "perception.verify_placement", "gripper.hold_object",
	}
	result := make([]AbilityBinding, 0, len(names))
	for _, name := range names {
		result = append(result, AbilityBinding{
			Action: ActionRef{Type: name, SchemaVersion: 1}, AbilityName: "FakeR1Pro.V1",
			TaskName: name, InstanceID: "fake-r1-" + name, Physical: physical[name],
		})
	}
	return result
}

// MockDemoResult 是一条命令验收时输出的最小可追踪证据。
type MockDemoResult struct {
	RobotID    string          `json:"robot_id"`
	Approach   *SkillExecution `json:"approach,omitempty"`
	Grasp      SkillExecution  `json:"grasp"`
	Navigation SkillExecution  `json:"navigation"`
	Placement  SkillExecution  `json:"placement"`
	TaskStarts map[string]int  `json:"ability_task_starts"`
}

// RunDepalletizingMockDemo 启动三个真实 Python Worker，并执行带局部恢复的完整 Mock 流程。
func RunDepalletizingMockDemo(ctx context.Context, skillCatalogRoot string) (MockDemoResult, error) {
	skills, err := ScanSkillCatalog(skillCatalogRoot)
	if err != nil {
		return MockDemoResult{}, err
	}
	const robotID = "robot://r1pro/fake-1"
	bindings := DepalletizingMockBindings()
	profile := RobotProfile{RobotID: robotID, Bindings: make(map[string]AbilityBinding, len(bindings))}
	for _, item := range bindings {
		profile.Bindings[item.Action.Key()] = item
	}
	catalog := NewCatalog(profile)
	client := NewDepalletizingMockAbilityClient()
	runtime := NewSkillRuntime(
		skills,
		catalog,
		NewRunner(catalog, client, NewMemoryActionJournal()),
		NewMemorySkillExecutionStore(),
		WorkerSupervisor{
			PythonExecutable: "python",
			PythonPaths:      []string{filepath.Dir(filepath.Dir(skillCatalogRoot))},
		},
		&MockAgentGateway{},
		nil,
		nil,
	)
	grasp, err := runMockSkill(ctx, runtime, SkillStartRequest{
		ProjectID: "project-mock", TaskID: "task-depalletizing", SubtaskID: "grasp",
		RobotID: robotID, SkillName: "grasp-object", Version: "0.1.0",
		Input: map[string]any{
			"object_ref": "object://pallet-a/box-17", "gripper_ref": "component://gripper/main",
			"preferred_strategy": "top",
		},
	})
	if err != nil {
		return MockDemoResult{}, err
	}
	held, ok := grasp.Result["held_object"].(map[string]any)
	if !ok {
		return MockDemoResult{}, fmt.Errorf("grasp result does not contain held_object")
	}
	navigation, err := runMockSkill(ctx, runtime, SkillStartRequest{
		ProjectID: "project-mock", TaskID: "task-depalletizing", SubtaskID: "navigate",
		RobotID: robotID, SkillName: "semantic-navigation", Version: "0.1.0",
		Input: map[string]any{
			"target": map[string]any{
				"target_ref": "semantic://pallet-b/preplace",
				"pose":       MockPose(2.2, 1, 0, "nav-target-r1"), "map_type": "simulation",
				"map_generation": "generation-1", "map_revision": 1,
			},
			"navigation_purpose": "carry_to_place", "carrying_object": held,
			"require_visual_confirmation": true,
		},
	})
	if err != nil {
		return MockDemoResult{}, err
	}
	carried, ok := navigation.Result["carrying_object"].(map[string]any)
	if !ok {
		return MockDemoResult{}, fmt.Errorf("navigation result does not contain carrying_object")
	}
	placement, err := runMockSkill(ctx, runtime, SkillStartRequest{
		ProjectID: "project-mock", TaskID: "task-depalletizing", SubtaskID: "place",
		RobotID: robotID, SkillName: "place-object", Version: "0.1.0",
		Input: map[string]any{
			"held_object": carried,
			"target": map[string]any{
				"target_ref": "slot://pallet-b/cell-07", "region_ref": "region://pallet-b",
				"support_surface_ref": "surface://pallet-b", "stability_duration_ms": 800,
				"expected_scene_revision": "scene-place-42",
			},
		},
	})
	if err != nil {
		return MockDemoResult{}, err
	}
	return MockDemoResult{
		RobotID: robotID, Grasp: grasp, Navigation: navigation, Placement: placement,
		TaskStarts: client.StartCounts(),
	}, nil
}

func runMockSkill(ctx context.Context, runtime *SkillRuntime, request SkillStartRequest) (SkillExecution, error) {
	started, err := runtime.Start(ctx, request)
	if err != nil {
		return SkillExecution{}, err
	}
	finished, err := runtime.Wait(ctx, started.ID)
	if err != nil {
		return finished, err
	}
	if finished.Status != SkillCompleted {
		return finished, fmt.Errorf("skill %s ended as %s: %v", request.SkillName, finished.Status, finished.Error)
	}
	return finished, nil
}

// MockAgentGateway 为第一阶段提供类型化回复；正常演示无需调用它。
type MockAgentGateway struct{}

func (*MockAgentGateway) Request(_ context.Context, request AgentRequest) (AgentReply, error) {
	return AgentReply{
		ExecutionID: request.ExecutionID, SkillName: request.SkillName, Stage: request.Stage,
		DecisionKey: request.DecisionKey, DecisionRevision: request.DecisionRevision,
		Payload: map[string]any{
			"expected_plan_revision": request.DecisionRevision,
			"action":                 "abort_subtask",
			"reason":                 "确定性 Gate 不自动重放物理动作：" + request.Reason,
		},
	}, nil
}

func cloneStringIntMap(source map[string]int) map[string]int {
	result := make(map[string]int, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

// 防止调用方忘记给整个演示设置截止时间。
func DefaultMockDemoContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 20*time.Second)
}
