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
	"os"
	"path/filepath"
	"testing"
)

func TestListInstalledIncludesDisabledPackage(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "packages", "grasp-object", "0.2.0")
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o750); err != nil {
		t.Fatal(err)
	}
	document := `---
name: grasp-object
description: grasp
category: robot_skill
version: 0.2.0
runtime:
  api_version: 1
  python: python
  entrypoint: scripts.skill:run
  stop_entrypoint: scripts.skill:stop
  input_model: scripts.models:Input
  state_model: scripts.models:State
  result_model: scripts.models:Result
required_actions:
  - type: robot.get_state
    schema_version: 2
stop_actions:
  - type: robot.stop
    schema_version: 2
---
# grasp
`
	for name, content := range map[string]string{
		"SKILL.md":          document,
		"requirements.lock": "",
		"scripts/skill.py":  "def run(): pass\ndef stop(): pass\n",
		"scripts/models.py": "class Input: pass\nclass State: pass\nclass Result: pass\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	catalog := &SkillCatalog{byName: make(map[string]SkillDefinition)}
	manager := &InstalledSkillManager{BaseDirectory: base, Catalog: catalog}
	actual := manager.ListInstalled()
	if len(actual) != 1 || actual[0].Definition.Name != "grasp-object" || actual[0].Enabled {
		t.Fatalf("disabled package snapshot=%#v", actual)
	}
	_, err := LoadSkillDefinition(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enable("grasp-object", "0.2.0"); err != nil {
		t.Fatal(err)
	}
	// Replacing an already active version must also work on Windows.
	if _, err := manager.Enable("grasp-object", "0.2.0"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := ScanSkillCatalog(filepath.Join(base, "active"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reloaded.Resolve("grasp-object", "0.2.0"); err != nil {
		t.Fatal(err)
	}
	actual = manager.ListInstalled()
	if len(actual) != 1 || !actual[0].Enabled {
		t.Fatalf("enabled package snapshot=%#v", actual)
	}
	if err := manager.Disable("grasp-object", "other-version"); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Resolve("grasp-object", "0.2.0"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Disable("grasp-object", "0.2.0"); err != nil {
		t.Fatal(err)
	}
	reloaded, err = ScanSkillCatalog(filepath.Join(base, "active"))
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.List()) != 0 {
		t.Fatal("disabled skill reappeared after restart")
	}
	if _, err := LoadSkillDefinition(root); err != nil {
		t.Fatal("disabling removed package:", err)
	}
}
