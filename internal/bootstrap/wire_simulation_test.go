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

package bootstrap

import (
	"reflect"
	"testing"

	"insightos.cn/semantic-framework/internal/simulation"
)

// Runtime 的启动命令必须来自经过严格校验的 runtimes.d 清单，Bootstrap 不再
// 根据 profile_id 拼接命令。这个测试同时保证三个 MuJoCo Profile 使用彼此隔离
// 的 uv 环境，并且只把管理员允许的环境引用传给子进程。
func TestRuntimeInstallationsUseManifestLaunchers(t *testing.T) {
	t.Setenv("SEMANTIC_MUJOCO_WORKDIR", "/tmp/plugin-mujoco")
	t.Setenv("SEMANTIC_MUJOCO_ASSET_ROOT", "/tmp/mujoco-assets")
	t.Setenv("SEMANTIC_MUJOCO_GL", "egl")
	t.Setenv("SEMANTIC_FRANKA_MODEL_ROOT", "/tmp/franka-model")
	t.Setenv("SEMANTIC_LIBERO_ROOT", "/tmp/libero")

	catalog, err := configuredRuntimeInstallations("configs/runtimes.d")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		installationID string
		wantArgs       []string
		wantEnv        []string
	}{
		{
			installationID: "local-native-mujoco",
			wantArgs:       []string{"run", "--frozen", "plugin-mujoco"},
			wantEnv: []string{
				"MUJOCO_ASSET_ROOT=/tmp/mujoco-assets",
				"MUJOCO_GL=egl",
			},
		},
		{
			installationID: "local-robosuite-1-5",
			wantArgs: []string{
				"run", "--project", "profiles/robosuite", "--frozen",
				"semantic-sim-runtime",
			},
			wantEnv: []string{"FRANKA_MODEL_ROOT=/tmp/franka-model"},
		},
		{
			installationID: "local-libero-1-4",
			wantArgs: []string{
				"run", "--project", "profiles/libero", "--frozen",
				"semantic-sim-runtime",
			},
			wantEnv: []string{
				"FRANKA_MODEL_ROOT=/tmp/franka-model",
				"SEMANTIC_LIBERO_ROOT=/tmp/libero",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.installationID, func(t *testing.T) {
			installation, getErr := catalog.Get(test.installationID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			launcher, ok := installation.Binding().Launcher.(simulation.ExecLauncher)
			if !ok {
				t.Fatalf("launcher type = %T", installation.Binding().Launcher)
			}
			if launcher.Command != "uv" || !reflect.DeepEqual(launcher.Args, test.wantArgs) {
				t.Fatalf("command = %s %v", launcher.Command, launcher.Args)
			}
			if launcher.Dir != "/tmp/plugin-mujoco" {
				t.Fatalf("workdir = %q", launcher.Dir)
			}
			if !reflect.DeepEqual(launcher.Env, test.wantEnv) {
				t.Fatalf("env = %v, want %v", launcher.Env, test.wantEnv)
			}
		})
	}
}

func TestRuntimeManifestDoesNotAcceptLegacyCommandOverride(t *testing.T) {
	t.Setenv("SEMANTIC_MUJOCO_WORKDIR", "/tmp/plugin-mujoco")
	t.Setenv("SEMANTIC_MUJOCO_COMMAND", "custom-runtime --serve")

	catalog, err := configuredRuntimeInstallations("configs/runtimes.d")
	if err != nil {
		t.Fatal(err)
	}
	installation, err := catalog.Get("local-native-mujoco")
	if err != nil {
		t.Fatal(err)
	}
	launcher, ok := installation.Binding().Launcher.(simulation.ExecLauncher)
	if !ok {
		t.Fatalf("launcher type = %T", installation.Binding().Launcher)
	}
	if launcher.Command != "uv" || !reflect.DeepEqual(
		launcher.Args, []string{"run", "--frozen", "plugin-mujoco"},
	) {
		t.Fatalf("Runtime 启动命令必须来自清单，got %s %v", launcher.Command, launcher.Args)
	}
}
