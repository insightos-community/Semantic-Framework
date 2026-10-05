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
	"fmt"
	"time"
)

// RobotLifecycleCoordinator 是 SimulationService 与受管 Robot 实例之间唯一的
// 生命周期边界。它只管理 Pilot、Ability 和 AbilityFramework，不拥有 MuJoCo
// Runtime；场景本身仍由 SimulationService 按 v0.4 生命周期启动、reset 和停止。
type RobotLifecycleCoordinator interface {
	StartSceneRobots(context.Context, string, SceneInstance, []VirtualRobotDescriptor) error
	HoldSceneRobots(context.Context, string, string, string) error
	StopSceneRobots(context.Context, string, string, string) error
}

func (s *Service) ConfigureRobotLifecycle(coordinator RobotLifecycleCoordinator) {
	s.robotLifecycle = coordinator
}

// startSceneRobots 在场景已 running、首个 Semantic Map generation 已经写入后
// 才调用。这样 Pilot 首次注册时看到的 scene/map 身份是稳定的，不会把 starting
// 场景中的空目录误报为 ready。Robot 启动失败只会形成 degraded/failed 设备，
// 不反向销毁仍可查看的场景。
func (s *Service) startSceneRobots(
	ctx context.Context, projectID string, instance SceneInstance,
) error {
	if s.robotLifecycle == nil {
		return nil
	}
	robots, err := s.Robots(ctx, projectID, instance.InstanceID)
	if err != nil {
		return fmt.Errorf("读取场景 Robot 描述: %w", err)
	}
	for index := range robots {
		robots[index].SceneInstanceID = instance.InstanceID
		if robots[index].Backend == "" {
			robots[index].Backend = "mujoco"
		}
	}
	return s.robotLifecycle.StartSceneRobots(ctx, projectID, instance, robots)
}

func (s *Service) startSceneRobotsAsync(projectID string, instance SceneInstance) {
	go func() {
		// Launcher 内部最多等待两分钟完成 AF、Ability、Pilot 与 Skill 对账。
		// 外层生命周期必须额外留出安全停止和失败持久化时间；两个 deadline
		// 完全相同时，readiness 超时会把 cleanup 一并取消，留下长期 starting
		// 的实例和孤儿进程。
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		_ = s.startSceneRobots(ctx, projectID, instance)
	}()
}

// holdSceneRobots 只让 Robot 的活动工作停止，并通过 Runtime 的同步 hold 端点
// 固定底盘、双臂和工具；它不会退出 Pilot、Ability 或 AbilityFramework。
//
// 这里先取得 Runtime 对真实物理状态的 hold 确认，再让 Pilot 收敛 Worker 和
// Execution。原来的顺序会在 Pilot 已断线时提前返回，导致 MuJoCo 明明提供了
// 可验证的 hold 能力却根本没有被调用。RobotLifecycleCoordinator 被调用时可以信任
// Runtime hold 已经成功；这个边界只用于 Framework 管理的仿真场景，不改变真机
// 必须由 Pilot/SDK 提供停止证据的规则。
func (s *Service) holdSceneRobots(
	ctx context.Context, projectID, instanceID string, generation int64,
	client RuntimeClient, reason string,
) error {
	robots, err := client.Robots(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("读取场景 Robot: %w", err)
	}
	for _, robot := range robots {
		result, holdErr := client.HoldRobot(ctx, robot.RobotID, generation)
		if holdErr != nil {
			return fmt.Errorf("Robot %s 进入 hold: %w", robot.RobotID, holdErr)
		}
		if result.Status != RobotCommandSucceeded {
			return fmt.Errorf("Robot %s 未确认 hold，状态 %s", robot.RobotID, result.Status)
		}
	}
	if s.robotLifecycle != nil {
		if err := s.robotLifecycle.HoldSceneRobots(ctx, projectID, instanceID, reason); err != nil {
			return fmt.Errorf("收敛场景 Robot 活动工作: %w", err)
		}
	}
	return nil
}

// stopSceneRobots 必须在 Runtime stop 前完成。Robot 侧未确认 hold 时，
// 场景继续保留原物理状态并返回错误；这比先 reset 后再把未知动作标成 stopped
// 更安全，也让真机和仿真共用同一套 Pilot 安全停止证据。
func (s *Service) stopSceneRobots(
	ctx context.Context, projectID, instanceID, reason string,
) error {
	if s.robotLifecycle == nil {
		return nil
	}
	if err := s.robotLifecycle.StopSceneRobots(ctx, projectID, instanceID, reason); err != nil {
		return fmt.Errorf("安全停止场景 Robot: %w", err)
	}
	return nil
}
