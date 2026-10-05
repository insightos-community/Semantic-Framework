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
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestHTTPRuntimeClientWithRealPlugin 验证 Framework 没有依赖测试夹具中的宽松字段。
// 默认测试不要求本机启动 MuJoCo；组合 Pipeline 必须设置 PLUGIN_MUJOCO_URL 后执行。
func TestHTTPRuntimeClientWithRealPlugin(t *testing.T) {
	endpoint := os.Getenv("PLUGIN_MUJOCO_URL")
	if endpoint == "" {
		t.Skip("PLUGIN_MUJOCO_URL 未设置")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	client := NewHTTPRuntimeClient(endpoint, nil)
	if err := client.Health(ctx); err != nil {
		t.Fatalf("真实 Plugin 健康检查失败: %v", err)
	}
	info, err := client.Runtime(ctx)
	if err != nil || info.RuntimeProfileID != "native-mujoco" {
		t.Fatalf("Runtime 信息不正确: info=%+v err=%v", info, err)
	}
	scenes, err := client.ListScenes(ctx)
	if err != nil || len(scenes) == 0 {
		t.Fatalf("没有读取到真实场景目录: scenes=%+v err=%v", scenes, err)
	}
	instance, err := client.StartScene(ctx, "palletizing_depalletizing_001", SceneStartRequest{
		RequestID: "framework-e2e-" + uuid.NewString(), RuntimeProfileID: "native-mujoco",
		Layout: "layout001", Seed: 7, Headless: true, RenderBackend: "egl",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_, _ = client.SceneOperation(stopCtx, instance.InstanceID, "stop", nil)
	}()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		instance, err = client.Scene(ctx, instance.InstanceID)
		if err != nil {
			t.Fatal(err)
		}
		if instance.State == "running" || instance.State == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if instance.State != "running" {
		t.Fatalf("真实场景没有进入 running: %+v", instance)
	}
	robots, err := client.Robots(ctx, instance.InstanceID)
	if err != nil || len(robots) == 0 || len(robots[0].JointNames) != 18 {
		t.Fatalf("虚拟 Robot 描述不正确: robots=%+v err=%v", robots, err)
	}
	state, err := client.RobotState(ctx, robots[0].RobotID)
	if err != nil || state.Generation != instance.Generation || state.BasePose == nil ||
		state.BasePose.FrameID != "world" || len(state.Joints) == 0 {
		t.Fatalf("Robot 状态没有按公共模型解析: state=%+v err=%v", state, err)
	}
	sensors, err := client.RobotSensors(ctx, robots[0].RobotID)
	if err != nil || len(sensors) == 0 || sensors[0].FrameID == "" {
		t.Fatalf("传感器描述不正确: sensors=%+v err=%v", sensors, err)
	}
	hold, err := client.HoldRobot(ctx, robots[0].RobotID, instance.Generation)
	if err != nil || hold.Status != RobotCommandSucceeded || hold.Generation != instance.Generation {
		t.Fatalf("hold 没有得到明确终态: command=%+v err=%v", hold, err)
	}
}
