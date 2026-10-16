package robot

import (
	"context"
	"errors"
	"insightos.cn/semantic-framework/internal/robotruntime"
	"insightos.cn/semantic-framework/internal/store"
	"testing"
	"time"
)

func TestProjectRobotCatalogOwnershipVersionsAndRemoval(t *testing.T) {
	st := openRobotTestStore(t)
	s := NewService(st, nil)
	project, err := st.CreateProject("owner", "real")
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateProject("owner", "other")
	if err != nil {
		t.Fatal(err)
	}
	disconnect := enrollMapPilot(t, s, "owner", "real-pilot", "real-robot", "")
	defer disconnect()
	defer enrollMapPilot(t, s, "foreign", "foreign-pilot", "foreign-robot", "")()
	if err = st.SaveRobotPilot(store.RobotPilot{PilotInstanceID: "legacy", RobotID: "unenrolled", LastSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"0.4.0-alpha.1", "0.4.0-alpha.2"} {
		if err = st.SaveRobotSkillPackage(store.RobotSkillPackage{Name: "r1pro-transfer", Version: version, Description: "搬运"}); err != nil {
			t.Fatal(err)
		}
	}
	if err = st.SaveRobotPilotSkill(store.RobotPilotSkill{PilotInstanceID: "real-pilot", Name: "r1pro-transfer", Version: "0.4.0-alpha.1", Enabled: true, Status: "installed", UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	before, err := s.ProjectRobots("owner", project.ID)
	if err != nil || len(before.Robots) != 0 || len(before.AvailableRobots) != 1 {
		t.Fatalf("catalog must filter foreign/unenrolled robots: %+v %v", before, err)
	}
	if err = s.SetProjectRobot("owner", project.ID, "foreign-robot", true); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign pilot added: %v", err)
	}
	if _, err = s.ProjectRobots("foreign", project.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign project readable: %v", err)
	}
	for range 2 {
		if err = s.SetProjectRobot("owner", project.ID, "real-robot", true); err != nil {
			t.Fatal(err)
		}
	}
	catalog, err := s.ProjectRobots("owner", project.ID)
	if err != nil || len(catalog.Robots) != 1 || len(catalog.AvailableRobots) != 0 {
		t.Fatalf("membership not idempotent: %+v %v", catalog, err)
	}
	skills := catalog.Robots[0]["skills"].([]map[string]any)
	if len(skills) != 1 || skills[0]["version"] != "0.4.0-alpha.1" {
		t.Fatalf("registry versions leaked into installed catalog: %+v", skills)
	}
	if _, ok := catalog.Robots[0]["configuration"]; ok {
		t.Fatal("configuration exposed")
	}
	unbound, _ := s.ProjectRobots("owner", other.ID)
	if len(unbound.Robots) != 0 {
		t.Fatal("project membership leaked")
	}
	if err = st.SaveRobotExecution(store.RobotExecution{ID: "active", ProjectID: project.ID, RobotID: "real-robot", RequestKey: "active", Status: "running", Revision: 1, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetProjectRobot("owner", project.ID, "real-robot", false); !errors.Is(err, ErrRobotBusy) {
		t.Fatalf("removed an executing robot: %v", err)
	}
	if err = st.SaveRobotExecution(store.RobotExecution{ID: "active", ProjectID: project.ID, RobotID: "real-robot", RequestKey: "active", Status: "completed", Revision: 2, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	disconnect()
	catalog, err = s.ProjectRobots("owner", project.ID)
	if err != nil || catalog.Robots[0]["pilot"].(map[string]any)["status"] != "offline" {
		t.Fatalf("offline fact lost: %+v %v", catalog, err)
	}
	if err = s.SetProjectRobot("owner", project.ID, "real-robot", false); err != nil {
		t.Fatal(err)
	}
	after, _ := s.ProjectRobots("owner", project.ID)
	if len(after.Robots) != 0 {
		t.Fatal("last member removal did not persist")
	}
}

func TestManagedSimulationRobotKeepsExistingProjectAssociation(t *testing.T) {
	st := openRobotTestStore(t)
	s := NewService(st, nil)
	a, _ := st.CreateProject("owner", "simulation")
	b, _ := st.CreateProject("owner", "real")
	err := st.SaveRuntimeInstance(context.Background(), robotruntime.RuntimeInstance{InstanceID: "sim-instance", ProjectID: a.ID, RobotID: "sim-robot", Backend: "mujoco", Kind: "simulation", Revision: 1, UpdatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	member, err := st.ProjectHasRobot(a.ID, "sim-robot")
	if err != nil || !member {
		t.Fatalf("managed simulation association lost: %v %v", member, err)
	}
	member, err = st.ProjectHasRobot(b.ID, "sim-robot")
	if err != nil || member {
		t.Fatal("managed robot leaked to another project")
	}
	if err = s.SetProjectRobot("owner", b.ID, "sim-robot", true); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("managed robot reassigned: %v", err)
	}
}
