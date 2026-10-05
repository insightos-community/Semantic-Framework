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
	"context"
	"fmt"

	robotdomain "insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/simulation"
	"insightos.cn/semantic-framework/internal/store"
)

type executionObserverChain []robotdomain.ExecutionObserver

func (observers executionObserverChain) OnRobotExecutionChanged(
	ctx context.Context,
	execution store.RobotExecution,
	eventType string,
	payload map[string]any,
) error {
	for _, observer := range observers {
		if observer == nil {
			continue
		}
		if err := observer.OnRobotExecutionChanged(ctx, execution, eventType, payload); err != nil {
			return err
		}
	}
	return nil
}

type robotMapCheckpointObserver struct {
	store      *store.Store
	simulation *simulation.Service
}

func (o robotMapCheckpointObserver) OnRobotExecutionChanged(
	ctx context.Context,
	execution store.RobotExecution,
	_ string,
	_ map[string]any,
) error {
	if execution.Status != "completed" ||
		(execution.SkillName != "grasp-object" && execution.SkillName != "place-object") {
		return nil
	}
	if execution.SubtaskID == "" {
		return nil
	}
	pilot, err := o.store.GetActiveRobotPilot(execution.RobotID)
	if err != nil {
		return err
	}
	if pilot.Backend != "mujoco" {
		// Fake 与真机没有 SceneSnapshot。它们继续由各自的 Observation/
		// Artifact 更新业务结果，不能因为不存在仿真 Runtime 而阻塞 Skill
		// 终态收敛。
		return nil
	}
	subTask, err := o.store.GetSubTask(execution.SubtaskID)
	if err != nil {
		return err
	}
	if subTask.Status == store.TaskStatusCompleted {
		// Pilot 重连可能重送同一终态。Workflow 已经完成该 SubTask 时说明
		// 对应 checkpoint 已经先于状态推进成功，不能再次增加 Map revision。
		return nil
	}
	instance, err := o.store.GetLatestRuntimeByRobot(ctx, execution.RobotID)
	if err != nil {
		return fmt.Errorf("查找 Robot 对应场景实例: %w", err)
	}
	if instance.ProjectID != "" && instance.ProjectID != execution.ProjectID {
		return fmt.Errorf("Robot Runtime 与 Execution Project 不一致")
	}
	if instance.SceneInstanceID == "" {
		return fmt.Errorf("Robot Runtime 缺少 scene_instance_id")
	}
	// grasp-object/place-object 进入 completed 的前提是 Skill 已经完成 stable
	// load 或 stable placement 的独立验证。因此这里读取一次真实 SceneSnapshot
	// 并更新地图；不能在 release/command accepted 时提前写入语义状态。
	reason := "robot_grasp_completed"
	if execution.SkillName == "place-object" {
		reason = "robot_place_completed"
	}
	return o.simulation.SyncSceneMapCheckpoint(
		ctx, execution.ProjectID, instance.SceneInstanceID, reason,
	)
}

var _ robotdomain.ExecutionObserver = executionObserverChain{}
var _ robotdomain.ExecutionObserver = robotMapCheckpointObserver{}
