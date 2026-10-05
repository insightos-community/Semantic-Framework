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
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
	"insightos.cn/semantic-framework/pkg/config"
)

const disabledRuntimeManifest = `schema_version: 1
installation_id: test-native
profile:
  runtime_profile_id: native-mujoco
  name: Test Native
  engine: mujoco
  loader: native
  api_version: v1
  scene_kinds: [scene_document]
  capabilities:
    editable_scene: true
launch_mode: process
enabled: false
installed_version: test
`

func TestRuntimeCLIRegisterCheckRemove(t *testing.T) {
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtimes.d")
	catalogDir := filepath.Join(root, "scenes.d")
	if err := os.MkdirAll(catalogDir, 0o750); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Simulation.RuntimesDir = runtimeDir
	cfg.Simulation.CatalogDir = catalogDir
	configBytes, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "semantic-server.yaml")
	if err := os.WriteFile(configPath, configBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "runtime.yaml")
	if err := os.WriteFile(source, []byte(disabledRuntimeManifest), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := runRuntimeRegister([]string{"-c", configPath, "--file", source}); code != 0 {
		t.Fatalf("register code=%d", code)
	}
	target := filepath.Join(runtimeDir, "test-native.yaml")
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o640 {
		t.Fatalf("manifest mode=%v", info.Mode())
	}
	if code := runRuntimeCheck([]string{"-c", configPath, "--id", "test-native"}); code != 0 {
		t.Fatalf("check code=%d", code)
	}
	if code := runRuntimeRegister([]string{"-c", configPath, "--file", source}); code == 0 {
		t.Fatal("未显式 --replace 时不得覆盖")
	}
	if code := runRuntimeRemove([]string{"-c", configPath, "--id", "test-native"}); code != 0 {
		t.Fatalf("remove code=%d", code)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("manifest 应被移除，err=%v", err)
	}
}

func TestRuntimeCLIRejectsUnsafeInstallationID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unsafe.yaml")
	data := []byte(`schema_version: 1
installation_id: ../../escape
profile:
  runtime_profile_id: native-mujoco
  name: Unsafe
  engine: mujoco
  loader: native
  api_version: v1
launch_mode: remote
endpoint: http://127.0.0.1:8090
enabled: true
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runRuntimeCheck([]string{"--file", path}); code == 0 {
		t.Fatal("不安全 installation_id 必须被拒绝")
	}
}
