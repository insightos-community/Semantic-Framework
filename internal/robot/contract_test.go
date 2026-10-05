package robot

import (
	"context"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

func TestInstalledContractUsesExactPilotAndRejectsWrongIdentity(t *testing.T) {
	for _, wrongVersion := range []bool{false, true} {
		st := openRobotTestStore(t)
		service := NewService(st, nil)
		commands, disconnect, err := service.Connect(store.RobotPilot{PilotInstanceID: "contract-pilot", RobotID: "contract-robot"})
		if err != nil {
			t.Fatal(err)
		}
		defer disconnect()
		if err := st.SaveRobotPilotSkill(store.RobotPilotSkill{PilotInstanceID: "contract-pilot", Name: "generic-skill", Version: "2.0", Enabled: true, Status: "installed"}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := service.DescribeSkillInput(ctx, "contract-robot", "generic-skill", "2.0")
			done <- err
		}()
		command := receiveCommand(t, commands, "skill.describe_input")
		version := "2.0"
		if wrongVersion {
			version = "3.0"
		}
		if err := service.HandleCommandAckResult("contract-pilot", command.CommandID, true, "", map[string]any{"name": "generic-skill", "version": version, "input_schema": map[string]any{"type": "object", "required": []any{"target"}}}); err != nil {
			t.Fatal(err)
		}
		if err := <-done; (err != nil) != wrongVersion {
			t.Fatalf("wrongVersion=%v err=%v", wrongVersion, err)
		}
		// Contract discovery must never submit an execution.start command.
		select {
		case c := <-commands:
			if c.Type == "execution.start" {
				t.Fatal("contract lookup executed a Skill")
			}
		default:
		}
	}
}
