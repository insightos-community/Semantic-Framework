// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pilot

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type debugAbilityClient struct {
	mu         sync.Mutex
	starts     int
	gets       int
	stops      int
	getStatus  string
	startErr   error
	stopErr    error
	stopStatus string
}

func (c *debugAbilityClient) StartTask(_ context.Context, _, _ string, _ map[string]any) (AbilityTask, error) {
	c.mu.Lock()
	c.starts++
	c.mu.Unlock()
	return AbilityTask{TaskID: "framework-task-1"}, c.startErr
}
func (c *debugAbilityClient) GetExecution(_ context.Context, _, _ string, _ int64) (AbilityExecution, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	status := c.getStatus
	if status == "" {
		status = "running"
	}
	return AbilityExecution{Status: status}, nil
}
func (c *debugAbilityClient) StopExecution(_ context.Context, _, _, _ string) (AbilityExecution, error) {
	c.mu.Lock()
	c.stops++
	defer c.mu.Unlock()
	status := c.stopStatus
	if status == "" {
		status = "stopped"
	}
	return AbilityExecution{Status: status, Result: map[string]any{"hold_confirmed": status == "stopped"}}, c.stopErr
}
func (c *debugAbilityClient) counts() (int, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.starts, c.gets, c.stops
}

func TestAbilityDebugStartsExactTaskAndStopsWithEvidence(t *testing.T) {
	client := &debugAbilityClient{}
	store := NewMemorySkillExecutionStore()
	service := NewAbilityDebugService("r1pro-test", true, client, store)
	service.Interval = time.Hour
	execution, err := service.Start(context.Background(), AbilityDebugExecution{
		ID: "debug-1", RobotID: "r1pro-test", AbilityInstanceID: "end-effector-1",
		TaskName: "HoldObject", Input: map[string]any{"side": "left"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if execution.InvocationID != "debug-1" || execution.FrameworkTaskID != "framework-task-1" {
		t.Fatalf("unexpected execution: %#v", execution)
	}
	stopped, err := service.Stop(context.Background(), "debug-1", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Status != "stopped" || stopped.Result["hold_confirmed"] != true {
		t.Fatalf("missing stop evidence: %#v", stopped)
	}
	starts, _, stops := client.counts()
	if starts != 1 || stops != 1 {
		t.Fatalf("starts=%d stops=%d", starts, stops)
	}
}

func TestAbilityDebugRecoverQueriesWithoutReplayingStart(t *testing.T) {
	client := &debugAbilityClient{getStatus: "succeeded"}
	store := NewMemorySkillExecutionStore()
	now := time.Now().UTC()
	if err := store.SaveAbilityDebug(AbilityDebugExecution{ID: "debug-recover", RobotID: "r1pro-test",
		AbilityInstanceID: "state-1", TaskName: "GetRobotState", InvocationID: "debug-recover",
		Status: "running", StartedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	service := NewAbilityDebugService("r1pro-test", true, client, store)
	if err := service.Recover(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		item, err := store.GetAbilityDebug("debug-recover")
		if err != nil {
			t.Fatal(err)
		}
		if item.Status == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("debug did not recover: %#v", item)
		}
		time.Sleep(time.Millisecond)
	}
	starts, gets, _ := client.counts()
	if starts != 0 || gets == 0 {
		t.Fatalf("recover starts=%d gets=%d", starts, gets)
	}
}

func TestInterruptedAbilityDebugKeepsRobotLocked(t *testing.T) {
	store := NewMemorySkillExecutionStore()
	if err := store.SaveAbilityDebug(AbilityDebugExecution{ID: "debug-unknown", RobotID: "r1pro-test", Status: "interrupted"}); err != nil {
		t.Fatal(err)
	}
	service := NewAbilityDebugService("r1pro-test", true, &debugAbilityClient{}, store)
	_, err := service.Start(context.Background(), AbilityDebugExecution{ID: "debug-new",
		AbilityInstanceID: "state-1", TaskName: "GetRobotState"})
	if !errors.Is(err, ErrRobotBusy) {
		t.Fatalf("interrupted debug released robot: %v", err)
	}
}

func TestRejectedAbilityDebugReleasesRobot(t *testing.T) {
	client := &debugAbilityClient{startErr: &AbilityStartRejectedError{StatusCode: 422, Message: "invalid input"}}
	store := NewMemorySkillExecutionStore()
	service := NewAbilityDebugService("r1pro-test", true, client, store)
	first, err := service.Start(context.Background(), AbilityDebugExecution{
		ID: "debug-rejected", AbilityInstanceID: "motion-1", TaskName: "MoveEndEffector",
	})
	if err == nil || first.Status != "failed" || first.Error["code"] != "ABILITY_START_REJECTED" {
		t.Fatalf("明确拒绝必须收敛为 failed: %#v %v", first, err)
	}

	client.startErr = nil
	second, err := service.Start(context.Background(), AbilityDebugExecution{
		ID: "debug-after-rejected", AbilityInstanceID: "state-1", TaskName: "GetRobotState",
	})
	if err != nil {
		t.Fatalf("明确拒绝后 Robot 应可继续调试: %v", err)
	}
	if second.Status != "running" {
		t.Fatalf("后续调试未启动: %#v", second)
	}
}

func TestAbilityDebugReconcilesExplicitFailureAfterStartError(t *testing.T) {
	client := &debugAbilityClient{
		startErr:  errors.New("Ability Task failed"),
		getStatus: "failed",
	}
	store := NewMemorySkillExecutionStore()
	service := NewAbilityDebugService("r1pro-test", true, client, store)
	first, err := service.Start(context.Background(), AbilityDebugExecution{
		ID: "debug-stale-route", AbilityInstanceID: "navigation-1", TaskName: "FollowRoute",
	})
	if err == nil || first.Status != "failed" {
		t.Fatalf("可对账的失败必须收敛为 failed: %#v %v", first, err)
	}

	client.startErr = nil
	client.getStatus = "running"
	second, err := service.Start(context.Background(), AbilityDebugExecution{
		ID: "debug-after-stale-route", AbilityInstanceID: "state-1", TaskName: "GetRobotState",
	})
	if err != nil || second.Status != "running" {
		t.Fatalf("明确失败后 Robot 应可继续调试: %#v %v", second, err)
	}
}

func TestInterruptedDebugCanRetryStopWithoutReplayingAction(t *testing.T) {
	for _, uncertain := range []string{"transport_error", "running", "interrupted"} {
		t.Run(uncertain, func(t *testing.T) {
			store := NewMemorySkillExecutionStore()
			original := AbilityDebugExecution{ID: "debug-unknown-stop", RobotID: "r1pro-test", AbilityInstanceID: "motion-1", InvocationID: "debug-unknown-stop", TaskName: "MoveEndEffector", Status: "interrupted", Revision: 3, Error: map[string]any{"code": "STOP_UNCONFIRMED"}}
			if err := store.SaveAbilityDebug(original); err != nil {
				t.Fatal(err)
			}
			client := &debugAbilityClient{stopStatus: uncertain}
			if uncertain == "transport_error" {
				client.stopErr = errors.New("stop response lost")
			}
			service := NewAbilityDebugService("r1pro-test", true, client, store)
			first, _ := service.Stop(context.Background(), original.ID, "operator retry")
			if first.Status != "interrupted" {
				t.Fatalf("unconfirmed stop released ownership: %#v", first)
			}
			if _, err := service.Start(context.Background(), AbilityDebugExecution{ID: "read-before-confirm", AbilityInstanceID: "state-1", TaskName: "GetRobotState"}); !errors.Is(err, ErrRobotBusy) {
				t.Fatalf("lost ownership: %v", err)
			}
			client.stopErr = nil
			client.stopStatus = "stopped"
			recovered, err := service.Stop(context.Background(), original.ID, "operator retry after hold")
			if err != nil || recovered.Status != "stopped" || recovered.Result["hold_confirmed"] != true || recovered.Error != nil {
				t.Fatalf("stop recovery failed: %#v %v", recovered, err)
			}
			if recovered.InvocationID != original.InvocationID || recovered.AbilityInstanceID != original.AbilityInstanceID || recovered.Revision <= first.Revision {
				t.Fatalf("lost original invocation: %#v", recovered)
			}
			starts, _, stops := client.counts()
			if starts != 0 || stops != 2 {
				t.Fatalf("replayed action or skipped stop: starts=%d stops=%d", starts, stops)
			}
			if _, err = service.Stop(context.Background(), original.ID, "repeat confirmed stop"); err != nil {
				t.Fatal(err)
			}
			_, _, stops = client.counts()
			if stops != 2 {
				t.Fatal("confirmed stop was resent")
			}
			next, err := service.Start(context.Background(), AbilityDebugExecution{ID: "read-after-confirm", AbilityInstanceID: "state-1", TaskName: "GetRobotState"})
			if err != nil {
				t.Fatalf("confirmed stop still blocked: %v", err)
			}
			if _, err = service.Stop(context.Background(), next.ID, "test cleanup"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
