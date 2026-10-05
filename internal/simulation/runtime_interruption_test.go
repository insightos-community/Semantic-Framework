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

func TestSwitchVariantRejectsLoadingBeforeCheckpoint(t *testing.T) {
	service, client, store, instance := newInterruptedSnapshotFixture(t)
	state, _ := store.LoadRuntimeState("project-interrupted")
	state.CatalogSceneID, state.SceneVersion = "catalog-scene", "1.0.0"
	if err := store.SaveRuntimeState(state); err != nil {
		t.Fatal(err)
	}
	client.instance.State = "starting"
	client.calls = nil
	_, err := service.SwitchCatalogVariant(context.Background(), "project-interrupted", instance.InstanceID, "init-1", "switch", 0)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("加载中应明确拒绝切换: %v", err)
	}
	if len(client.calls) != 0 {
		t.Fatalf("加载中不应执行停止/重置: %v", client.calls)
	}
}

func TestSnapshotReadsEngineDataOnlyWhenSceneReady(t *testing.T) {
	for _, status := range []string{"starting", "resetting", "stopping", "failed", "running", "paused"} {
		t.Run(status, func(t *testing.T) {
			service, client, store, _ := newInterruptedSnapshotFixture(t)
			client.instance.State = status
			client.robotReads, client.evaluationReads = 0, 0
			snapshot, err := service.Snapshot(context.Background(), "project-interrupted")
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Instance == nil || snapshot.Instance.State != status {
				t.Fatalf("生命周期状态未及时返回: %+v", snapshot)
			}
			ready := status == "running" || status == "paused"
			if ready && client.robotReads != 1 || !ready && (client.robotReads != 0 || client.evaluationReads != 0) {
				t.Fatalf("%s 阶段引擎读取次数不正确: robot=%d evaluation=%d", status, client.robotReads, client.evaluationReads)
			}
			persisted, _ := store.LoadRuntimeState("project-interrupted")
			if persisted.LastInstance.State != status {
				t.Fatal("跳过引擎数据时仍须保存最新生命周期状态")
			}
		})
	}
}

func newInterruptedSnapshotFixture(t *testing.T) (*Service, *fakeRuntimeClient, *memoryRuntimeStateStore, SceneInstance) {
	t.Helper()
	client := &fakeRuntimeClient{
		healthy: true,
		info: RuntimeInfo{
			State: "ready", Engine: "mujoco", APIVersion: "v1",
		},
	}
	store := &memoryRuntimeStateStore{values: make(map[string]ProjectRuntimeState)}
	service := NewService(client, nil, store)
	instance, err := service.StartScene(
		context.Background(), "project-interrupted", "scene",
		SceneStartRequest{RequestID: "request-interrupted"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return service, client, store, instance
}

func TestSnapshotKeepsLastStateWhileRuntimeOfflineAndClearsDerivedRecovery(t *testing.T) {
	service, client, store, instance := newInterruptedSnapshotFixture(t)
	client.healthy = false

	snapshot, err := service.Snapshot(context.Background(), "project-interrupted")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Instance == nil || snapshot.Instance.InstanceID != instance.InstanceID ||
		snapshot.Instance.State != "running" || snapshot.Instance.FailureReason != "" {
		t.Fatalf("离线快照改写了 Runtime 最后真实状态: %+v", snapshot.Instance)
	}
	if snapshot.Recovery != "runtime_offline" || snapshot.RecoveryInfo == nil ||
		snapshot.RecoveryInfo.Code != "runtime_offline" ||
		snapshot.RecoveryInfo.DerivedState != "interrupted" ||
		snapshot.RecoveryInfo.LastKnownState != "running" ||
		snapshot.RecoveryInfo.Message == "" {
		t.Fatalf("离线快照缺少明确派生诊断: %+v", snapshot)
	}
	persisted, err := store.LoadRuntimeState("project-interrupted")
	if err != nil || persisted.LastInstance == nil ||
		persisted.LastInstance.State != "running" || persisted.LastInstance.FailureReason != "" {
		t.Fatalf("派生恢复状态不应污染持久化实例: state=%+v err=%v", persisted, err)
	}

	client.healthy = true
	client.info.State = "ready"
	recovered, err := service.Snapshot(context.Background(), "project-interrupted")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Recovery != "" || recovered.RecoveryInfo != nil ||
		recovered.Instance == nil || recovered.Instance.State != "running" {
		t.Fatalf("Runtime 恢复后新快照没有清除派生中断状态: %+v", recovered)
	}
}

func TestSnapshotKeepsFailedInstanceReachableUntilExplicitStop(t *testing.T) {
	service, client, _, instance := newInterruptedSnapshotFixture(t)
	client.instance.State = "failed"
	client.instance.FailureReason = "physics diverged"
	client.info.State = "failed"

	snapshot, err := service.Snapshot(context.Background(), "project-interrupted")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Recovery != "" || snapshot.RecoveryInfo != nil ||
		snapshot.Instance == nil || snapshot.Instance.State != "failed" {
		t.Fatalf("可连接的 failed 实例不应被派生为 interrupted: %+v", snapshot)
	}

	stopped, err := service.SceneOperation(
		context.Background(), "project-interrupted", instance.InstanceID, "stop", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != "stopped" {
		t.Fatalf("failed 实例未被显式 stop 清理: %+v", stopped)
	}
	after, err := service.Snapshot(context.Background(), "project-interrupted")
	if err != nil {
		t.Fatal(err)
	}
	if after.Instance != nil {
		t.Fatalf("stop 后不应恢复历史 failed/stopped 实例: %+v", after.Instance)
	}
}

func TestSnapshotDerivesInterruptedWhenRuntimeReportsUnknown(t *testing.T) {
	service, client, _, _ := newInterruptedSnapshotFixture(t)
	client.info.State = "unknown"

	snapshot, err := service.Snapshot(context.Background(), "project-interrupted")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Recovery != "runtime_unknown" || snapshot.RecoveryInfo == nil ||
		snapshot.RecoveryInfo.DerivedState != "interrupted" ||
		snapshot.Instance == nil || snapshot.Instance.State != "running" {
		t.Fatalf("unknown Runtime 没有保留最后实例并派生 interrupted: %+v", snapshot)
	}
}
