package robot

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"insightos.cn/semantic-framework/internal/store"
)

func TestBoundInputsReachOriginalValidatorAndRemainIdempotent(t *testing.T) {
	st := openRobotTestStore(t)
	project, err := st.CreateProject("binding-user", "binding")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, nil)
	commands, disconnect, err := svc.Connect(store.RobotPilot{
		PilotInstanceID: "pilot-binding", RobotID: "robot-binding", RobotStatus: "idle", AbilityFrameworkStatus: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	if err := st.SaveRobotPilotSkill(store.RobotPilotSkill{PilotInstanceID: "pilot-binding", Name: "plan", Version: "1", Status: "installed", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	flags := strings.Repeat("1", 80)
	source := store.RobotExecution{ID: "source", ProjectID: project.ID, RobotID: "robot-binding", Status: "completed",
		Result: map[string]any{"payload": map[string]any{"config": map[string]any{"flags": []any{flags}, "nx": float64(10), "ny": float64(8)}, "q": []any{.1, .2, .3, .4}}}}
	if err := st.SaveRobotExecution(source); err != nil {
		t.Fatal(err)
	}
	request := RunRequest{ProjectID: project.ID, RobotID: "robot-binding", SkillName: "plan", SkillVersion: "1", RequestKey: "bound-input",
		Input: map[string]any{"task": "plan", "parameters": map[string]any{"kind": "target-handoff"}},
		InputBindings: []store.RobotInputBinding{
			{InputPath: "/parameters/parameters/config", ExecutionID: "source", ResultPath: "/payload/config"},
			{InputPath: "/parameters/parameters/start_torso", ExecutionID: "source", ResultPath: "/payload/q"},
		}}
	type outcome struct {
		execution store.RobotExecution
		err       error
	}
	done := make(chan outcome, 1)
	go func() { e, err := svc.Run(context.Background(), request); done <- outcome{e, err} }()
	validation := receiveCommand(t, commands, "skill.validate_input")
	input := validation.Payload.(map[string]any)["input"].(map[string]any)
	parameters := input["parameters"].(map[string]any)["parameters"].(map[string]any)
	if !reflect.DeepEqual(parameters["config"], source.Result["payload"].(map[string]any)["config"]) ||
		!reflect.DeepEqual(parameters["start_torso"], source.Result["payload"].(map[string]any)["q"]) {
		t.Fatal("source data changed", parameters)
	}
	if _, exists := request.Input["parameters"].(map[string]any)["parameters"]; exists {
		t.Fatal("caller input mutated")
	}
	if err := svc.HandleCommandAckResult("pilot-binding", validation.CommandID, true, "", map[string]any{"valid": true, "input": input}); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	start := receiveCommand(t, commands, "execution.start")
	execution := start.Payload.(map[string]any)["execution"].(store.RobotExecution)
	if !reflect.DeepEqual(execution.Input, input) || !reflect.DeepEqual(execution.InputBindings, request.InputBindings) {
		t.Fatal("binding provenance or resolved input lost")
	}
	duplicate, err := svc.Run(context.Background(), request)
	if err != nil || duplicate.ID != execution.ID {
		t.Fatal("bound retry is not idempotent", err)
	}
	source.ID = "same-values-other-source"
	source.RequestKey = source.ID
	if err := st.SaveRobotExecution(source); err != nil {
		t.Fatal(err)
	}
	changed := request
	changed.InputBindings = append([]store.RobotInputBinding(nil), request.InputBindings...)
	changed.InputBindings[0].ExecutionID = source.ID
	if _, err := svc.Run(context.Background(), changed); !errors.Is(err, ErrRequestConflict) {
		t.Fatal("source substitution did not change request identity", err)
	}
	select {
	case command := <-commands:
		t.Fatal("retry issued a command", command)
	default:
	}
}

func TestInputBindingRejectsWrongScopeAndUnavailableValues(t *testing.T) {
	st := openRobotTestStore(t)
	svc := NewService(st, nil)
	for _, tc := range []struct{ name, project, robot, status, path string }{
		{"project", "other", "r", "completed", "/payload"},
		{"robot", "p", "other", "completed", "/payload"},
		{"inflight", "p", "r", "running", "/payload"},
		{"failed", "p", "r", "failed", "/payload"},
		{"missing", "p", "r", "completed", "/missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := st.SaveRobotExecution(store.RobotExecution{ID: tc.name, RequestKey: tc.name, ProjectID: tc.project, RobotID: tc.robot, Status: tc.status, Result: map[string]any{"payload": 1}}); err != nil {
				t.Fatal(err)
			}
			_, err := svc.resolveInputBindings(RunRequest{ProjectID: "p", RobotID: "r", Input: map[string]any{}, InputBindings: []store.RobotInputBinding{{InputPath: "/parameters/x", ExecutionID: tc.name, ResultPath: tc.path}}})
			if err == nil {
				t.Fatal("invalid source accepted")
			}
		})
	}
}
