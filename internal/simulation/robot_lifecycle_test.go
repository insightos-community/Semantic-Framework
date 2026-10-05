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
	"testing"
)

type recordingRobotLifecycle struct {
	client      *fakeRuntimeClient
	started     []VirtualRobotDescriptor
	holds       []string
	stops       []string
	holdFailure error
	stopFailure error
}

func (r *recordingRobotLifecycle) StartSceneRobots(
	_ context.Context,
	_ string,
	_ SceneInstance,
	robots []VirtualRobotDescriptor,
) error {
	r.started = append(r.started, robots...)
	return nil
}

func (r *recordingRobotLifecycle) HoldSceneRobots(
	_ context.Context,
	_ string,
	_ string,
	reason string,
) error {
	for _, call := range r.client.calls {
		if call == "reset" || call == "stop" {
			return errors.New("Robot hold 在 Runtime 操作之后才执行")
		}
	}
	r.holds = append(r.holds, reason)
	return r.holdFailure
}

func (r *recordingRobotLifecycle) StopSceneRobots(
	_ context.Context,
	_ string,
	_ string,
	reason string,
) error {
	for _, call := range r.client.calls {
		if call == "reset" || call == "stop" {
			return errors.New("Robot 生命周期在 Runtime 操作之后才执行")
		}
	}
	r.stops = append(r.stops, reason)
	return r.stopFailure
}

func TestSceneRobotLifecycleUsesRuntimeEndpointAndStopsBeforeRuntime(t *testing.T) {
	client := &fakeRuntimeClient{
		healthy: true,
		info: RuntimeInfo{
			State: "ready", Engine: "mujoco",
			Endpoint: "http://127.0.0.1:8090",
		},
	}
	state := &memoryRuntimeStateStore{values: make(map[string]ProjectRuntimeState)}
	service := NewService(client, nil, state)
	lifecycle := &recordingRobotLifecycle{client: client}
	service.ConfigureRobotLifecycle(lifecycle)

	instance, err := service.StartScene(
		context.Background(), "project-1", "depalletizing",
		SceneStartRequest{RequestID: "request-1", Layout: "layout001"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.startSceneRobots(context.Background(), "project-1", instance); err != nil {
		t.Fatal(err)
	}
	if len(lifecycle.started) != 1 {
		t.Fatalf("未启动场景 Robot: %+v", lifecycle.started)
	}
	robot := lifecycle.started[0]
	if robot.SceneInstanceID != instance.InstanceID ||
		robot.Endpoint != "http://127.0.0.1:8090" ||
		robot.Backend != "mujoco" {
		t.Fatalf("受管 Robot 描述没有补齐 Scene/Endpoint/Backend: %+v", robot)
	}

	if _, err := service.SceneOperation(
		context.Background(), "project-1", instance.InstanceID, "reset", nil,
	); err != nil {
		t.Fatal(err)
	}
	if len(lifecycle.holds) != 1 || lifecycle.holds[0] != "scene_reset" {
		t.Fatalf("reset 前没有让 Robot 进入 hold: %+v", lifecycle.holds)
	}
	if len(lifecycle.stops) != 0 {
		t.Fatalf("reset 不应退出受管 Robot 实例: %+v", lifecycle.stops)
	}
	if len(lifecycle.started) != 1 {
		t.Fatalf("reset 不应重启 Pilot/AbilityFramework: %+v", lifecycle.started)
	}
	if len(client.calls) != 2 || client.calls[0] != "hold" || client.calls[1] != "reset" {
		t.Fatalf("Runtime reset 调用顺序错误: %+v", client.calls)
	}
}

func TestSceneOperationKeepsRuntimeWhenRobotStopIsUnconfirmed(t *testing.T) {
	client := &fakeRuntimeClient{
		healthy: true,
		info:    RuntimeInfo{State: "ready", Engine: "mujoco"},
	}
	service := NewService(
		client, nil,
		&memoryRuntimeStateStore{values: make(map[string]ProjectRuntimeState)},
	)
	lifecycle := &recordingRobotLifecycle{
		client: client, stopFailure: errors.New("hold 未确认"),
	}
	service.ConfigureRobotLifecycle(lifecycle)
	instance, err := service.StartScene(
		context.Background(), "project-1", "depalletizing",
		SceneStartRequest{RequestID: "request-1", Layout: "layout001"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SceneOperation(
		context.Background(), "project-1", instance.InstanceID, "stop", nil,
	); err == nil {
		t.Fatal("Robot hold 未确认时不应停止场景 Runtime")
	}
	if len(client.calls) != 1 || client.calls[0] != "hold" {
		t.Fatalf("实例停止失败时不应继续停止 Scene Runtime: %+v", client.calls)
	}
}
