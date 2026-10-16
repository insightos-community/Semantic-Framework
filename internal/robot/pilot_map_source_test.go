package robot

import (
	"errors"
	"sync"
	"testing"

	"insightos.cn/semantic-framework/internal/store"
)

func mapFixture() store.PilotMapSnapshot {
	return store.PilotMapSnapshot{SchemaVersion: "1.0.0", FrameID: "map", Units: "m", MapHash: "room", MappingRevision: "align", Points: [][3]float64{{1, 2, 3}}, Entities: []store.MapSourceEntity{{SourceID: "table", Entity: store.MapEntity{Type: "table", Name: "table", FrameID: "map", Pose: store.Pose{Orientation: store.Quaternion{W: 1}}, Bounds: store.Bounds{Kind: "point"}}}}}
}

func enrollMapPilot(t *testing.T, s *Service, owner, pilot, robot, project string) func() {
	t.Helper()
	e, err := s.CreatePilotEnrollment(owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ClaimPilotEnrollment(e.Code, pilot); err != nil {
		t.Fatal(err)
	}
	_, close, err := s.Connect(store.RobotPilot{PilotInstanceID: pilot, RobotID: robot, RobotModel: "R1PRO", Backend: "real", PilotVersion: "test", Configuration: map[string]any{"map_sync": map[string]any{"enabled": true, "project_id": project, "service_url": "http://user:secret@127.0.0.1:18050/?token=private"}}})
	if err != nil {
		t.Fatal(err)
	}
	return close
}

func TestPilotMapSubscriptions(t *testing.T) {
	st := openRobotTestStore(t)
	s := NewService(st, nil)
	a, _ := st.CreateProject("owner", "legacy")
	b, _ := st.CreateProject("owner", "new kitchen")
	foreign, _ := st.CreateProject("foreign", "foreign")
	close1 := enrollMapPilot(t, s, "owner", "pilot-a", "real", a.ID)
	defer close1()
	close2 := enrollMapPilot(t, s, "owner", "pilot-b", "sim", "")
	defer close2()
	close3 := enrollMapPilot(t, s, "foreign", "pilot-c", "other", "")
	defer close3()
	first := mapFixture()
	if _, err := s.SyncPilotMap("pilot-a", a.ID, first); err != nil {
		t.Fatal(err)
	}
	choices, err := s.MapSources("owner", b.ID)
	if err != nil || len(choices.Sources) != 2 {
		t.Fatalf("choices: %+v %v", choices, err)
	}
	for _, choice := range choices.Sources {
		if choice.ServiceURL != "http://127.0.0.1:18050/" {
			t.Fatalf("credential leaked: %+v", choice)
		}
	}
	if _, err = s.SelectMapSource("owner", foreign.ID, "pilot-a"); !errors.Is(err, ErrDirectRunScope) {
		t.Fatalf("foreign project accepted: %v", err)
	}
	if _, err = s.SelectMapSource("owner", b.ID, "pilot-c"); !errors.Is(err, ErrDirectRunScope) {
		t.Fatalf("foreign pilot accepted: %v", err)
	}
	if _, err = s.SelectMapSource("owner", b.ID, "pilot-b"); !errors.Is(err, ErrMapSourceUnavailable) {
		t.Fatalf("missing source accepted: %v", err)
	}
	bound, err := s.SelectMapSource("owner", b.ID, "pilot-a")
	if err != nil || len(bound.Entities) != 2 {
		t.Fatalf("immediate map: %+v %v", bound, err)
	}
	repeat, err := s.SelectMapSource("owner", b.ID, "pilot-a")
	if err != nil || repeat.Map.Revision != bound.Map.Revision {
		t.Fatalf("select not idempotent: %v", err)
	}
	first.Entities = nil
	if _, err = s.SyncPilotMap("pilot-a", a.ID, first); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, b.ID} {
		m, _ := st.GetMapSnapshot(id, store.MapSlotReal, 0)
		for _, e := range m.Entities {
			if e.Type == "table" && e.Status != "removed" {
				t.Fatal("deletion not fanned out")
			}
		}
	}
	second := mapFixture()
	second.MapHash = "other room"
	if _, err = s.SyncPilotMap("pilot-b", "", second); err != nil {
		t.Fatal(err)
	}
	switched, err := s.SelectMapSource("owner", a.ID, "pilot-b")
	if err != nil || switched.Map.Generation != 2 {
		t.Fatalf("source switch: %+v %v", switched, err)
	}
	old, _ := st.GetMapSnapshot(a.ID, store.MapSlotReal, 1)
	if !old.ReadOnly {
		t.Fatal("old generation lost")
	}
	if _, err = s.SyncPilotMap("pilot-a", a.ID, first); err != nil {
		t.Fatal(err)
	}
	binding, _ := st.GetPilotMapBinding(a.ID)
	if binding.PilotID != "pilot-b" {
		t.Fatal("legacy project overwrote explicit selection")
	}
	if _, err = s.SelectMapSource("owner", a.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SyncPilotMap("pilot-a", a.ID, first); err != nil {
		t.Fatal(err)
	}
	binding, _ = st.GetPilotMapBinding(a.ID)
	if binding.PilotID != "" {
		t.Fatal("legacy project overwrote explicit disconnect")
	}
	invalid := second
	invalid.Units = "mm"
	if _, err = s.SyncPilotMap("pilot-b", "", invalid); err == nil {
		t.Fatal("invalid source accepted")
	}
	cached, _ := st.GetPilotMapSource("pilot-b")
	if cached.Snapshot.Units != "m" {
		t.Fatal("invalid source poisoned cache")
	}
	recovered := NewService(st, nil)
	choices, err = recovered.MapSources("owner", b.ID)
	if err != nil || choices.Binding.PilotID != "pilot-a" {
		t.Fatalf("subscription did not survive service restart: %v", err)
	}
	for _, choice := range choices.Sources {
		if choice.Online {
			t.Fatal("offline source marked online")
		}
	}
	if _, err = recovered.SelectMapSource("owner", b.ID, "pilot-b"); err != nil {
		t.Fatalf("offline cached map unavailable: %v", err)
	}
	simulation, _ := st.GetMapSnapshot(b.ID, store.MapSlotSimulation, 0)
	if len(simulation.Entities) != 0 || simulation.Map.Revision != 0 {
		t.Fatal("simulation map modified")
	}
}

func TestPilotMapConcurrentSelectAndUpload(t *testing.T) {
	st := openRobotTestStore(t)
	s := NewService(st, nil)
	project, _ := st.CreateProject("owner", "kitchen")
	defer enrollMapPilot(t, s, "owner", "a", "real-a", "")()
	defer enrollMapPilot(t, s, "owner", "b", "real-b", "")()
	for _, pilot := range []string{"a", "b"} {
		if _, err := s.SyncPilotMap(pilot, "", mapFixture()); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pilot := []string{"a", "b"}[i%2]
			var err error
			if i%3 == 0 {
				_, err = s.SyncPilotMap(pilot, "", mapFixture())
			} else {
				_, err = s.SelectMapSource("owner", project.ID, pilot)
			}
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	binding, _ := st.GetPilotMapBinding(project.ID)
	m, _ := st.GetMapSnapshot(project.ID, store.MapSlotReal, 0)
	for _, e := range m.Entities {
		if e.Status == "active" && e.Properties["pilot_instance_id"] != binding.PilotID {
			t.Fatal("binding and projected map diverged")
		}
	}
}
