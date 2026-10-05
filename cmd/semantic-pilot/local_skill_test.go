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

package main

import (
	"strings"
	"testing"
)

func TestSplitSkillReference(t *testing.T) {
	name, version, err := splitSkillReference("grasp-object@0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if name != "grasp-object" || version != "0.2.0" {
		t.Fatalf("unexpected reference %q %q", name, version)
	}
	if _, _, err := splitSkillReference("grasp-object"); err == nil {
		t.Fatal("reference without an exact version must be rejected")
	}
}

func TestReadJSONObject(t *testing.T) {
	value, err := readJSONObject("-", strings.NewReader(`{"target":{"object_ref":"tote-large-smoke"}}`))
	if err != nil {
		t.Fatal(err)
	}
	target, ok := value["target"].(map[string]any)
	if !ok || target["object_ref"] != "tote-large-smoke" {
		t.Fatalf("unexpected input: %#v", value)
	}
}
