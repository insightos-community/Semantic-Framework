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
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestJSONLineEventSinkIncludesExecutionContext(t *testing.T) {
	var output bytes.Buffer
	sink := NewJSONLineEventSink(&output)
	sink.now = func() time.Time { return time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC) }
	execution := SkillExecution{
		ID: "skill-1", ProjectID: "project-1", TaskID: "task-1", SubtaskID: "subtask-1",
		RobotID: "robot-1", SkillName: "grasp-object", SkillVersion: "0.1.0",
	}
	sink.Report(execution, "action.started", map[string]any{
		"action_key":  "grasp:close:1",
		"action_type": "gripper.close",
	})

	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if record["robot_id"] != "robot-1" || record["skill_execution_id"] != "skill-1" {
		t.Fatalf("missing execution fields: %#v", record)
	}
	fields, _ := record["fields"].(map[string]any)
	if fields["action_type"] != "gripper.close" {
		t.Fatalf("missing action fields: %#v", record)
	}
}

func TestRuntimeEventPayloadKeepsActionStatus(t *testing.T) {
	payload := runtimeEventPayload(
		SkillExecution{ID: "skill-1", Status: SkillRunning},
		map[string]any{"status": ActionSucceeded, "action_id": "action-1"},
	)
	if payload["status"] != ActionSucceeded {
		t.Fatalf("Action 终态不得被 Skill 状态覆盖: %#v", payload)
	}
	if payload["skill_status"] != SkillRunning || payload["execution_id"] != "skill-1" {
		t.Fatalf("Skill 状态与 Execution ID 必须独立上报: %#v", payload)
	}
}
