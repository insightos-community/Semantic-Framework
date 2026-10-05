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
	"encoding/json"
	"testing"
)

func TestSetPropertyUpdatesDocumentPhysicsWithoutNode(t *testing.T) {
	document := SceneDocument{Physics: map[string]any{"timestep_seconds": 0.002}}

	err := applySceneOperation(&document, SceneOperation{
		Type:     SceneOperationSetProperty,
		Property: "physics",
		Value:    json.RawMessage(`{"gravity_m_s2":[0,0,-3.71],"timestep_seconds":0.001}`),
	})
	if err != nil {
		t.Fatalf("更新 SceneDocument physics 失败: %v", err)
	}
	if got := document.Physics["timestep_seconds"]; got != 0.001 {
		t.Fatalf("physics 没有落到场景文档: %#v", document.Physics)
	}
}

func TestDocumentSetPropertyRejectsUnknownFieldAndNonObjectPhysics(t *testing.T) {
	document := SceneDocument{}
	cases := []SceneOperation{
		{Type: SceneOperationSetProperty, Property: "name", Value: json.RawMessage(`"scene"`)},
		{Type: SceneOperationSetProperty, Property: "physics", Value: json.RawMessage(`[]`)},
	}
	for _, operation := range cases {
		if err := applySceneOperation(&document, operation); err == nil {
			t.Fatalf("文档级非法 SetProperty 应被拒绝: %#v", operation)
		}
	}
}

func TestValidateScenePhysicsRejectsUnknownAndOutOfRangeValues(t *testing.T) {
	issues := validateScenePhysics(SceneDocument{Physics: map[string]any{
		"gravity_m_s2":     []any{0, "down", -9.81},
		"timestep_seconds": 0.2,
		"solver":           "hidden",
	}})
	fields := make(map[string]bool, len(issues))
	for _, issue := range issues {
		fields[issue.Field] = true
	}
	if !fields["physics.gravity_m_s2"] || !fields["physics.timestep_seconds"] || !fields["physics.solver"] {
		t.Fatalf("物理参数问题未被完整定位: %#v", issues)
	}
}
