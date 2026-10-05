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

package simulation

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestV040PublicInterfaceFixtures(t *testing.T) {
	var profile RuntimeProfile
	readFixture(t, "runtime-profile-native.json", &profile)
	if profile.RuntimeProfileID != "native-mujoco" ||
		profile.APIVersion != "v1" ||
		!slices.Equal(profile.SceneKinds, []string{"scene_document", "asset_scene"}) ||
		!slices.Equal(profile.Capabilities.RobotModels, []string{"r1_pro_chassis"}) ||
		profile.Environment != "project" ||
		!profile.EnvironmentReady || !profile.Available ||
		!profile.Capabilities.EditableScene {
		t.Fatalf("RuntimeProfile 样例无效: %+v", profile)
	}

	var bundle RuntimeBundle
	readFixture(t, "runtime-bundle.json", &bundle)
	if bundle.RuntimeProfileID != profile.RuntimeProfileID ||
		bundle.Document.SceneKind != "scene_document" ||
		len(bundle.Document.Nodes) != 1 ||
		bundle.Document.Nodes[0].Properties["model"] != "r1_pro_chassis" ||
		!bundle.Validation.Valid {
		t.Fatalf("RuntimeBundle 样例无效: %+v", bundle)
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if strings.Contains(text, "project_id") || strings.Contains(text, "source_path") ||
		strings.Contains(text, "\"status\":\"draft\"") {
		t.Fatalf("RuntimeBundle 暴露了 Framework 所有权或宿主实现字段: %s", text)
	}

	var start SceneStartRequest
	readFixture(t, "scene-start-request.json", &start)
	if start.RuntimeProfileID != profile.RuntimeProfileID ||
		start.RuntimeBundleID != bundle.RuntimeBundleID ||
		start.Layout != "layout001" ||
		start.RenderBackend != "egl" {
		t.Fatalf("SceneStartRequest 样例无效: %+v", start)
	}

	var command RobotDebugCommand
	readFixture(t, "robot-command-joint.json", &command)
	robot := VirtualRobotDescriptor{
		RobotID: "r1", JointNames: []string{"left_arm_joint1"},
		Capabilities: RobotCapability{
			Commands: []RobotCommandType{RobotCommandJointTrajectory},
		},
	}
	if err := validateRobotDebugCommand(robot, command); err != nil {
		t.Fatalf("低层轨迹命令样例无效: %v", err)
	}
	if command.CommandID != "joint-001" ||
		command.SceneGeneration != 1 ||
		command.Joint.Points[1].Positions["left_arm_joint1"] != 0.3 {
		t.Fatalf("低层轨迹命令样例发生漂移: %+v", command)
	}
}

func TestV040FixtureRejectsUnknownFields(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "v1", "runtime-profile-native.json"))
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("{"), []byte("{\"unexpected\":true,"), 1)
	var profile RuntimeProfile
	if err := decodeStrictFixture(data, &profile); err == nil {
		t.Fatal("canonical fixture 出现未知字段时必须失败，不能被 encoding/json 静默忽略")
	}
}

func readFixture(t *testing.T, name string, target any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "v1", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeStrictFixture(data, target); err != nil {
		t.Fatalf("解析样例 %s 失败: %v", name, err)
	}
}

func decodeStrictFixture(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("canonical fixture 只能包含一个 JSON 值")
	}
	return err
}
