package robot

import (
	"testing"

	"insightos.cn/semantic-framework/internal/store"
)

func TestContinuationAvailabilityRequiresLiveReadyPilot(t *testing.T) {
	st := openRobotTestStore(t)
	service := NewService(st, nil)
	pilot := store.RobotPilot{PilotInstanceID: "pilot-resume", RobotID: "robot-resume",
		RobotStatus: "idle", AbilityFrameworkStatus: "ready", Backend: "mujoco"}
	_, disconnect, err := service.Connect(pilot)
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	if !service.IsRobotAvailable(pilot.RobotID) {
		t.Fatal("connected ready Robot unavailable")
	}
	// The new Server sees the same database but no reconnected WebSocket yet.
	restarted := NewService(st, nil)
	if restarted.IsRobotAvailable(pilot.RobotID) {
		t.Fatal("persisted online status cannot authorize a continuation")
	}
	pilot.RobotStatus = "busy"
	if err := service.Heartbeat(pilot.PilotInstanceID, pilot); err != nil {
		t.Fatal(err)
	}
	if service.IsRobotAvailable(pilot.RobotID) {
		t.Fatal("busy Robot should wait for existing availability event")
	}
	pilot.RobotStatus = "idle"
	pilot.AbilityFrameworkStatus = "unavailable"
	if err := service.Heartbeat(pilot.PilotInstanceID, pilot); err != nil {
		t.Fatal(err)
	}
	if service.IsRobotAvailable(pilot.RobotID) {
		t.Fatal("Ability Framework is not ready")
	}
}
