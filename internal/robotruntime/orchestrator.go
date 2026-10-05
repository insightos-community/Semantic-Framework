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

package robotruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type OrchestratorConfig struct {
	Catalog   *Catalog
	Store     Store
	Ports     PortLeaser
	Launcher  InstanceLauncher
	Events    EventSink
	DataRoot  string
	PortFirst int
	PortLast  int
	Now       func() time.Time
	NewID     func() string
}

// Orchestrator 由 Server 管理多台 Robot Runtime Instance。所有状态先持久化再
// 触发外部动作；重启后不会因为上层没有收到 ack 而重复启动 Robot 或进程。
type Orchestrator struct {
	catalog    *Catalog
	store      Store
	ports      PortLeaser
	launcher   InstanceLauncher
	events     EventSink
	dataRoot   string
	first      int
	last       int
	now        func() time.Time
	newID      func() string
	locksMu    sync.Mutex
	robotLocks map[string]*sync.Mutex
}

// ReloadCatalog 只更新后续启动的 Bundle 选择；已运行实例继续持有原始版本。
func (o *Orchestrator) ReloadCatalog(root string) error {
	next, err := LoadCatalog(root)
	if err != nil {
		return err
	}
	o.catalog.mu.Lock()
	defer o.catalog.mu.Unlock()
	o.catalog.bundles = next.bundles
	return nil
}

func NewOrchestrator(config OrchestratorConfig) (*Orchestrator, error) {
	if config.Catalog == nil || config.Store == nil || config.Ports == nil || config.Launcher == nil {
		return nil, errors.New("Robot Runtime Orchestrator 缺少 Catalog、Store、PortLeaser 或 Launcher")
	}
	if strings.TrimSpace(config.DataRoot) == "" {
		return nil, errors.New("Robot Runtime data root 必填")
	}
	if config.PortFirst <= 0 {
		config.PortFirst = 18100
	}
	if config.PortLast < config.PortFirst {
		config.PortLast = config.PortFirst + 99
	}
	if config.Events == nil {
		config.Events = nopEventSink{}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.NewID == nil {
		config.NewID = func() string { return "runtime-" + uuid.NewString() }
	}
	return &Orchestrator{catalog: config.Catalog, store: config.Store, ports: config.Ports,
		launcher: config.Launcher, events: config.Events,
		dataRoot: config.DataRoot, first: config.PortFirst, last: config.PortLast,
		now: config.Now, newID: config.NewID,
		robotLocks: make(map[string]*sync.Mutex)}, nil
}

func (o *Orchestrator) Start(ctx context.Context, request StartRequest) (RuntimeInstance, error) {
	descriptor := request.Descriptor
	if strings.TrimSpace(request.PilotInstanceID) == "" || strings.TrimSpace(descriptor.RobotID) == "" {
		return RuntimeInstance{}, errors.New("启动 Runtime 需要 pilot_instance_id 和 robot_id")
	}
	lock := o.lockRobot(descriptor.RobotID)
	lock.Lock()
	defer lock.Unlock()
	if existing, err := o.store.GetActiveRuntimeByRobot(ctx, descriptor.RobotID); err == nil && existing.Status.Active() {
		return existing, fmt.Errorf("%w: %s", ErrRobotAlreadyInUse, existing.InstanceID)
	} else if err != nil && !errors.Is(err, ErrInstanceNotFound) {
		return RuntimeInstance{}, err
	}
	instanceID := strings.TrimSpace(request.InstanceID)
	if instanceID == "" {
		instanceID = o.newID()
	}
	now := o.now().UTC()
	bundle, err := o.catalog.ResolveDescriptor(descriptor)
	if err != nil {
		failed := RuntimeInstance{
			InstanceID: instanceID, PilotInstanceID: request.PilotInstanceID,
			RobotID: descriptor.RobotID, SceneInstanceID: descriptor.SceneInstanceID,
			RobotModel: descriptor.Model, Backend: descriptor.Backend, Kind: descriptor.Kind,
			BackendProfile: descriptor.BackendProfile, ProjectID: request.ProjectID,
			Status: StateFailed, FailureReason: err.Error(),
			Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
		if saveErr := o.saveAndPublish(ctx, &failed, "robot.runtime.failed"); saveErr != nil {
			return failed, errors.Join(err, saveErr)
		}
		return failed, err
	}
	// 同一仿真 Robot ID 可能在场景切换后再次出现，但 scene_instance_id、
	// Runtime endpoint 和工具描述都可能已经变化。运行数据按 Robot 归档、按
	// Runtime Instance 隔离，既保留旧执行证据，也避免 supervisor 复用旧场景
	// 的 RobotDeployment。场景 reset 仍复用同一 instance ID 和目录。
	dataDirectory := filepath.Join(
		o.dataRoot, "robots", safePathPart(descriptor.RobotID), safePathPart(instanceID),
	)
	if err := os.MkdirAll(dataDirectory, 0o750); err != nil {
		return RuntimeInstance{}, fmt.Errorf("创建 Runtime 数据目录: %w", err)
	}
	instance := RuntimeInstance{
		InstanceID: instanceID, PilotInstanceID: request.PilotInstanceID, RobotID: descriptor.RobotID,
		SceneInstanceID: descriptor.SceneInstanceID, BundleName: bundle.Name, BundleVersion: bundle.Version,
		RobotModel: descriptor.Model, Backend: descriptor.Backend, Kind: descriptor.Kind,
		BackendProfile: descriptor.BackendProfile, Status: StateStarting,
		DataDirectory: dataDirectory, ProjectID: request.ProjectID,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := o.saveAndPublish(ctx, &instance, "robot.runtime.starting"); err != nil {
		return RuntimeInstance{}, err
	}
	port, err := o.ports.AcquireAbilityFrameworkPort(ctx, instance.InstanceID, o.first, o.last)
	if err != nil {
		return o.failStart(ctx, instance, err, false)
	}
	instance.AbilityFrameworkPort = port
	o.bump(&instance)
	if err := o.store.SaveRuntimeInstance(ctx, instance); err != nil {
		return o.failStart(ctx, instance, err, false)
	}
	result, err := o.launcher.Start(ctx, LaunchRequest{Instance: instance, Bundle: bundle, Descriptor: descriptor})
	if err != nil {
		return o.failStart(ctx, instance, fmt.Errorf("启动 Ability Runtime: %w", err), true)
	}
	if strings.TrimSpace(result.AbilityFrameworkEndpoint) == "" {
		return o.failStart(ctx, instance, errors.New("Ability Runtime 就绪但没有 endpoint"), true)
	}
	instance.AbilityFrameworkEndpoint = result.AbilityFrameworkEndpoint
	instance.Status = StateReady
	instance.FailureReason = ""
	o.bump(&instance)
	if err := o.saveAndPublish(ctx, &instance, "robot.runtime.ready"); err != nil {
		// 外部进程已经 ready，此时不能因持久化失败重启。调用方必须把实例视为
		// interrupted 并由人工/对账处理。
		instance.Status = StateInterrupted
		instance.FailureReason = err.Error()
		o.bump(&instance)
		_ = o.store.SaveRuntimeInstance(ctx, instance)
		return instance, err
	}
	return instance, nil
}

// Stop 只停止当前 Robot 类型包实例。Launcher 内部按 Pilot 安全停止、Ability、
// AbilityFramework 的顺序执行；场景与仿真 Runtime 仍由 SimulationService 管理。
// 未取得 Robot hold 证据时进入 interrupted，并保留端口与数据。
func (o *Orchestrator) Stop(ctx context.Context, instanceID, reason string) (RuntimeInstance, error) {
	instance, err := o.store.GetRuntimeInstance(ctx, instanceID)
	if err != nil {
		return RuntimeInstance{}, err
	}
	lock := o.lockRobot(instance.RobotID)
	lock.Lock()
	defer lock.Unlock()
	instance, err = o.store.GetRuntimeInstance(ctx, instanceID)
	if err != nil {
		return RuntimeInstance{}, err
	}
	if instance.Status == StateStopped || instance.Status == StateFailed {
		return instance, nil
	}
	instance.Status = StateStopping
	instance.FailureReason = ""
	o.bump(&instance)
	if err := o.saveAndPublish(ctx, &instance, "robot.runtime.stopping"); err != nil {
		return instance, err
	}
	evidence, err := o.launcher.Stop(ctx, instance, reason)
	if err != nil || !evidence.Confirmed {
		return o.interruptStop(ctx, instance, "robot_instance", evidence, err)
	}
	if err := o.ports.ReleaseAbilityFrameworkPort(ctx, instance.InstanceID); err != nil {
		return o.interruptStop(ctx, instance, "port_release", StopEvidence{}, err)
	}
	instance.Status = StateStopped
	o.bump(&instance)
	if err := o.saveAndPublish(ctx, &instance, "robot.runtime.stopped"); err != nil {
		return instance, err
	}
	return instance, nil
}

func (o *Orchestrator) MarkDegraded(ctx context.Context, instanceID, reason string) (RuntimeInstance, error) {
	instance, err := o.store.GetRuntimeInstance(ctx, instanceID)
	if err != nil {
		return RuntimeInstance{}, err
	}
	lock := o.lockRobot(instance.RobotID)
	lock.Lock()
	defer lock.Unlock()
	instance, err = o.store.GetRuntimeInstance(ctx, instanceID)
	if err != nil {
		return RuntimeInstance{}, err
	}
	if instance.Status != StateReady && instance.Status != StateDegraded {
		return instance, fmt.Errorf("Runtime %s 当前状态 %s 不能标记 degraded", instanceID, instance.Status)
	}
	instance.Status = StateDegraded
	instance.FailureReason = reason
	o.bump(&instance)
	return instance, o.saveAndPublish(ctx, &instance, "robot.runtime.degraded")
}

// ReclaimInterruptedSimulation 回收已经失去 supervisor 的旧受管仿真实例。
// 它只接受两种可验证前提：新的 running Scene 已替换旧物理世界，或同一 Scene
// 的 Runtime 同步 hold 已经成功。后者专供 Scene stop 清理失去 Pilot 的本机
// 仿真实例，不能被普通 Robot stop 调用。真机、远程 Runtime、没有这两类证据
// 以及仍在线的 supervisor 都会被拒绝。
func (o *Orchestrator) ReclaimInterruptedSimulation(
	ctx context.Context, instanceID, nextSceneInstanceID string,
	runtimeHoldConfirmed bool, reason string,
) (RuntimeInstance, error) {
	instance, err := o.store.GetRuntimeInstance(ctx, instanceID)
	if err != nil {
		return RuntimeInstance{}, err
	}
	lock := o.lockRobot(instance.RobotID)
	lock.Lock()
	defer lock.Unlock()
	instance, err = o.store.GetRuntimeInstance(ctx, instanceID)
	if err != nil {
		return RuntimeInstance{}, err
	}
	if instance.Status != StateInterrupted || (instance.Backend != "mujoco" && instance.Backend != "isaac") ||
		strings.TrimSpace(nextSceneInstanceID) == "" ||
		(instance.SceneInstanceID == nextSceneInstanceID && !runtimeHoldConfirmed) {
		return instance, fmt.Errorf(
			"Runtime %s 不满足受管仿真回收条件: status=%s backend=%s old_scene=%s new_scene=%s hold_confirmed=%t",
			instance.InstanceID, instance.Status, instance.Backend,
			instance.SceneInstanceID, nextSceneInstanceID,
			runtimeHoldConfirmed,
		)
	}
	cleaner, ok := o.launcher.(InterruptedSimulationCleaner)
	if !ok {
		return instance, errors.New("Robot Runtime launcher 不支持受管仿真恢复")
	}
	evidence, err := cleaner.ReclaimInterruptedSimulation(ctx, instance, reason)
	if err != nil || !evidence.Confirmed {
		return o.interruptStop(ctx, instance, "managed_simulation_reclaim", evidence, err)
	}
	if err := o.ports.ReleaseAbilityFrameworkPort(ctx, instance.InstanceID); err != nil {
		return o.interruptStop(ctx, instance, "port_release", StopEvidence{}, err)
	}
	instance.Status = StateStopped
	instance.FailureReason = ""
	o.bump(&instance)
	if err := o.saveAndPublish(ctx, &instance, "robot.runtime.stopped"); err != nil {
		return instance, err
	}
	return instance, nil
}

func (o *Orchestrator) failStart(ctx context.Context, instance RuntimeInstance, cause error,
	launcherMayHaveStarted bool) (RuntimeInstance, error) {
	confirmed := true
	// 只有 Launcher 已经接手实例后才需要执行安全停止。端口分配或持久化失败时
	// 尚无 Robot 侧进程，直接释放端口即可，不能虚构一次 Robot stop。
	if launcherMayHaveStarted {
		evidence, err := o.launcher.Stop(ctx, instance, "runtime startup failed")
		confirmed = err == nil && evidence.Confirmed
	}
	if confirmed && instance.AbilityFrameworkPort > 0 {
		confirmed = o.ports.ReleaseAbilityFrameworkPort(ctx, instance.InstanceID) == nil
	}
	instance.Status = StateFailed
	if !confirmed {
		instance.Status = StateInterrupted
	}
	instance.FailureReason = cause.Error()
	o.bump(&instance)
	_ = o.saveAndPublish(ctx, &instance, "robot.runtime."+string(instance.Status))
	return instance, cause
}

func (o *Orchestrator) interruptStop(ctx context.Context, instance RuntimeInstance, step string,
	evidence StopEvidence, cause error) (RuntimeInstance, error) {
	instance.Status = StateInterrupted
	if cause != nil {
		instance.FailureReason = step + ": " + cause.Error()
	} else {
		instance.FailureReason = step + ": 未取得停止证据"
	}
	o.bump(&instance)
	_ = o.saveAndPublish(ctx, &instance, "robot.runtime.interrupted")
	if cause != nil {
		return instance, cause
	}
	return instance, fmt.Errorf("%s 未取得停止证据: %+v", step, evidence.Details)
}

func (o *Orchestrator) saveAndPublish(ctx context.Context, instance *RuntimeInstance, eventType string) error {
	if err := o.store.SaveRuntimeInstance(ctx, *instance); err != nil {
		return err
	}
	return o.events.PublishRuntimeEvent(ctx, Event{Type: eventType, RobotID: instance.RobotID,
		ProjectID: instance.ProjectID, Instance: *instance})
}

func (o *Orchestrator) bump(instance *RuntimeInstance) {
	instance.Revision++
	instance.UpdatedAt = o.now().UTC()
}

func (o *Orchestrator) lockRobot(robotID string) *sync.Mutex {
	o.locksMu.Lock()
	defer o.locksMu.Unlock()
	lock := o.robotLocks[robotID]
	if lock == nil {
		lock = &sync.Mutex{}
		o.robotLocks[robotID] = lock
	}
	return lock
}

func safePathPart(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "/", "_")
	value = strings.ReplaceAll(value, string(filepath.Separator), "_")
	value = strings.ReplaceAll(value, "..", "_")
	if value == "" {
		return "_"
	}
	return value
}
