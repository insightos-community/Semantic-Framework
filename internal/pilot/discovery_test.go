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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestAbilityFrameworkDiscoveryBuildsExactActionBinding(t *testing.T) {
	heartbeats := []abilityHeartbeat{{ID: "nav-1", InstanceName: "navigation-a",
		AbilityName: "R1ProNavigation.V1", Version: "0.1.0", State: "Standby"}}
	server := discoveryServer(t, heartbeats, map[string][]map[string]any{
		"nav-1": navigationManifestTasks(),
	})
	defer server.Close()
	deployment := testDeployment(server.URL)
	client := NewAbilityFrameworkClient(server.URL, nil)
	catalog := NewCatalog()
	discovery := NewAbilityFrameworkDiscovery(deployment, client, catalog, nil)
	discovery.HTTPClient = server.Client()
	if err := discovery.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	binding, err := catalog.Resolve("r1pro-test", ActionRef{Type: "navigation.follow_route", SchemaVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if binding.InstanceID != "nav-1" || binding.TaskName != "FollowRoute" || !binding.Physical {
		t.Fatalf("unexpected binding: %#v", binding)
	}
	snapshot := discovery.Snapshot()
	if snapshot.Status != "ready" || snapshot.Revision != 1 {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
	if len(snapshot.Abilities) != 1 || snapshot.Abilities[0]["selected"] != true {
		t.Fatalf("unexpected abilities: %#v", snapshot.Abilities)
	}
	view := snapshot.Abilities[0]
	if view["status"] != "ready" || view["health"] != "healthy" {
		t.Fatalf("Ability 设备视图没有提供可解释健康状态: %#v", view)
	}
	debugTasks, _ := view["debug_tasks"].([]map[string]any)
	if len(debugTasks) != 3 || debugTasks[0]["name"] != "FollowRoute" {
		t.Fatalf("Ability 调试 Task 应来自 Manifest 并排除管理 Task: %#v", view)
	}
	actions, _ := view["actions"].([]string)
	if len(actions) != 3 || actions[0] != "navigation.follow_route" {
		t.Fatalf("Ability 语义 Action 没有从 Manifest 暴露: %#v", view)
	}
	if debugTasks[0]["input_model"] != "models:FollowRouteInput" {
		t.Fatalf("Ability Task 输入模型没有从 Manifest 暴露: %#v", debugTasks)
	}
	inputFields, _ := debugTasks[0]["input_fields"].([]map[string]any)
	if len(inputFields) != 1 || inputFields[0]["name"] != "route_ref" {
		t.Fatalf("Ability Task 参数说明没有从 Manifest 暴露: %#v", debugTasks)
	}
}

func TestAbilityFrameworkDiscoveryRefusesAmbiguousRoleUnlessConfigured(t *testing.T) {
	heartbeats := []abilityHeartbeat{
		{ID: "nav-1", AbilityName: "R1ProNavigation.V1", Version: "0.1.0", State: "Standby"},
		{ID: "nav-2", AbilityName: "R1ProNavigation.V1", Version: "0.1.0", State: "Running"},
	}
	tasks := navigationManifestTasks()
	server := discoveryServer(t, heartbeats, map[string][]map[string]any{"nav-1": tasks, "nav-2": tasks})
	defer server.Close()
	deployment := testDeployment(server.URL)
	client, catalog := NewAbilityFrameworkClient(server.URL, nil), NewCatalog()
	discovery := NewAbilityFrameworkDiscovery(deployment, client, catalog, nil)
	discovery.HTTPClient = server.Client()
	if err := discovery.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Resolve("r1pro-test", ActionRef{Type: "navigation.follow_route", SchemaVersion: 1}); !errors.Is(err, ErrActionNotBound) {
		t.Fatalf("ambiguous instance was selected: %v", err)
	}

	deployment.Abilities["navigation"] = AbilityDeployment{InstanceID: "nav-2"}
	discovery = NewAbilityFrameworkDiscovery(deployment, client, catalog, nil)
	discovery.HTTPClient = server.Client()
	if err := discovery.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	binding, err := catalog.Resolve("r1pro-test", ActionRef{Type: "navigation.follow_route", SchemaVersion: 1})
	if err != nil || binding.InstanceID != "nav-2" {
		t.Fatalf("configured instance not selected: %#v, %v", binding, err)
	}
}

func TestLoadRobotDeploymentUsesOneRobotProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "robot.yaml")
	content := "api_version: 1\nrobot:\n  id: r1pro-test\n  model: r1pro\n  backend: fake\n" +
		"  sdk:\n    package: semantic-robot-sdk-r1pro\n    endpoint: http://127.0.0.1:8090\n    firmware_profile: fake-v1\n    providers: {navigation: local}\n" +
		"  frames:\n    world: world\n    base: base_link\n    end_effectors: {left: left_tool, right: right_tool}\n  tools:\n    - {tool_ref: component://tool/left, side: left, kind: tote_clamp, frame: left_tool, joint: left_joint}\n  kinematics: {urdf_path: /opt/robot.urdf}\n  safety: {maximum_base_speed: 0.4}\n" +
		"ability_framework:\n  endpoint: http://127.0.0.1:8080\nabilities:\n  navigation: {}\n" +
		"pilot:\n  heartbeat_interval_seconds: 2\n  allow_ability_debug: true\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	deployment, err := LoadRobotDeployment(path)
	if err != nil {
		t.Fatal(err)
	}
	if deployment.Robot.ID != "r1pro-test" || deployment.AbilityFramework.Endpoint != "http://127.0.0.1:8080" {
		t.Fatalf("unexpected deployment: %#v", deployment)
	}
	if deployment.Robot.Frames["end_effectors"] == nil || len(deployment.Robot.Tools) != 1 {
		t.Fatalf("双末端部署信息没有被 Pilot 保留: %#v", deployment.Robot)
	}
	configuration := deployment.DeviceConfiguration()
	sdk := configuration["sdk"].(map[string]any)
	if sdk["package"] != "semantic-robot-sdk-r1pro" || sdk["endpoint"] != "http://127.0.0.1:8090" {
		t.Fatalf("unexpected device configuration: %#v", configuration)
	}
}

func testDeployment(endpoint string) RobotDeployment {
	var result RobotDeployment
	result.APIVersion = 1
	result.Robot.ID = "r1pro-test"
	result.Robot.Model = "r1pro"
	result.Robot.Backend = "fake"
	result.AbilityFramework.Endpoint = endpoint
	result.Abilities = map[string]AbilityDeployment{"navigation": {}}
	return result
}

func navigationManifestTasks() []map[string]any {
	return []map[string]any{
		{"taskType": 0, "taskName": "PlanRoute", "abilityRole": "navigation", "actionType": "navigation.plan_route", "schemaVersion": 1, "physical": false},
		{"taskType": 1, "taskName": "FollowRoute", "abilityRole": "navigation", "actionType": "navigation.follow_route", "schemaVersion": 1, "physical": true, "inputModel": "models:FollowRouteInput", "inputFields": []map[string]any{{"name": "route_ref", "type": "string", "required": true, "description": "路线引用"}}},
		{"taskType": 2, "taskName": "VerifyArrival", "abilityRole": "navigation", "actionType": "navigation.verify_arrival", "schemaVersion": 1, "physical": false},
		{"taskType": 3, "taskName": "GetExecution"},
		{"taskType": 4, "taskName": "StopExecution"},
	}
}

func discoveryServer(t *testing.T, heartbeats []abilityHeartbeat, tasks map[string][]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/ability-heartbeat" {
			_ = json.NewEncoder(w).Encode(heartbeats)
			return
		}
		const prefix = "/api/manifest/"
		if len(r.URL.Path) > len(prefix) && r.URL.Path[:len(prefix)] == prefix {
			// 同一 Ability 版本的多个实例共享一份真实 Manifest。
			for _, heartbeat := range heartbeats {
				if r.URL.Path == prefix+heartbeat.AbilityName+"/"+heartbeat.Version {
					_ = json.NewEncoder(w).Encode(map[string]any{"tasks": tasks[heartbeat.ID]})
					return
				}
			}
		}
		http.NotFound(w, r)
	}))
}

func TestNamedAbilitiesRouteSplitMotionActions(t *testing.T) {
	for _, conflicting := range []bool{false, true} {
		t.Run(fmt.Sprint(conflicting), func(t *testing.T) {
			heartbeats := []abilityHeartbeat{
				{ID: "arm-1", AbilityName: "Arm.V2", Version: "1", State: "Standby"},
				{ID: "torso-1", AbilityName: "Torso.V2", Version: "1", State: "Standby"},
			}
			tasks := func(action string) []map[string]any {
				return []map[string]any{
					{"taskName": "Move", "taskType": 0, "actionType": action, "abilityRole": "manipulator_motion", "schemaVersion": 2, "physical": true},
					{"taskName": "GetExecution", "taskType": 1}, {"taskName": "StopExecution", "taskType": 2},
				}
			}
			torsoAction := "motion.set_torso_state"
			if conflicting {
				torsoAction = "motion.move_arm_joint"
			}
			server := discoveryServer(t, heartbeats, map[string][]map[string]any{"arm-1": tasks("motion.move_arm_joint"), "torso-1": tasks(torsoAction)})
			defer server.Close()
			deployment := testDeployment(server.URL)
			deployment.Abilities = map[string]AbilityDeployment{"arm": {AbilityName: "Arm.V2"}, "torso": {AbilityName: "Torso.V2"}}
			catalog := NewCatalog()
			discovery := NewAbilityFrameworkDiscovery(deployment, NewAbilityFrameworkClient(server.URL, nil), catalog, nil)
			discovery.HTTPClient = server.Client()
			if err := discovery.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			binding, err := catalog.Resolve("r1pro-test", ActionRef{Type: "motion.move_arm_joint", SchemaVersion: 2})
			if conflicting {
				if !errors.Is(err, ErrActionNotBound) || discovery.Snapshot().Status == "ready" {
					t.Fatalf("conflicting action was routed: %#v %v", binding, err)
				}
				return
			}
			if err != nil || binding.InstanceID != "arm-1" {
				t.Fatalf("arm routing failed: %#v %v", binding, err)
			}
			binding, err = catalog.Resolve("r1pro-test", ActionRef{Type: torsoAction, SchemaVersion: 2})
			if err != nil || binding.InstanceID != "torso-1" || discovery.Snapshot().Status != "ready" {
				t.Fatalf("torso routing failed: %#v %v", binding, err)
			}
		})
	}
}

func TestNamedAbilityRequiresUniqueHealthyInstance(t *testing.T) {
	items := []discoveredAbility{
		{role: "manipulator_motion", heartbeat: abilityHeartbeat{ID: "first", AbilityName: "Arm.V2", State: "Standby"}},
		{role: "manipulator_motion", heartbeat: abilityHeartbeat{ID: "second", AbilityName: "Arm.V2", State: "Standby"}},
	}
	desired := AbilityDeployment{AbilityName: "Arm.V2"}
	if got := selectAbilityInstance(items, "arm", desired); got != -1 {
		t.Fatal("selected ambiguous instance", got)
	}
	desired.InstanceID = "second"
	if got := selectAbilityInstance(items, "arm", desired); got != 1 {
		t.Fatal("explicit instance not selected", got)
	}
	desired.AbilityName = "Missing.V2"
	if got := selectAbilityInstance(items, "arm", desired); got != -1 {
		t.Fatal("name filter ignored", got)
	}
}
