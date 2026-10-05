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
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func binding(actionType, instance string, physical bool) AbilityBinding {
	action := ActionRef{Type: actionType, SchemaVersion: 1}
	return AbilityBinding{Action: action, AbilityName: "R1Pro", TaskName: actionType, InstanceID: instance, Physical: physical}
}

func profile(robotID string, bindings ...AbilityBinding) RobotProfile {
	result := RobotProfile{RobotID: robotID, Bindings: make(map[string]AbilityBinding)}
	for _, item := range bindings {
		result.Bindings[item.Action.Key()] = item
	}
	return result
}

func TestCatalogResolvesExactRobotActionAndVersion(t *testing.T) {
	catalog := NewCatalog(
		profile("robot-a", binding("navigation.follow_route", "navigation-a", true)),
		profile("robot-b", binding("navigation.follow_route", "navigation-b", true)),
	)
	resolved, err := catalog.Resolve("robot-b", ActionRef{Type: "navigation.follow_route", SchemaVersion: 1})
	if err != nil {
		t.Fatalf("resolve binding: %v", err)
	}
	if resolved.InstanceID != "navigation-b" {
		t.Fatalf("unexpected instance %q", resolved.InstanceID)
	}
	if _, err := catalog.Resolve("robot-b", ActionRef{Type: "navigation.follow_route", SchemaVersion: 2}); !errors.Is(err, ErrInterfaceMismatch) {
		t.Fatalf("expected interface mismatch, got %v", err)
	}
	if _, err := catalog.Resolve("missing", ActionRef{Type: "navigation.follow_route", SchemaVersion: 1}); !errors.Is(err, ErrRobotNotFound) {
		t.Fatalf("expected robot not found, got %v", err)
	}
}

func TestSkillCatalogReadsOnlySkillMarkdownFrontmatter(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "navigation")
	if err := os.MkdirAll(filepath.Join(directory, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"skill.py", "models.py"} {
		if err := os.WriteFile(filepath.Join(directory, "scripts", name), []byte("def run(): pass\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "requirements.lock"), []byte("pydantic==2.13.2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	document := `---
name: semantic-navigation
description: test
category: robot_skill
version: 0.1.0
runtime:
  api_version: 1
  python: ">=3.11"
  entrypoint: scripts.skill:run
  stop_entrypoint: scripts.skill:run
  input_model: scripts.models:run
  state_model: scripts.models:run
  result_model: scripts.models:run
  controllers: {}
required_actions:
  - {type: navigation.follow_route, schema_version: 1}
stop_actions:
  - {type: navigation.follow_route, schema_version: 1}
---
# test
`
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog, err := ScanSkillCatalog(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	definition, err := catalog.Resolve("semantic-navigation", "0.1.0")
	if err != nil || definition.Runtime.APIVersion != 1 {
		t.Fatalf("resolve: %#v %v", definition, err)
	}
}
