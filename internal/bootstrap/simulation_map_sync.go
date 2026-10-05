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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/simulation"
	"insightos.cn/semantic-framework/internal/store"
)

type projectSimulationResolver struct{ store *store.Store }

func (r projectSimulationResolver) RuntimePreference(projectID string) (string, string, error) {
	project, err := r.store.GetProject(projectID)
	if err != nil {
		return "", "", err
	}
	return project.RuntimeProfileID, project.PreferredRuntimeInstallationID, nil
}

func (r projectSimulationResolver) RememberRuntimePreference(
	projectID, profileID, installationID string,
) error {
	return r.store.RememberProjectRuntimePreference(projectID, profileID, installationID)
}

// simulationMapSync 把 Runtime 公共 source_id 稳定映射成 Semantic Map Entity。
// 它只由明确检查点调用；关节、Contact、Camera 与连续物理位姿不会触发写入。
type simulationMapSync struct {
	store *store.Store
	bus   *event.Bus
}

func (s simulationMapSync) ResolveSimulationSource(
	projectID string, generation int64, sourceID string,
) (string, error) {
	// generation 属于 Semantic Map，不是 MuJoCo Scene generation。必须使用
	// 调用方明确携带的版本查询；旧版本失效时由 Store 返回 stale，禁止悄悄
	// 换成当前地图后把旧目标解析成另一个实体。
	entityID, err := s.store.ResolveMapSourceEntity(
		projectID, store.MapSlotSimulation, generation, "simulation", sourceID,
	)
	return entityID, simulationLinkError(err)
}

func (s simulationMapSync) ResolveSimulationEntity(
	projectID string, generation int64, entityID string,
) (string, error) {
	sourceID, err := s.store.ResolveMapEntitySource(
		projectID, store.MapSlotSimulation, generation, "simulation", entityID,
	)
	return sourceID, simulationLinkError(err)
}

func simulationLinkError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return simulation.ErrNotFound
	}
	if errors.Is(err, store.ErrStaleMapGeneration) {
		return simulation.ErrConflict
	}
	return err
}

func (s simulationMapSync) ApplySimulationSnapshot(
	ctx context.Context, projectID string, snapshot simulation.SceneSnapshot,
) error {
	return s.ApplySimulationCheckpoint(ctx, projectID, snapshot, "checkpoint", false)
}

func (s simulationMapSync) ApplySimulationCheckpoint(
	_ context.Context,
	projectID string,
	snapshot simulation.SceneSnapshot,
	reason string,
	newGeneration bool,
) error {
	current, err := s.store.GetSemanticMap(projectID, store.MapSlotSimulation)
	if err != nil {
		return err
	}
	if newGeneration {
		current, err = s.store.CreateMapGeneration(
			projectID, store.MapSlotSimulation, current.Revision, reason, time.Now().UTC())
		if err != nil {
			return err
		}
	}
	entities := make([]store.MapSourceEntity, 0,
		len(snapshot.Robots)+len(snapshot.Objects)+len(snapshot.Regions))
	for _, raw := range snapshot.Objects {
		entity, sourceID, parseErr := snapshotEntity(raw, snapshot, false)
		if parseErr != nil {
			return parseErr
		}
		entities = append(entities, store.MapSourceEntity{SourceID: sourceID, Entity: entity})
	}
	for _, raw := range snapshot.Regions {
		entity, sourceID, parseErr := snapshotEntity(raw, snapshot, true)
		if parseErr != nil {
			return parseErr
		}
		entities = append(entities, store.MapSourceEntity{SourceID: sourceID, Entity: entity})
	}
	for _, raw := range snapshot.Robots {
		entity, sourceID, parseErr := snapshotRobot(raw, snapshot)
		if parseErr != nil {
			return parseErr
		}
		entities = append(entities, store.MapSourceEntity{SourceID: sourceID, Entity: entity})
	}
	entities, err = s.changedSimulationEntities(projectID, current.Generation, entities)
	if err != nil {
		return err
	}
	if len(entities) == 0 {
		if newGeneration {
			s.publishCheckpoint(current, reason)
		}
		return nil
	}
	updated, err := s.store.ApplyMapUpdate(projectID, store.MapSlotSimulation, store.MapUpdate{
		Generation: current.Generation, ExpectedRevision: current.Revision,
		Source: "simulation", SourceEntities: entities,
	}, time.Now().UTC())
	if err == nil {
		s.publishCheckpoint(updated.Map, reason)
	}
	return err
}

// 所有检查点（包括 reset 与 Robot 完成）统一在写入成功后通知 Studio。
// 使用地图自身的 generation/revision，而不是 Runtime 的场景版本。
func (s simulationMapSync) publishCheckpoint(m store.SemanticMap, reason string) {
	if s.bus == nil {
		return
	}
	envelope := ws.NewEnvelope("", ws.ChannelDialogue, "simulation.map.synced", ws.ImportanceNormal, map[string]any{
		"map_id": m.Slot, "generation": m.Generation, "revision": m.Revision, "reason": reason,
	})
	envelope.ProjectID, envelope.ResourceType, envelope.ResourceID = m.ProjectID, "semantic_map", m.Slot
	envelope.Revision = m.Revision
	s.bus.Publish(event.TopicAgentEvents, envelope)
}

// changedSimulationEntities 只把真实发生变化的场景对象写入 Map。SceneSnapshot
// 每次都会携带所有对象；若无条件 upsert，所有 Entity revision 都会增长，
// Leader 和 Robot Agent 看到的记忆会产生没有语义意义的变化。Map revision 仍在
// 每个抓取/放置检查点按一次事务推进，Entity revision 只记录实体自身变化；
// 两者都用于规划追踪，不作为 Robot Skill 接受或拒绝物理动作的依据。
func (s simulationMapSync) changedSimulationEntities(
	projectID string,
	generation int64,
	incoming []store.MapSourceEntity,
) ([]store.MapSourceEntity, error) {
	current, err := s.store.GetMapSnapshot(projectID, store.MapSlotSimulation, generation)
	if err != nil {
		return nil, err
	}
	bySource := make(map[string]store.MapEntity, len(current.Entities))
	for _, entity := range current.Entities {
		sourceID, _ := entity.Properties["source_id"].(string)
		if sourceID != "" {
			bySource[sourceID] = entity
		}
	}
	changed := make([]store.MapSourceEntity, 0, len(incoming))
	for _, item := range incoming {
		existing, exists := bySource[item.SourceID]
		if !exists || !sameSimulationEntity(existing, item.Entity) {
			changed = append(changed, item)
		}
	}
	return changed, nil
}

func sameSimulationEntity(existing, incoming store.MapEntity) bool {
	// 与 Store 的 manual-field 规则保持一致：用户显式固定的字段不会被 Runtime
	// 覆盖，也不应因 Runtime 仍上报原值而反复制造 revision。
	if existing.ManualFields["existence"] {
		incoming.Status = existing.Status
	}
	if existing.ManualFields["type"] {
		incoming.Type = existing.Type
	}
	if existing.ManualFields["name"] {
		incoming.Name = existing.Name
	}
	if existing.ManualFields["frame_id"] {
		incoming.FrameID = existing.FrameID
	}
	if existing.ManualFields["pose"] {
		incoming.Pose = existing.Pose
	}
	if existing.ManualFields["bounds"] {
		incoming.Bounds = existing.Bounds
	}
	if existing.ManualFields["properties"] {
		incoming.Properties = existing.Properties
	}
	type comparableEntity struct {
		Type       string
		Name       string
		Status     string
		FrameID    string
		Pose       store.Pose
		Bounds     store.Bounds
		Properties map[string]any
	}
	left, _ := json.Marshal(comparableEntity{
		existing.Type, existing.Name, existing.Status, existing.FrameID,
		existing.Pose, existing.Bounds, existing.Properties,
	})
	right, _ := json.Marshal(comparableEntity{
		incoming.Type, incoming.Name, incoming.Status, incoming.FrameID,
		incoming.Pose, incoming.Bounds, incoming.Properties,
	})
	return bytes.Equal(left, right)
}

type snapshotPose struct {
	Position       [3]float64 `json:"position"`
	QuaternionXYZW [4]float64 `json:"quaternion_xyzw"`
	FrameID        string     `json:"frame_id"`
}

type snapshotObject struct {
	SourceID  string            `json:"source_id"`
	Category  string            `json:"category"`
	Name      string            `json:"name"`
	Pose      snapshotPose      `json:"pose"`
	Extent    *[3]float64       `json:"extent"`
	State     map[string]any    `json:"state"`
	VisualRef map[string]string `json:"visual_ref,omitempty"`
}

func snapshotEntity(
	raw json.RawMessage, snapshot simulation.SceneSnapshot, region bool,
) (store.MapEntity, string, error) {
	var value snapshotObject
	if err := json.Unmarshal(raw, &value); err != nil {
		return store.MapEntity{}, "", fmt.Errorf("解析 Runtime 场景对象失败: %w", err)
	}
	if value.SourceID == "" || value.Name == "" {
		return store.MapEntity{}, "", fmt.Errorf("Runtime 场景对象缺少 source_id 或 name")
	}
	category := value.Category
	if category == "" {
		category = "region"
	}
	bounds := store.Bounds{Kind: "point"}
	if value.Extent != nil {
		size := store.Position{X: value.Extent[0], Y: value.Extent[1], Z: value.Extent[2]}
		bounds = store.Bounds{Kind: "box", Size: &size}
	}
	frameID := value.Pose.FrameID
	if frameID == "" {
		frameID = snapshot.CoordinateFrame
	}
	properties := value.State
	if properties == nil {
		properties = make(map[string]any)
	}
	// source_id 是 Runtime 与地图 Entity 的稳定公开关联键。将它随 Entity
	// 快照返回，Studio 才能把同一场景 GLB 的节点与地图对象一次性关联，
	// 无需为每个 Entity 逐个请求 SourceLink，也不会暴露引擎 body/geom ID。
	properties["source_id"] = value.SourceID
	if len(value.VisualRef) > 0 {
		properties["visual_ref"] = value.VisualRef
	}
	entity := store.MapEntity{
		Type: category, Name: value.Name, Status: store.EntityStatusActive,
		FrameID: frameID,
		Pose: store.Pose{
			Position: store.Position{
				X: value.Pose.Position[0], Y: value.Pose.Position[1], Z: value.Pose.Position[2],
			},
			Orientation: store.Quaternion{
				X: value.Pose.QuaternionXYZW[0], Y: value.Pose.QuaternionXYZW[1],
				Z: value.Pose.QuaternionXYZW[2], W: value.Pose.QuaternionXYZW[3],
			},
		},
		Bounds: bounds, Properties: properties, Source: "simulation",
		SourceTimestamp: &snapshot.ObservedAt,
		Evidence:        []string{"scene-snapshot:" + snapshot.InstanceID},
	}
	if region {
		entity.Type = "region"
	}
	return entity, value.SourceID, nil
}

type snapshotRobotValue struct {
	RobotID   string            `json:"robot_id"`
	BasePose  *snapshotPose     `json:"base_pose"`
	VisualRef map[string]string `json:"visual_ref,omitempty"`
}

func snapshotRobot(
	raw json.RawMessage, snapshot simulation.SceneSnapshot,
) (store.MapEntity, string, error) {
	var value snapshotRobotValue
	if err := json.Unmarshal(raw, &value); err != nil {
		return store.MapEntity{}, "", fmt.Errorf("解析 Runtime Robot 状态失败: %w", err)
	}
	if value.RobotID == "" || value.BasePose == nil {
		return store.MapEntity{}, "", fmt.Errorf("Runtime Robot 状态缺少 robot_id 或 base_pose")
	}
	objectRaw, _ := json.Marshal(snapshotObject{
		SourceID: value.RobotID, Category: "robot", Name: value.RobotID,
		Pose: *value.BasePose, State: map[string]any{"virtual": true},
		VisualRef: value.VisualRef,
	})
	return snapshotEntity(objectRaw, snapshot, false)
}
