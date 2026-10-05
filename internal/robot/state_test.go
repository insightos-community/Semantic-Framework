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

package robot

import (
	"context"
	"errors"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/robotruntime"
)

func TestReadRobotStatePreservesObservationAndAvailability(t *testing.T) {
	service, _, request := directRunFixture(t)
	now := time.Now().UTC()
	service.now = func() time.Time { return now }
	if got := service.ReadRobotState(context.Background(), request.ProjectID, request.RobotID); got.Status != "unavailable" || got.InHold != nil || got.ObservedAt != nil {
		t.Fatalf("未配置读取不能伪造空载: %+v", got)
	}
	if err := service.st.SaveRuntimeInstance(context.Background(), robotruntime.RuntimeInstance{
		InstanceID: "runtime-state", RobotID: request.RobotID, ProjectID: request.ProjectID,
		SceneInstanceID: "scene-state", Status: robotruntime.StateReady,
	}); err != nil {
		t.Fatal(err)
	}
	object := "object://tote-1"
	for _, tc := range []struct {
		name, wantStatus, wantReason string
		observation                  StateObservation
		err                          error
	}{
		{"held", "observed", "", StateObservation{RobotID: request.RobotID, ObservedAt: now,
			Generation: 3, HoldingObject: &object, InHold: true}, nil},
		{"no_reported_object", "observed", "", StateObservation{RobotID: request.RobotID, ObservedAt: now}, nil},
		{"old", "stale", "observation_timestamp_out_of_range", StateObservation{RobotID: request.RobotID,
			ObservedAt: now.Add(-time.Minute), HoldingObject: &object, InHold: true}, nil},
		{"future", "stale", "observation_timestamp_out_of_range", StateObservation{RobotID: request.RobotID,
			ObservedAt: now.Add(time.Minute)}, nil},
		{"missing_timestamp", "stale", "observation_timestamp_missing", StateObservation{RobotID: request.RobotID}, nil},
		{"wrong_robot", "unavailable", "state_robot_mismatch", StateObservation{RobotID: "other", ObservedAt: now}, nil},
		{"read_failure", "unavailable", "state_read_failed", StateObservation{}, errors.New("private transport details")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service.SetRobotStateReader("simulation.robot_state", func(ctx context.Context, projectID, sceneID, robotID string) (StateObservation, error) {
				if projectID != request.ProjectID || sceneID != "scene-state" || robotID != request.RobotID {
					t.Fatalf("读取必须使用已校验的具体实例: %s/%s/%s", projectID, sceneID, robotID)
				}
				if _, bounded := ctx.Deadline(); !bounded {
					t.Fatal("只读状态请求必须有超时边界")
				}
				return tc.observation, tc.err
			})
			got := service.ReadRobotState(context.Background(), request.ProjectID, request.RobotID)
			if got.Status != tc.wantStatus || got.Source != "simulation.robot_state" || got.Reason != tc.wantReason || got.Limitation == "" {
				t.Fatalf("状态与来源必须如实返回: %+v", got)
			}
			if got.Status == "unavailable" {
				if got.InHold != nil || got.HoldingObject != nil || got.ObservedAt != nil {
					t.Fatalf("失败不能伪造空载或历史样本: %+v", got)
				}
				return
			}
			if got.InHold == nil || *got.InHold != tc.observation.InHold || got.HoldingObject != tc.observation.HoldingObject {
				t.Fatalf("不得改写接口真实携物语义: %+v", got)
			}
			if !tc.observation.ObservedAt.IsZero() && (got.ObservedAt == nil || !got.ObservedAt.Equal(tc.observation.ObservedAt)) {
				t.Fatalf("stale 也必须保留 observed_at: %+v", got)
			}
		})
	}
}

func TestReadRobotStateDoesNotQueryOutsideProject(t *testing.T) {
	service, _, request := directRunFixture(t)
	if err := service.st.SaveRuntimeInstance(context.Background(), robotruntime.RuntimeInstance{
		InstanceID: "runtime-other", RobotID: request.RobotID, ProjectID: "other-project",
		SceneInstanceID: "scene-other", Status: robotruntime.StateReady,
	}); err != nil {
		t.Fatal(err)
	}
	service.SetRobotStateReader("simulation.robot_state", func(context.Context, string, string, string) (StateObservation, error) {
		t.Fatal("不得查询其他 Project 的 Runtime")
		return StateObservation{}, nil
	})
	if got := service.ReadRobotState(context.Background(), request.ProjectID, request.RobotID); got.Status != "unavailable" || got.Reason != "robot_outside_project" {
		t.Fatalf("跨项目必须拒绝: %+v", got)
	}
}
