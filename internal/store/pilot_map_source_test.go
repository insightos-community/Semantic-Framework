package store

import (
	"reflect"
	"testing"
	"time"
)

func TestProjectRealSceneFollowsPersistedPilotBinding(t *testing.T) {
	st := openMigratedStore(t)
	project, err := st.CreateProject("owner", "kitchen")
	if err != nil {
		t.Fatal(err)
	}
	if scene, err := st.GetProjectRealScene(project.ID); err != nil || scene != nil {
		t.Fatalf("unbound project acquired a real scene: %+v %v", scene, err)
	}
	bind := func(id, backend string) {
		t.Helper()
		if err := st.SaveRobotPilot(RobotPilot{PilotInstanceID: id, RobotID: "robot-" + id,
			DisplayName: "R1Pro " + id, Backend: backend, Status: "offline", LastSeenAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := st.SavePilotMapBinding(PilotMapBinding{ProjectID: project.ID, PilotID: id, SyncedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	bind("first", "real")
	first, err := st.GetProjectRealScene(project.ID)
	if err != nil || first == nil {
		t.Fatalf("missing real scene: %+v %v", first, err)
	}
	if first.SchemaVersion != "1.0.0" || first.MapID != MapSlotReal || first.Kind != "real" || first.RobotID != "robot-first" {
		t.Fatalf("unexpected default scene: %+v", first)
	}
	again, err := st.GetProjectRealScene(project.ID)
	if err != nil || !reflect.DeepEqual(first, again) {
		t.Fatalf("refresh changed scene: %+v %v", again, err)
	}
	bind("second", "real")
	next, err := st.GetProjectRealScene(project.ID)
	if err != nil || next == nil || next.SceneID != first.SceneID || next.PilotID != "second" || next.RobotName != "R1Pro second" {
		t.Fatalf("source switch lost scene identity or kept stale robot: %+v %v", next, err)
	}
	bind("virtual", "simulation")
	if scene, err := st.GetProjectRealScene(project.ID); err != nil || scene != nil {
		t.Fatalf("simulation-only project acquired real scene: %+v %v", scene, err)
	}
	if err := st.SavePilotMapBinding(PilotMapBinding{ProjectID: project.ID, SyncedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if scene, err := st.GetProjectRealScene(project.ID); err != nil || scene != nil {
		t.Fatalf("removed binding retained real scene: %+v %v", scene, err)
	}
}

func TestPilotMapBindingAndGenerationRollbackTogether(t *testing.T) {
	st := openMigratedStore(t)
	project, err := st.CreateProject("owner", "map")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := PilotMapSnapshot{SchemaVersion: "1.0.0", FrameID: "map", Units: "m", MapHash: "one", MappingRevision: "one"}
	first := PilotMapSource{PilotID: "one", RobotID: "robot-one", Revision: 1, Snapshot: snapshot}
	before, err := st.ApplyPilotMapSource(project.ID, first, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.db.Exec(`CREATE TRIGGER reject_binding BEFORE UPDATE ON project_pilot_maps BEGIN SELECT RAISE(ABORT,'test binding failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	next := first
	next.PilotID = "two"
	next.RobotID = "robot-two"
	if _, err = st.ApplyPilotMapSource(project.ID, next, time.Now()); err == nil {
		t.Fatal("injected failure ignored")
	}
	after, _ := st.GetMapSnapshot(project.ID, MapSlotReal, 0)
	binding, _ := st.GetPilotMapBinding(project.ID)
	if binding.PilotID != "one" || after.Map.Generation != before.Map.Generation || after.Map.Revision != before.Map.Revision {
		t.Fatalf("partial source switch: %+v %+v", binding, after.Map)
	}
}
