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
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecLauncherMissingBinaryIsUnavailable(t *testing.T) {
	launcher := ExecLauncher{Command: filepath.Join(t.TempDir(), "plugin-mujoco")}
	_, err := launcher.Start(context.Background())
	if !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("缺入口应映射为 Runtime 不可用, 得到 %v", err)
	}
	if !strings.Contains(err.Error(), "入口不存在") {
		t.Fatalf("错误应指出入口不存在: %v", err)
	}
}

func TestExecLauncherEmptyCommandIsUnavailable(t *testing.T) {
	_, err := ExecLauncher{}.Start(context.Background())
	if !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("空命令应映射为 Runtime 不可用, 得到 %v", err)
	}
}

func TestExecLauncherResolvesBinaryFromPATH(t *testing.T) {
	launcher := ExecLauncher{Command: "sleep", Args: []string{"30"}}
	process, err := launcher.Start(context.Background())
	if err != nil {
		t.Fatalf("PATH 中存在的 Runtime 入口应能启动: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := process.Stop(ctx); err != nil {
		t.Fatalf("停止测试 Runtime: %v", err)
	}
}
