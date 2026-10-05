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
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHTTPRuntimeClientDecodesCanonicalRobotState(t *testing.T) {
	client := NewHTTPRuntimeClient("http://runtime.example", &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != "/api/v1/robots/r1/state" {
				t.Fatalf("请求路径错误: %s", request.URL.Path)
			}
			body := `{"robot_id":"r1","generation":3,"observed_at":"2026-08-09T00:00:00Z",` +
				`"base_pose":{"position":[1,2,0],"quaternion_xyzw":[0,0,0,1],"frame_id":"world"},` +
				`"joints":{"joint-1":{"position":0.2,"velocity":0.1,"effort":1.5}},` +
				`"end_effectors":{},"grippers":{"left":0.04},"in_hold":true}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		}),
	})
	state, err := client.RobotState(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if state.Generation != 3 || state.BasePose == nil ||
		state.BasePose.QuaternionXYZW != [4]float64{0, 0, 0, 1} ||
		state.Joints["joint-1"].Velocity != 0.1 || !state.InHold {
		t.Fatalf("Robot 状态没有按公共模型解码: %+v", state)
	}
}

func TestRuntimeRegistryRequiresExplicitProfileWhenMultipleProfilesExist(t *testing.T) {
	client := &fakeRuntimeClient{healthy: true}
	registry, err := NewRuntimeRegistry(
		RuntimeBinding{Profile: RuntimeProfile{
			RuntimeProfileID: "native-mujoco", SceneKinds: []string{"scene_document"},
		}, Client: client},
		RuntimeBinding{Profile: RuntimeProfile{
			RuntimeProfileID: "robosuite-1.5", SceneKinds: []string{"robosuite"},
		}, Client: client},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.DefaultProfileID(); err == nil {
		t.Fatal("多 Runtime profile 时不应静默选择默认环境")
	}
	if registry.Compatible("robosuite-1.5", "scene_document") {
		t.Fatal("可编辑 SceneDocument 不应交给 robosuite 原生环境")
	}
}

func TestSceneOperationsAreAtomicAndRejectHierarchyCycles(t *testing.T) {
	authoring := NewSceneAuthoringService(NewFileStore(fixedWorkspace{root: t.TempDir()}))
	document, err := authoring.Create("project-1", "操作测试")
	if err != nil {
		t.Fatal(err)
	}
	validTransform := Transform{
		QuaternionXYZW: [4]float64{0, 0, 0, 1},
		Scale:          [3]float64{1, 1, 1},
	}
	document, err = authoring.ApplyOperations(
		"project-1", document.ID, document.Revision,
		[]SceneOperation{
			{Type: SceneOperationCreateNode, Node: &SceneNode{
				ID: "root", Name: "Root", Kind: "group", Transform: validTransform,
			}},
			{Type: SceneOperationAddRobot, Node: &SceneNode{
				ID: "robot-1", ParentID: "root", Name: "R1 Pro", Transform: validTransform,
			}},
		},
	)
	if err != nil {
		t.Fatalf("合法操作失败: %v", err)
	}
	beforeRevision := document.Revision
	_, err = authoring.ApplyOperations(
		"project-1", document.ID, document.Revision,
		[]SceneOperation{{Type: SceneOperationMoveNode, NodeID: "root", ParentID: "robot-1"}},
	)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("循环层级应被拒绝，实际: %v", err)
	}
	stored, err := authoring.Get("project-1", document.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Revision != beforeRevision || stored.Nodes[0].ParentID != "" {
		t.Fatalf("失败操作不应保存半成品: %+v", stored)
	}
}

func TestRobotDebugCommandChecksCapabilityAndGeneration(t *testing.T) {
	client := &fakeRuntimeClient{
		healthy: true, info: RuntimeInfo{State: "ready", Engine: "mujoco"},
	}
	service := NewService(client, nil,
		&memoryRuntimeStateStore{values: make(map[string]ProjectRuntimeState)})
	instance, err := service.StartScene(context.Background(), "project-1", "scene",
		SceneStartRequest{RequestID: "request-1"})
	if err != nil {
		t.Fatal(err)
	}
	command := RobotDebugCommand{
		CommandID: "command-1", SceneGeneration: instance.Generation,
		Type: RobotCommandJointTrajectory,
		Joint: &JointTrajectory{
			Resources: []string{"left_arm"}, FrameID: "base",
			Points: []TrajectoryPoint{{
				TimeFromStartSeconds: 1,
				Positions:            map[string]float64{"joint-1": 0.2},
			}},
		},
	}
	record, err := service.SubmitRobotCommand(
		context.Background(), "project-1", instance.InstanceID, "r1", command,
	)
	if err != nil || record.Status != RobotCommandAccepted {
		t.Fatalf("合法低层轨迹命令失败: record=%+v err=%v", record, err)
	}
	command.SceneGeneration--
	if _, err := service.SubmitRobotCommand(
		context.Background(), "project-1", instance.InstanceID, "r1", command,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("旧 generation 命令应被拒绝，实际: %v", err)
	}
	command.SceneGeneration = instance.Generation
	command.Type = RobotCommandStop
	if _, err := service.SubmitRobotCommand(
		context.Background(), "project-1", instance.InstanceID, "r1", command,
	); err == nil {
		t.Fatal("Stop 必须使用明确 stop 端点，不能混入 commands")
	}
}

func TestSnapshotDetectsRuntimeInstanceMismatchWithoutReplay(t *testing.T) {
	client := &fakeRuntimeClient{
		healthy: true, info: RuntimeInfo{State: "ready", Engine: "mujoco"},
	}
	service := NewService(client, nil,
		&memoryRuntimeStateStore{values: make(map[string]ProjectRuntimeState)})
	instance, err := service.StartScene(context.Background(), "project-1", "scene",
		SceneStartRequest{RequestID: "request-1"})
	if err != nil {
		t.Fatal(err)
	}
	client.info.ActiveInstanceID = "another-instance"
	snapshot, err := service.Snapshot(context.Background(), "project-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Recovery != "instance_mismatch" || snapshot.Instance == nil ||
		snapshot.Instance.State != "running" || snapshot.Instance.InstanceID != instance.InstanceID ||
		snapshot.RecoveryInfo == nil || snapshot.RecoveryInfo.DerivedState != "interrupted" ||
		snapshot.RecoveryInfo.LastKnownState != "running" ||
		snapshot.Instance.FailureReason != "" {
		t.Fatalf("Runtime 不一致没有形成明确恢复状态: %+v", snapshot)
	}
}

type recordingMapSink struct {
	projectID string
	snapshot  SceneSnapshot
}

func (s *recordingMapSink) ApplySimulationSnapshot(
	_ context.Context, projectID string, snapshot SceneSnapshot,
) error {
	s.projectID, s.snapshot = projectID, snapshot
	return nil
}

func TestSceneSnapshotIsReadOnlyAndExplicitSyncChecksGeneration(t *testing.T) {
	client := &fakeRuntimeClient{
		healthy: true, info: RuntimeInfo{State: "ready", Engine: "mujoco"},
	}
	store := &memoryRuntimeStateStore{values: make(map[string]ProjectRuntimeState)}
	registry, _ := NewRuntimeRegistry(RuntimeBinding{
		Profile: RuntimeProfile{
			RuntimeProfileID: "native-mujoco", SceneKinds: []string{"scene_document"},
		},
		Client: client,
	})
	sink := &recordingMapSink{}
	service := NewServiceWithRegistry(registry, store, sink)
	instance, err := service.StartScene(context.Background(), "project-1", "scene",
		SceneStartRequest{RequestID: "request-1", RuntimeProfileID: "native-mujoco"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SceneSnapshot(
		context.Background(), "project-1", instance.InstanceID,
	); err != nil {
		t.Fatal(err)
	}
	if sink.projectID != "" {
		t.Fatalf("读取场景快照不应隐式更新 Map: %+v", sink)
	}
	if _, err := service.SyncSceneMap(
		context.Background(), "project-1", instance.InstanceID,
	); err != nil {
		t.Fatal(err)
	}
	if sink.projectID != "project-1" || sink.snapshot.Generation != instance.Generation {
		t.Fatalf("显式同步没有把当前 generation 送入 Map 接口: %+v", sink)
	}
	client.instance.Generation++
	if _, err := service.SyncSceneMap(
		context.Background(), "project-1", instance.InstanceID,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("错误 generation 的显式同步不应进入 Map: %v", err)
	}
}

func TestNativeEvaluationIsGenerationCheckedAndRestored(t *testing.T) {
	client := &fakeRuntimeClient{
		healthy: true, info: RuntimeInfo{State: "ready", Engine: "mujoco"},
	}
	registry, err := NewRuntimeRegistry(RuntimeBinding{
		Profile: RuntimeProfile{
			RuntimeProfileID: "robosuite-1.5", SceneKinds: []string{"robosuite"},
			Capabilities: RuntimeCapability{NativeEvaluator: true},
		},
		Client: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewServiceWithRegistry(
		registry, &memoryRuntimeStateStore{values: make(map[string]ProjectRuntimeState)}, nil,
	)
	instance, err := service.StartScene(context.Background(), "project-1", "Lift",
		SceneStartRequest{RequestID: "evaluation-start", RuntimeProfileID: "robosuite-1.5"})
	if err != nil {
		t.Fatal(err)
	}
	evaluation, err := service.SceneEvaluation(
		context.Background(), "project-1", instance.InstanceID,
	)
	if err != nil || !evaluation.Success || evaluation.Reward != 1.25 {
		t.Fatalf("读取原生评测失败: evaluation=%+v err=%v", evaluation, err)
	}
	snapshot, err := service.Snapshot(context.Background(), "project-1")
	if err != nil || snapshot.Evaluation == nil || !snapshot.Evaluation.Success {
		t.Fatalf("Studio 快照没有恢复评测证据: snapshot=%+v err=%v", snapshot, err)
	}
	client.instance.Generation++
	if _, err := service.SceneEvaluation(
		context.Background(), "project-1", instance.InstanceID,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("旧 generation 的评测结果应被拒绝: %v", err)
	}
}

type stubbornLauncher struct {
	process *fakeProcess
	starts  int
}

func (l *stubbornLauncher) Start(context.Context) (RuntimeProcess, error) {
	l.starts++
	l.process.alive = true
	return l.process, nil
}

func TestRuntimeSupervisorCleansProcessWhenStartupIsCancelled(t *testing.T) {
	process := &fakeProcess{}
	client := &fakeRuntimeClient{healthy: false}
	launcher := &stubbornLauncher{process: process}
	registry, err := NewRuntimeRegistry(RuntimeBinding{
		Profile: RuntimeProfile{RuntimeProfileID: "native-mujoco"},
		Client:  client, Launcher: launcher,
	})
	if err != nil {
		t.Fatal(err)
	}
	supervisor := NewRuntimeSupervisor(registry)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := supervisor.Ensure(ctx, "native-mujoco"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("启动取消应返回 context deadline，实际: %v", err)
	}
	if launcher.starts != 1 || !process.stopped || process.alive {
		t.Fatalf("未通过健康检查的本次启动进程没有清理: %+v", process)
	}
	slot := supervisor.slots["native-mujoco"]
	if slot.process != nil {
		t.Fatal("清理后 Supervisor 仍保留失败进程")
	}
}

func TestStopAndHoldKeepExplicitTerminalState(t *testing.T) {
	client := &fakeRuntimeClient{
		healthy: true, info: RuntimeInfo{State: "ready", Engine: "mujoco"},
	}
	service := NewService(client, nil,
		&memoryRuntimeStateStore{values: make(map[string]ProjectRuntimeState)})
	instance, err := service.StartScene(context.Background(), "project-1", "scene",
		SceneStartRequest{RequestID: "request-stop"})
	if err != nil {
		t.Fatal(err)
	}

	hold, err := service.HoldRobot(
		context.Background(), "project-1", instance.InstanceID, "r1", instance.Generation,
	)
	if err != nil || hold.Status != RobotCommandSucceeded || hold.Type != RobotCommandHold {
		t.Fatalf("hold 没有返回明确终态: record=%+v err=%v", hold, err)
	}
	stoppedCommand, err := service.StopRobotCommand(
		context.Background(), "project-1", instance.InstanceID, "r1", "command-1",
	)
	if err != nil || stoppedCommand.Status != RobotCommandCancelled {
		t.Fatalf("停止命令没有返回 cancelled: record=%+v err=%v", stoppedCommand, err)
	}
	stoppedScene, err := service.SceneOperation(
		context.Background(), "project-1", instance.InstanceID, "stop", nil,
	)
	if err != nil || stoppedScene.State != "stopped" {
		t.Fatalf("停止场景失败: instance=%+v err=%v", stoppedScene, err)
	}
	snapshot, err := service.Snapshot(context.Background(), "project-1")
	if err != nil || snapshot.Instance != nil {
		t.Fatalf("停止后不应把终态实例恢复为活动实例: snapshot=%+v err=%v", snapshot, err)
	}
}

func TestSceneBuildServicePersistsAcceptedBundleBeforePublish(t *testing.T) {
	fileStore := NewFileStore(fixedWorkspace{root: t.TempDir()})
	authoring := NewSceneAuthoringService(fileStore)
	document, err := authoring.Create("project-1", "可构建场景")
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeRuntimeClient{
		healthy: true, info: RuntimeInfo{State: "ready", Engine: "mujoco"},
	}
	service := NewService(client, nil, fileStore)
	builder := NewSceneBuildService(authoring, service)
	bundle, result, err := builder.Build(
		context.Background(), "project-1", document.ID, "native-mujoco",
	)
	if err != nil || !result.Valid {
		t.Fatalf("RuntimeBundle 构建失败: bundle=%+v result=%+v err=%v", bundle, result, err)
	}
	stored, err := fileStore.LoadRuntimeBundle("project-1", bundle.RuntimeBundleID)
	if err != nil || stored.DocumentID != document.ID {
		t.Fatalf("成功构建没有持久化: stored=%+v err=%v", stored, err)
	}
	published, err := authoring.Publish("project-1", document.ID, document.Revision)
	if err != nil || published.Version != bundle.SceneVersion {
		t.Fatalf("成功构建后无法发布: document=%+v err=%v", published, err)
	}
}
