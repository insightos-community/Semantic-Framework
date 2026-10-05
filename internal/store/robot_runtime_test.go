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

package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/robotruntime"
)

func TestRobotRuntimeStoreSurvivesServerRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "server.db")
	open := func() *Store {
		return openMigratedStoreAt(t, path)
	}
	st := open()
	now := time.Now().UTC()
	first := robotruntime.RuntimeInstance{InstanceID: "runtime-a", PilotInstanceID: "pilot-a",
		RobotID: "robot-a", Status: robotruntime.StateStarting, Revision: 1, CreatedAt: now, UpdatedAt: now}
	second := robotruntime.RuntimeInstance{InstanceID: "runtime-b", PilotInstanceID: "pilot-b",
		RobotID: "robot-b", Status: robotruntime.StateReady, Revision: 2, CreatedAt: now, UpdatedAt: now}
	if err := st.SaveRuntimeInstance(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRuntimeInstance(ctx, second); err != nil {
		t.Fatal(err)
	}
	portA, err := st.AcquireAbilityFrameworkPort(ctx, first.InstanceID, 21000, 21002)
	if err != nil {
		t.Fatal(err)
	}
	portB, err := st.AcquireAbilityFrameworkPort(ctx, second.InstanceID, 21000, 21002)
	if err != nil || portA == portB {
		t.Fatalf("Runtime 端口未隔离: a=%d b=%d err=%v", portA, portB, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	restored := open()
	defer restored.Close()
	got, err := restored.GetActiveRuntimeByRobot(ctx, second.RobotID)
	if err != nil || got.InstanceID != second.InstanceID {
		t.Fatalf("Server 重启后 Runtime 未恢复: %#v err=%v", got, err)
	}
	gotPort, err := restored.AcquireAbilityFrameworkPort(ctx, first.InstanceID, 21000, 21002)
	if err != nil || gotPort != portA {
		t.Fatalf("Server 重启后端口租约未恢复: got=%d want=%d err=%v", gotPort, portA, err)
	}
	if err := restored.ReleaseAbilityFrameworkPort(ctx, first.InstanceID); err != nil {
		t.Fatal(err)
	}
	reused, err := restored.AcquireAbilityFrameworkPort(ctx, "runtime-c", 21000, 21002)
	if err != nil || reused != portA {
		t.Fatalf("确认释放后的端口应可复用: got=%d want=%d err=%v", reused, portA, err)
	}
}
