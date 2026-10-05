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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestRuntimeInstallationSetEnabledPersistsOnlyManagedField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "native.yaml")
	manifest := `schema_version: 1
installation_id: local-native
profile:
  runtime_profile_id: native-mujoco
  name: Native MuJoCo
  engine: mujoco
  loader: native
  api_version: v1
  scene_kinds: [asset_scene]
  capabilities:
    editable_scene: true
    viewer: true
    scene_reset: true
launch_mode: remote
endpoint: http://127.0.0.1:18090
enabled: true
installed_version: 0.4.0-test
`
	if err := os.WriteFile(path, []byte(manifest), 0o640); err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadRuntimeInstallations(dir)
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := catalog.SetEnabled("local-native", false)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Enabled || disabled.Status != "disabled" {
		t.Fatalf("停用后的公共状态不正确: %+v", disabled)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "enabled: false") ||
		!strings.Contains(text, "endpoint: http://127.0.0.1:18090") {
		t.Fatalf("清单写回丢失固定配置: %s", text)
	}

	enabled, err := catalog.SetEnabled("local-native", true)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled.Enabled || enabled.Status != "offline" || enabled.ProfileID != "native-mujoco" {
		t.Fatalf("重新启用后的公共状态不正确: %+v", enabled)
	}
	reloaded, err := LoadRuntimeInstallationFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Enabled || reloaded.Endpoint != "http://127.0.0.1:18090" {
		t.Fatalf("磁盘清单没有保存启用状态: %+v", reloaded)
	}
}

func TestEmbeddedRuntimeInstallationIsReadableButImmutable(t *testing.T) {
	fsys := fstest.MapFS{
		"runtimes.d/native.yaml": {Data: []byte(`schema_version: 1
installation_id: embedded-native
profile:
  runtime_profile_id: native-mujoco
  name: Native MuJoCo
  engine: mujoco
  loader: native
  api_version: v1
  scene_kinds: [asset_scene]
  capabilities: {}
launch_mode: remote
endpoint: http://127.0.0.1:18090
enabled: true
`)},
	}
	catalog, err := LoadRuntimeInstallationsFS(fsys, "runtimes.d")
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.Views(); len(got) != 1 || got[0].InstallationID != "embedded-native" {
		t.Fatalf("内置清单读取错误: %+v", got)
	}
	if _, err := catalog.SetEnabled("embedded-native", false); err == nil ||
		!strings.Contains(err.Error(), "只读模板") {
		t.Fatalf("内置清单不允许由网页修改: %v", err)
	}
}

func TestRuntimeInstallationViewsReturnIndependentCopies(t *testing.T) {
	catalog := &RuntimeInstallationCatalog{
		items: map[string]RuntimeInstallation{
			"one": {
				InstallationID: "one",
				Enabled:        true,
				Profile: RuntimeProfile{
					RuntimeProfileID: "native-mujoco",
					Capabilities:     RuntimeCapability{RobotModels: []string{"r1pro"}},
				},
				HardwareRequirements: map[string]string{"gpu": "optional"},
			},
		},
		order: []string{"one"},
	}
	catalog.ObserveStatus("one", "ready", "")
	views := catalog.Views()
	if views[0].Status != "ready" {
		t.Fatalf("运行期观测状态没有进入公共视图: %+v", views[0])
	}
	views[0].HardwareRequirements["gpu"] = "changed"
	views[0].Capabilities.RobotModels[0] = "changed"
	if got := catalog.Views()[0].HardwareRequirements["gpu"]; got != "optional" {
		t.Fatalf("公共视图修改污染了安装清单: %s", got)
	}
	if got := catalog.Views()[0].Capabilities.RobotModels[0]; got != "r1pro" {
		t.Fatalf("公共能力视图修改污染了安装清单: %s", got)
	}
}
