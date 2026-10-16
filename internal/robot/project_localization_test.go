package robot

import (
	"strings"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

func TestProjectLocalizationUsesEachRobotFeedbackWithoutMapEntities(t *testing.T) {
	st := openRobotTestStore(t)
	s := NewService(st, nil)
	p, _ := st.CreateProject("owner", "lab")
	disconnect := enrollMapPilot(t, s, "owner", "real", "real-robot", p.ID)
	defer disconnect()
	defer enrollMapPilot(t, s, "owner", "sim", "sim-robot", "")()
	for _, id := range []string{"real-robot", "sim-robot"} {
		if err := s.SetProjectRobot("owner", p.ID, id, true); err != nil {
			t.Fatal(err)
		}
	}
	model := twinMap(t, s, "real", p.ID)
	now := time.Now()
	s.now = func() time.Time { return now }
	age := .05
	v := PilotTwinState{SchemaVersion: "1.0.0", Frame: "map", Units: "m_rad", StreamID: "a", Sequence: 1,
		MapHash: "room", MappingRevision: "align", ModelSHA256: model, URDFSHA256: strings.Repeat("a", 64),
		Pose: []float64{1.7, -2.1, .168, -1.5}, PoseAge: &age, SampleAge: .1, CollectedAt: 123}
	if err := s.SyncPilotTwin("real", v); err != nil {
		t.Fatal(err)
	}
	before, _ := st.GetMapSnapshot(p.ID, store.MapSlotReal, 0)
	read := func(id string) map[string]any {
		t.Helper()
		catalog, err := s.ProjectRobots("owner", p.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range catalog.Robots {
			if r["robot_id"] == id {
				return r["localization"].(map[string]any)
			}
		}
		t.Fatal("missing project member", id)
		return nil
	}
	value := read("real-robot")
	if value["available"] != true || value["status"] != "current" || value["selected_map_source"] != true || value["age_s"].(float64) > .16 {
		t.Fatalf("fresh pose missing: %+v", value)
	}
	pose := value["pose"].(map[string]any)
	if pose["x"] != 1.7 || pose["y"] != -2.1 || pose["yaw"] != -1.5 || pose["z"] != nil {
		t.Fatalf("incorrect planar pose: %+v", pose)
	}
	if sim := read("sim-robot"); sim["available"] != false || sim["pose"] != nil || sim["selected_map_source"] != false {
		t.Fatalf("real feedback substituted for simulator: %+v", sim)
	}
	// Disconnecting the map source does not erase a member's own localization.
	if _, err := s.SelectMapSource("owner", p.ID, ""); err != nil {
		t.Fatal(err)
	}
	if value = read("real-robot"); value["available"] != true || value["selected_map_source"] != false {
		t.Fatal(value)
	}
	// Report delayed or lost feedback explicitly, without exposing stale coordinates.
	now = now.Add(6 * time.Second)
	if value = read("real-robot"); value["available"] != false || value["status"] != "stale" || value["pose"] != nil {
		t.Fatal(value)
	}
	v.Sequence++
	v.PoseAge = new(float64)
	*v.PoseAge = 6
	if err := s.SyncPilotTwin("real", v); err != nil {
		t.Fatal(err)
	}
	if value = read("real-robot"); value["status"] != "stale" || value["pose"] != nil {
		t.Fatal(value)
	}
	v.Sequence++
	v.Pose = nil
	v.PoseAge = nil
	if err := s.SyncPilotTwin("real", v); err != nil {
		t.Fatal(err)
	}
	if value = read("real-robot"); value["status"] != "pose_missing" || value["pose"] != nil {
		t.Fatal(value)
	}
	v.Sequence++
	v.Pose = []float64{1, 2, 0, 0}
	v.PoseAge = &age
	if err := s.SyncPilotTwin("real", v); err != nil {
		t.Fatal(err)
	}
	disconnect()
	if value = read("real-robot"); value["status"] != "offline" || value["pose"] != nil {
		t.Fatal(value)
	}
	pilot, err := st.GetRobotPilot("real")
	if err != nil {
		t.Fatal(err)
	}
	_, closeAgain, err := s.Connect(pilot)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAgain()
	if value = read("real-robot"); value["available"] != false || value["pose"] != nil {
		t.Fatal("old session revived", value)
	}
	v.Sequence++
	if err := s.SyncPilotTwin("real", v); err != nil {
		t.Fatal(err)
	}
	if value = read("real-robot"); value["available"] != true {
		t.Fatal(value)
	}
	after, _ := st.GetMapSnapshot(p.ID, store.MapSlotReal, 0)
	if before.Map.Revision != after.Map.Revision {
		t.Fatal("query changed map revision")
	}
	other, _ := st.CreateProject("owner", "other")
	catalog, err := s.ProjectRobots("owner", other.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range catalog.AvailableRobots {
		if r["localization"] != nil {
			t.Fatal("unadded robot position exposed")
		}
	}
	if _, err = s.ProjectRobots("foreign", p.ID); err == nil {
		t.Fatal("foreign owner allowed")
	}
	// A new coordinate mapping invalidates feedback until the next matching frame.
	snapshot, err := st.GetPilotMapSource("real")
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Snapshot.MappingRevision = "new-frame"
	if _, err = s.SyncPilotMap("real", p.ID, snapshot.Snapshot); err != nil {
		t.Fatal(err)
	}
	if value = read("real-robot"); value["status"] != "map_changed" || value["pose"] != nil {
		t.Fatal(value)
	}
}
