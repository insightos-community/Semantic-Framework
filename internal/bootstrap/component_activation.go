package bootstrap

import (
	"context"
	"fmt"

	"insightos.cn/semantic-framework/internal/robotruntime"
	"insightos.cn/semantic-framework/internal/simulation"
)

// 显式生效只重启目标 Robot 的 supervisor，不停止、重置或重新创建 Scene。
// 停止仍由既有 Pilot/Ability/SDK 链路确认 hold；失败时保留现有对账入口。
func (l *managedSceneRobotLifecycle) applyInstalledComponents(ctx context.Context, projectID, robotID string, sim *simulation.Service) error {
	instance, err := l.store.GetLatestRuntimeByRobot(ctx, robotID)
	if err != nil {
		return err
	}
	if instance.ProjectID != projectID || instance.Backend == "real" {
		return fmt.Errorf("请选择当前项目的受管仿真 Robot")
	}
	if instance.Status.Active() {
		if err := l.store.CheckRobotAdmission(robotID, "", ""); err != nil {
			return fmt.Errorf("Robot 仍有任务占用: %w", err)
		}
		device, err := l.robots.Device(robotID)
		if err != nil {
			return err
		}
		if device["status"] != "idle" {
			return fmt.Errorf("Robot 尚未空闲，请结束当前执行后生效")
		}
	}
	descriptors, err := sim.Robots(ctx, projectID, instance.SceneInstanceID)
	if err != nil {
		return err
	}
	var descriptor *simulation.VirtualRobotDescriptor
	for _, item := range descriptors {
		if item.RobotID == robotID {
			copy := item
			descriptor = &copy
			break
		}
	}
	if descriptor == nil {
		return fmt.Errorf("当前场景未找到 Robot")
	}
	if instance.Status.Active() {
		if _, err := l.orchestrator.Stop(ctx, instance.InstanceID, "安装组件后显式生效"); err != nil {
			return err
		}
	}
	_, err = l.orchestrator.Start(ctx, robotruntime.StartRequest{Descriptor: *descriptor, ProjectID: projectID, PilotInstanceID: instance.PilotInstanceID})
	return err
}
