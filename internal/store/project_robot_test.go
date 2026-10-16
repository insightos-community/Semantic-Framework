package store

import (
	"testing"
	"time"
)

func TestProjectRealRobotBeforeMapAndMigration(t *testing.T) {
	st := openMigratedStore(t)
	project, err := st.CreateProject("owner", "real onboarding")
	if err != nil {
		t.Fatal(err)
	}
	if scene, err := st.GetProjectRealScene(project.ID); err != nil || scene != nil {
		t.Fatalf("empty project changed simulation path: %+v %v", scene, err)
	}
	now := time.Now().UTC()
	if err = st.SaveRobotPilot(RobotPilot{PilotInstanceID: "pilot", RobotID: "robot", Backend: "real", DisplayName: "R1Pro", Status: "offline", LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if err = st.SetProjectRobot(project.ID, "robot", true); err != nil {
		t.Fatal(err)
	}
	scene, err := st.GetProjectRealScene(project.ID)
	if err != nil || scene == nil || scene.PilotID != "" || scene.RobotID != "robot" {
		t.Fatalf("real onboarding incorrectly requires a map: %+v %v", scene, err)
	}
	if err = st.SetProjectRobot(project.ID, "robot", false); err != nil {
		t.Fatal(err)
	}
	if scene, err = st.GetProjectRealScene(project.ID); err != nil || scene != nil {
		t.Fatalf("removed robot retained a scene: %+v %v", scene, err)
	}
	if err = st.SavePilotEnrollment(PilotEnrollment{ID: "enrollment", Code: "123456", Status: "claimed", CreatedBy: "owner", PilotInstanceID: "pilot", CreatedAt: now, ClaimedAt: &now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err = st.SavePilotMapBinding(PilotMapBinding{ProjectID: project.ID, PilotID: "pilot", SyncedAt: now}); err != nil {
		t.Fatal(err)
	}
	tx, err := st.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DROP TABLE project_robot_bindings`); err != nil {
		t.Fatal(err)
	}
	if err = migrateV37(tx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	ids, err := st.ListProjectRobotIDs(project.ID)
	if err != nil || len(ids) != 1 || ids[0] != "robot" {
		t.Fatalf("existing Pilot map association not migrated: %+v %v", ids, err)
	}
}
