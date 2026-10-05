package bootstrap

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/simulation"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/log"
)

func TestSnapshotRobotKeepsRuntimeVisualReference(t *testing.T) {
	raw := json.RawMessage(`{"robot_id":"r1-pro-1","base_pose":{"position":[0,0,0.01],"quaternion_xyzw":[0,0,0,1],"frame_id":"world"},"visual_ref":{"visual_id":"r1_pro_chassis","version":"2"}}`)
	entity, sourceID, err := snapshotRobot(raw, simulation.SceneSnapshot{
		InstanceID: "scene-1", CoordinateFrame: "world",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sourceID != "r1-pro-1" {
		t.Fatalf("Robot source_id 丢失: %q", sourceID)
	}
	if entity.Properties["source_id"] != "r1-pro-1" {
		t.Fatalf("Entity 未保留公开 source_id: %#v", entity.Properties)
	}
	visual, ok := entity.Properties["visual_ref"].(map[string]string)
	if !ok {
		t.Fatalf("Robot visual_ref 未保留: %#v", entity.Properties)
	}
	if visual["visual_id"] != "r1_pro_chassis" || visual["version"] != "2" {
		t.Fatalf("Robot visual_ref 错误: %#v", visual)
	}
}

func TestSimulationCheckpointOnlyAdvancesChangedEntityRevision(t *testing.T) {
	st := storetest.OpenMigrated(t, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	project, err := st.CreateProject("map-owner", "Map checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	rawObject := func(sourceID string, x float64) json.RawMessage {
		body, marshalErr := json.Marshal(map[string]any{
			"source_id": sourceID, "category": "tote", "name": sourceID,
			"pose": map[string]any{
				"position":        []float64{x, 0, 0.2},
				"quaternion_xyzw": []float64{0, 0, 0, 1},
				"frame_id":        "world",
			},
			"extent": []float64{0.6, 0.4, 0.34},
			"state":  map[string]any{"stable": true},
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return body
	}
	snapshot := simulation.SceneSnapshot{
		InstanceID: "scene-1", Generation: 1, CoordinateFrame: "world",
		ObservedAt: time.Now().UTC(),
		Objects: []json.RawMessage{
			rawObject("tote-a", 0),
			rawObject("tote-b", 1),
		},
	}
	bus := event.NewBus(log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	events := bus.Subscribe(event.TopicAgentEvents)
	defer bus.Unsubscribe(events)
	syncer := simulationMapSync{store: st, bus: bus}
	if err := syncer.ApplySimulationCheckpoint(
		context.Background(), project.ID, snapshot, "scene_started", false,
	); err != nil {
		t.Fatal(err)
	}
	first, err := st.GetMapSnapshot(project.ID, store.MapSlotSimulation, 0)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.ObservedAt = snapshot.ObservedAt.Add(time.Second)
	if err := syncer.ApplySimulationCheckpoint(
		context.Background(), project.ID, snapshot, "unchanged", false,
	); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := st.GetMapSnapshot(project.ID, store.MapSlotSimulation, 0)
	if unchanged.Map.Revision != first.Map.Revision {
		t.Fatalf("相同物理快照不应推进 Map revision: %d -> %d",
			first.Map.Revision, unchanged.Map.Revision)
	}

	snapshot.Objects[0] = rawObject("tote-a", 0.1)
	if err := syncer.ApplySimulationCheckpoint(
		context.Background(), project.ID, snapshot, "grasp_completed", false,
	); err != nil {
		t.Fatal(err)
	}
	changed, _ := st.GetMapSnapshot(project.ID, store.MapSlotSimulation, 0)
	revisions := make(map[string]int64)
	for _, entity := range changed.Entities {
		sourceID, _ := entity.Properties["source_id"].(string)
		revisions[sourceID] = entity.Revision
	}
	if changed.Map.Revision != first.Map.Revision+1 ||
		revisions["tote-a"] != 2 || revisions["tote-b"] != 1 {
		t.Fatalf("只应推进真实变化的 Entity: map=%d revisions=%#v",
			changed.Map.Revision, revisions)
	}
	snapshot.Objects[0] = rawObject("tote-a", 0)
	if err := syncer.ApplySimulationCheckpoint(context.Background(), project.ID, snapshot, "scene_reset", true); err != nil {
		t.Fatal(err)
	}
	reset, _ := st.GetMapSnapshot(project.ID, store.MapSlotSimulation, 0)
	if reset.Map.Generation != first.Map.Generation+1 {
		t.Fatalf("reset 未创建新地图版本: %+v", reset.Map)
	}
	for _, entity := range reset.Entities {
		if entity.Name == "tote-a" && entity.Pose.Position.X != 0 {
			t.Fatalf("reset 未恢复物品: %+v", entity)
		}
	}
	for i, m := range []store.SemanticMap{first.Map, changed.Map, reset.Map} {
		select {
		case ev := <-events:
			envelope := ev.Payload.(ws.Envelope)
			payload := envelope.Payload.(map[string]any)
			if envelope.ProjectID != project.ID || envelope.ResourceType != "semantic_map" ||
				envelope.ResourceID != store.MapSlotSimulation || envelope.Revision != m.Revision ||
				payload["map_id"] != store.MapSlotSimulation || payload["generation"] != m.Generation ||
				payload["reason"] != []string{"scene_started", "grasp_completed", "scene_reset"}[i] {
				t.Fatalf("地图通知的作用域或版本错误: %+v", envelope)
			}
		case <-time.After(time.Second):
			t.Fatal("地图检查点写入成功后没有通知")
		}
	}
	select {
	case ev := <-events:
		t.Fatalf("无变化的检查点不应制造通知: %+v", ev)
	default:
	}
}
