package robot

import (
	"insightos.cn/semantic-framework/internal/store"
	"testing"
)

func TestPilotRealMapOwnershipRevisionsAndRemoval(t *testing.T) {
	st := openRobotTestStore(t)
	project, err := st.CreateProject("owner", "real lab")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := st.CreateProject("another-user", "other lab")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(st, nil)
	enrollment, err := service.CreatePilotEnrollment("owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = service.ClaimPilotEnrollment(enrollment.Code, "pilot-real"); err != nil {
		t.Fatal(err)
	}
	_, close, err := service.Connect(store.RobotPilot{PilotInstanceID: "pilot-real", RobotID: "r1pro", RobotModel: "R1PRO", Backend: "real", PilotVersion: "0.5.0", Configuration: map[string]any{"map_sync": map[string]any{"project_id": project.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	syncToProject := func(pilotID, projectID string, snapshot store.PilotMapSnapshot) (store.MapSnapshot, error) {
		if _, err := service.SyncPilotMap(pilotID, projectID, snapshot); err != nil {
			return store.MapSnapshot{}, err
		}
		return st.GetMapSnapshot(projectID, store.MapSlotReal, 0)
	}
	snapshot := store.PilotMapSnapshot{SchemaVersion: "1.0.0", FrameID: "map", Units: "m", MapHash: "cloud-a", MappingRevision: "align-a", Points: [][3]float64{{1, 2, 3}}, Entities: []store.MapSourceEntity{{SourceID: "waypoint:one", Entity: store.MapEntity{Type: "waypoint", Name: "table", FrameID: "map", Pose: store.Pose{Position: store.Position{X: 1, Y: 2}, Orientation: store.Quaternion{W: 1}}, Bounds: store.Bounds{Kind: "point"}}}}}
	if _, err = syncToProject("pilot-real", foreign.ID, snapshot); err == nil {
		t.Fatal("cross-owner upload accepted")
	}
	first, err := syncToProject("pilot-real", project.ID, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if first.Map.FrameID != "map" || len(first.Entities) != 2 {
		t.Fatalf("map conversion: %+v", first)
	}
	query, err := st.QuerySemanticMap(project.ID, store.MapSlotReal, store.MapQuery{Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	for _, entity := range query.Entities {
		if entity.Type == "map_layer" {
			if _, exists := entity.Properties["points_m"]; exists {
				t.Fatal("point cloud leaked into Agent query")
			}
		}
	}
	repeat, err := syncToProject("pilot-real", project.ID, snapshot)
	if err != nil || repeat.Map.Revision != first.Map.Revision {
		t.Fatalf("retry created a new revision: %v", err)
	}
	snapshot.Entities = nil
	removed, err := syncToProject("pilot-real", project.ID, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if removed.Map.Revision != first.Map.Revision+1 {
		t.Fatal("snapshot was not atomically applied")
	}
	for _, e := range removed.Entities {
		if e.Type == "waypoint" && e.Status != "removed" {
			t.Fatal("deleted waypoint still active")
		}
	}
	snapshot.MapHash = "cloud-b"
	replaced, err := syncToProject("pilot-real", project.ID, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.Map.Generation != removed.Map.Generation+1 || replaced.Map.Revision != 1 {
		t.Fatal("map replacement failed to invalidate old generation")
	}
	old, err := st.GetMapSnapshot(project.ID, store.MapSlotReal, removed.Map.Generation)
	if err != nil || !old.ReadOnly {
		t.Fatal("old map was not retained read-only")
	}
	snapshot.Units = "mm"
	if _, err = syncToProject("pilot-real", project.ID, snapshot); err == nil {
		t.Fatal("millimetres accepted as metres")
	}
}
