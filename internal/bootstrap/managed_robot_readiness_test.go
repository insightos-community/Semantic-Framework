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
	"io"
	"strings"
	"testing"
	"time"

	robotdomain "insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/robotruntime"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/log"
)

func TestManagedRobotReadinessUsesServerDesiredOverDeploymentSeed(t *testing.T) {
	for _, test := range []struct {
		name          string
		serverVersion string
		actualVersion string
		wantVersion   string
		wantReady     bool
	}{
		{"new_server_desired_old_deployment", "0.4.22", "0.4.22", "0.4.22", true},
		{"old_actual_does_not_satisfy_new_desired", "0.4.22", "0.4.21", "0.4.22", false},
		{"first_connection_seeds_deployment", "", "0.4.21", "0.4.21", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
			st := storetest.OpenMigrated(t, logger)
			const robotID, pilotID = "readiness-robot", "readiness-pilot"
			if test.serverVersion != "" {
				if err := st.SaveRobotDesiredSkill(store.RobotDesiredSkill{
					RobotID: robotID, Name: "grasp-object", Version: test.serverVersion,
					Enabled: true, UpdatedAt: time.Now().UTC(),
				}); err != nil {
					t.Fatal(err)
				}
			}
			robots := robotdomain.NewService(st, nil)
			_, disconnect, err := robots.ConnectWithSkillSnapshot(store.RobotPilot{
				PilotInstanceID: pilotID, RobotID: robotID, RobotModel: "readiness-model", Backend: "fake",
				AbilityFrameworkStatus: "ready", Abilities: []map[string]any{{"health": "healthy"}},
				DesiredSkills: []store.RobotDesiredSkill{{Name: "grasp-object", Version: "0.4.21", Enabled: true}},
			}, []store.RobotPilotSkill{{
				Name: "grasp-object", Version: test.actualVersion, Status: "installed", Enabled: true,
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer disconnect()
			desired, err := st.ListRobotDesiredSkills(robotID)
			if err != nil || len(desired) != 1 || desired[0].Version != test.wantVersion {
				t.Fatalf("接入后 Server desired 错误: %+v %v", desired, err)
			}
			pilot, err := st.GetRobotPilot(pilotID)
			if err != nil || len(pilot.DesiredSkills) != 1 || pilot.DesiredSkills[0].Version != "0.4.21" {
				t.Fatalf("回归前提应保留旧 Deployment 上报: %+v %v", pilot.DesiredSkills, err)
			}
			launcher := &managedRobotInstanceLauncher{robots: robots, store: st, logger: logger}
			instance := robotruntime.RuntimeInstance{RobotID: robotID, PilotInstanceID: pilotID}
			ready, reason := launcher.pilotReady(instance)
			if ready != test.wantReady || (!ready && !strings.Contains(reason, "grasp-object@"+test.wantVersion)) {
				t.Fatalf("就绪应按 Server desired 核对实际版本: ready=%v reason=%q", ready, reason)
			}

			// 同一个等待器在后续轮询重新读取 desired，不能缓存启动时的版本。
			if err := st.SaveRobotDesiredSkill(store.RobotDesiredSkill{
				RobotID: robotID, Name: "grasp-object", Version: "0.4.23", Enabled: false,
				UpdatedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatal(err)
			}
			if ready, reason := launcher.pilotReady(instance); ready || !strings.Contains(reason, "grasp-object@0.4.23") {
				t.Fatalf("下一轮就绪检查必须采用更新后的 desired: ready=%v reason=%q", ready, reason)
			}
			if err := st.SaveRobotPilotSkill(store.RobotPilotSkill{
				PilotInstanceID: pilotID, Name: "grasp-object", Version: "0.4.23", Status: "installed", Enabled: false,
			}); err != nil {
				t.Fatal(err)
			}
			if ready, reason := launcher.pilotReady(instance); !ready {
				t.Fatalf("新版实际目录及 enabled 收敛后应就绪: %s", reason)
			}
		})
	}
}
