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
	"sort"
	"time"

	"insightos.cn/semantic-framework/internal/robotruntime"
	"insightos.cn/semantic-framework/internal/store"
)

type DeviceEvent struct {
	ID               string    `json:"id"`
	Sequence         int64     `json:"sequence"`
	ResourceType     string    `json:"resource_type"`
	ResourceID       string    `json:"resource_id"`
	ResourceRevision int64     `json:"resource_revision"`
	Type             string    `json:"type"`
	OccurredAt       time.Time `json:"occurred_at"`
	Payload          any       `json:"payload"`
}

type DeviceSnapshot struct {
	SnapshotVersion int                       `json:"snapshot_version"`
	EventSequence   int64                     `json:"event_sequence"`
	Robots          []map[string]any          `json:"robots"`
	SkillPackages   []store.RobotSkillPackage `json:"skill_packages"`
	Executions      []store.RobotExecution    `json:"executions"`
}

func (s *Service) DeviceSnapshot() (DeviceSnapshot, error) {
	pilots, err := s.st.ListRobotPilots()
	if err != nil {
		return DeviceSnapshot{}, err
	}
	runtimes, err := s.st.ListRuntimeInstances(context.Background())
	if err != nil {
		return DeviceSnapshot{}, err
	}
	latestRuntime := make(map[string]robotruntime.RuntimeInstance)
	for _, instance := range runtimes {
		if _, exists := latestRuntime[instance.RobotID]; !exists {
			latestRuntime[instance.RobotID] = instance
		}
	}
	latestPilot := make(map[string]store.RobotPilot)
	for _, pilot := range pilots {
		current, exists := latestPilot[pilot.RobotID]
		if !exists || pilot.LastSeenAt.After(current.LastSeenAt) {
			latestPilot[pilot.RobotID] = pilot
		}
	}
	robots := make([]map[string]any, 0, len(latestPilot)+len(latestRuntime))
	for robotID, pilot := range latestPilot {
		if instance, exists := latestRuntime[robotID]; exists {
			pilot.RuntimeInstance = &instance
		}
		view, viewErr := s.deviceView(pilot)
		if viewErr != nil {
			return DeviceSnapshot{}, viewErr
		}
		robots = append(robots, view)
		delete(latestRuntime, robotID)
	}
	for _, instance := range latestRuntime {
		robots = append(robots, runtimeDeviceView(instance))
	}
	packages, err := s.st.ListRobotSkillPackages()
	if packages == nil {
		packages = make([]store.RobotSkillPackage, 0)
	}
	sort.Slice(robots, func(i, j int) bool {
		return stringValue(robots[i]["robot_id"]) < stringValue(robots[j]["robot_id"])
	})

	if err != nil {
		return DeviceSnapshot{}, err
	}
	executions, err := s.st.ListRobotExecutions("", "", 200)
	if err != nil {
		return DeviceSnapshot{}, err
	}
	if executions == nil {
		executions = make([]store.RobotExecution, 0)
	}
	return DeviceSnapshot{SnapshotVersion: 1, EventSequence: s.deviceSequence.Load(), Robots: robots, SkillPackages: packages, Executions: executions}, nil
}

func (s *Service) Device(robotID string) (map[string]any, error) {
	pilot, err := s.st.GetActiveRobotPilot(robotID)
	if err == nil {
		return s.deviceView(pilot)
	}
	instance, runtimeErr := s.st.GetLatestRuntimeByRobot(context.Background(), robotID)
	if runtimeErr == nil {
		return runtimeDeviceView(instance), nil
	}
	if !errors.Is(runtimeErr, robotruntime.ErrInstanceNotFound) {
		return nil, runtimeErr
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, store.ErrNotFound
	}
	return nil, err
}

func (s *Service) deviceView(pilot store.RobotPilot) (map[string]any, error) {
	if instance, err := s.st.GetLatestRuntimeByRobot(context.Background(), pilot.RobotID); err == nil {
		pilot.RuntimeInstance = &instance
	} else if !errors.Is(err, robotruntime.ErrInstanceNotFound) {
		return nil, err
	}
	skills, err := s.st.ListRobotPilotSkills(pilot.PilotInstanceID)
	if err != nil {
		return nil, err
	}
	if skills == nil {
		skills = make([]store.RobotPilotSkill, 0)
	}
	desiredSkills, err := s.st.ListRobotDesiredSkills(pilot.RobotID)
	if err != nil {
		return nil, err
	}
	executions, err := s.st.ListRobotExecutions("", pilot.RobotID, 20)
	if err != nil {
		return nil, err
	}
	var current *store.RobotExecution
	for index := range executions {
		if executions[index].ID == pilot.CurrentExecutionID || activeRobotStatus(executions[index].Status) {
			item := executions[index]
			current = &item
			break
		}
	}
	// Store 保存的是最后一次 Pilot 上报，进程异常退出或 Server 重启后可能仍是
	// busy/ready。设备页必须以当前 Gateway 会话为在线事实；历史 Execution 和
	// Ability 目录仍保留供排障，但不能再把它们显示成在线或可执行。
	live := s.IsOnline(pilot.PilotInstanceID)
	if !live {
		pilot.Status = "offline"
		pilot.RobotStatus = "offline"
		pilot.AbilityFrameworkStatus = "offline"
	}
	abilities := pilot.Abilities
	if !live {
		abilities = make([]map[string]any, 0, len(pilot.Abilities))
		for _, ability := range pilot.Abilities {
			cached := make(map[string]any, len(ability)+4)
			for key, value := range ability {
				cached[key] = value
			}
			cached["status"], cached["health"], cached["state"], cached["healthy"] = "offline", "unknown", "Unknown", false
			abilities = append(abilities, cached)
		}
	}
	healthy := 0
	for _, ability := range abilities {
		if value, ok := ability["healthy"].(bool); ok {
			if value {
				healthy++
			}
			continue
		}
		if status, _ := ability["health"].(string); status == "healthy" {
			healthy++
			continue
		}
		if state, _ := ability["state"].(string); state == "Running" {
			healthy++
		}
	}
	if !live {
		healthy = 0
	}
	environment := "real"
	if pilot.Backend != "real" {
		environment = "simulation"
	}
	status := pilot.RobotStatus
	if status == "" {
		if pilot.Status == "online" {
			status = "idle"
		} else {
			status = "offline"
		}
	}
	view := map[string]any{"robot_id": pilot.RobotID, "display_name": pilot.DisplayName, "model": pilot.RobotModel,
		"backend": pilot.Backend, "environment": environment, "status": status, "revision": pilot.Revision,
		"pilot":                  map[string]any{"instance_id": pilot.PilotInstanceID, "status": pilot.Status, "version": pilot.PilotVersion, "last_heartbeat_at": pilot.LastSeenAt},
		"ability_framework":      map[string]any{"status": pilot.AbilityFrameworkStatus, "healthy_instances": healthy, "total_instances": len(abilities)},
		"skill_catalog_revision": pilot.SkillCatalogRevision, "ability_catalog_revision": pilot.AbilityCatalogRevision,
		"installed_skills": skills, "desired_skills": desiredSkills, "abilities": abilities, "sensors": pilot.Sensors,
		"configuration":        pilot.Configuration,
		"runtime_instance":     pilot.RuntimeInstance,
		"current_execution_id": pilot.CurrentExecutionID, "progress": nil}
	if run, err := s.st.GetRobotConversationRun(pilot.RobotID); err == nil {
		view["current_run"] = run
		view["run_id"] = run.ID
		view["project_id"] = run.ProjectID
		view["conversation_id"] = run.ChatSessionID
		if live && status == "idle" {
			view["status"] = "busy"
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if current != nil {
		view["project_id"] = current.ProjectID
		view["task_id"] = current.TaskID
		view["current_stage"] = current.Stage
		view["progress"] = current.Progress
	}
	return view, nil
}

func runtimeDeviceView(instance robotruntime.RuntimeInstance) map[string]any {
	environment := instance.Kind
	if environment == "" {
		environment = "unknown"
	}
	pilotStatus := "connecting"
	if instance.Status == robotruntime.StateFailed || instance.Status == robotruntime.StateStopped ||
		instance.Status == robotruntime.StateInterrupted {
		pilotStatus = "offline"
	}
	return map[string]any{
		"robot_id": instance.RobotID, "display_name": instance.RobotID, "model": instance.RobotModel,
		"backend": instance.Backend, "environment": environment, "status": instance.Status,
		"revision":               instance.Revision,
		"pilot":                  map[string]any{"instance_id": instance.PilotInstanceID, "status": pilotStatus},
		"ability_framework":      map[string]any{"status": instance.Status, "healthy_instances": 0, "total_instances": 0},
		"skill_catalog_revision": int64(0), "ability_catalog_revision": int64(0),
		"installed_skills": []store.RobotPilotSkill{}, "desired_skills": []store.RobotDesiredSkill{}, "abilities": []map[string]any{}, "sensors": []map[string]any{},
		"runtime_instance": instance, "current_execution_id": "", "progress": nil,
	}
}

// PublishRuntimeEvent 实现 robotruntime.EventSink。Orchestrator 已先保存状态，
// 因此设备快照与事件在 Pilot 尚未注册时也保持一致。
func (s *Service) PublishRuntimeEvent(_ context.Context, event robotruntime.Event) error {
	payload := map[string]any{"runtime_instance": event.Instance, "robot_id": event.RobotID}
	s.publishDevice("robot_runtime_instance", event.Instance.InstanceID, event.Type,
		event.Instance.Revision, payload)
	if event.ProjectID != "" {
		s.events.PublishRobotEvent(event.ProjectID, "robot_runtime_instance", event.Instance.InstanceID,
			event.Type, event.Instance.Revision, payload)
	}
	return nil
}
func (s *Service) SubscribeDevices(after int64) (<-chan DeviceEvent, func(), error) {
	current := s.deviceSequence.Load()
	if after < 0 || after > current {
		return nil, nil, errors.New("设备事件游标非法")
	}
	if after != 0 && after < current {
		return nil, nil, errors.New("设备事件存在缺口，请重新读取快照")
	}
	channel := make(chan DeviceEvent, 128)
	s.mu.Lock()
	s.subscribers[channel] = struct{}{}
	s.mu.Unlock()
	return channel, func() {
		s.mu.Lock()
		if _, ok := s.subscribers[channel]; ok {
			delete(s.subscribers, channel)
			close(channel)
		}
		s.mu.Unlock()
	}, nil
}

func (s *Service) publishDevice(resourceType, resourceID, eventType string, revision int64, payload any) {
	sequence := s.deviceSequence.Add(1)
	event := DeviceEvent{ID: "device-event-" + time.Now().UTC().Format("20060102T150405.000000000") + "-" + itoa64(sequence), Sequence: sequence,
		ResourceType: resourceType, ResourceID: resourceID, ResourceRevision: revision, Type: eventType, OccurredAt: s.now().UTC(), Payload: payload}
	s.mu.RLock()
	targets := make([]chan DeviceEvent, 0, len(s.subscribers))
	for target := range s.subscribers {
		targets = append(targets, target)
	}
	s.mu.RUnlock()
	for _, target := range targets {
		select {
		case target <- event:
		default:
		}
	}
}

func (s *Service) publishPilotView(pilot store.RobotPilot, eventType, resourceType string) {
	view, err := s.deviceView(pilot)
	if err != nil {
		return
	}
	if resourceType == "" {
		resourceType = "robot"
	}
	s.publishDevice(resourceType, pilot.RobotID, eventType, pilot.Revision, map[string]any{"robot": view})
}

func itoa64(value int64) string {
	if value == 0 {
		return "0"
	}
	buffer := [20]byte{}
	position := len(buffer)
	for value > 0 {
		position--
		buffer[position] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[position:])
}
