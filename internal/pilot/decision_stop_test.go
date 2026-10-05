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
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkerDecisionStopConvergesWithHoldAndDuplicateStop(t *testing.T) {
	python := os.Getenv("SEMANTIC_ROBOT_SKILL_PYTHON")
	sdk := os.Getenv("SEMANTIC_ROBOT_SKILLS_DIR")
	if python == "" || sdk == "" {
		t.Skip("set SEMANTIC_ROBOT_SKILL_PYTHON and SEMANTIC_ROBOT_SKILLS_DIR")
	}
	for _, afterAction := range []bool{false, true} {
		t.Run(map[bool]string{false: "waiting_decision", true: "decision_after_stopped_action"}[afterAction], func(t *testing.T) {
			directory, err := filepath.Abs("testdata/decision_stop")
			if err != nil {
				t.Fatal(err)
			}
			action := ActionRef{Type: "navigation.follow_route", SchemaVersion: 1}
			definition := SkillDefinition{Name: "decision-stop", Version: "test", Directory: directory,
				Runtime:         SkillRuntimeSpec{APIVersion: 1, Entrypoint: "fixture:run", StopEntrypoint: "fixture:on_stop", InputModel: "fixture:Input"},
				RequiredActions: []ActionRef{action}, StopActions: []ActionRef{action}}
			skills := &SkillCatalog{byName: map[string]SkillDefinition{definition.Name: definition}}
			catalog := NewCatalog(profile("r", binding(action.Type, "navigation-r1", true)))
			client := newControllableNavigationAbility()
			reported := make(chan AgentRequest, 2)
			gateway := NewRemoteAgentGateway(func(request AgentRequest) error { reported <- request; return nil })
			runtime := NewSkillRuntime(skills, catalog, NewRunner(catalog, client, NewMemoryActionJournal()),
				NewMemorySkillExecutionStore(), WorkerSupervisor{PythonExecutable: python, PythonPaths: []string{sdk}}, gateway, nil, nil)
			execution, err := runtime.Start(context.Background(), SkillStartRequest{RobotID: "r", SkillName: definition.Name,
				Version: definition.Version, Input: map[string]any{"wait_for_action": afterAction}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if a := runtime.getActive(execution.ID); a != nil {
					a.cancel()
					_ = a.currentWorker().Kill()
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if afterAction {
				select {
				case <-client.followStarted:
				case <-ctx.Done():
					t.Fatal("action did not start")
				}
			} else {
				select {
				case <-reported:
				case <-ctx.Done():
					t.Fatal("decision did not start")
				}
			}
			done := make(chan SkillExecution, 2)
			errs := make(chan error, 2)
			for range 2 {
				go func() {
					stopped, err := runtime.Stop(ctx, execution.ID, "user", "operator stop", "safe")
					done <- stopped
					errs <- err
				}()
			}
			for range 2 {
				stopped := <-done
				if err := <-errs; err != nil {
					t.Fatal(err)
				}
				if stopped.Status != SkillStopped || stopped.StopOutcome["safe"] != true {
					t.Fatalf("stop not confirmed: %+v", stopped)
				}
			}
			select {
			case <-reported:
				t.Fatal("late decision reached Agent after stop")
			default:
			}
			client.mu.Lock()
			starts := client.counts[action.Type]
			client.mu.Unlock()
			want := 1
			if afterAction {
				want = 2
			}
			if starts != want {
				t.Fatalf("hold replayed or action missing: %d starts, want %d", starts, want)
			}
		})
	}
}

func TestDecisionStopCancelsExistingAndLateRequests(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "waiting", true: "late"}[late], func(t *testing.T) {
			store := NewMemorySkillExecutionStore()
			execution := SkillExecution{ID: "e", RobotID: "r", Status: SkillRunning}
			if err := store.SaveSkillExecution(execution); err != nil {
				t.Fatal(err)
			}
			reported := make(chan AgentRequest, 1)
			gateway := NewRemoteAgentGateway(func(request AgentRequest) error { reported <- request; return nil })
			runtime := NewSkillRuntime(nil, nil, nil, store, WorkerSupervisor{}, gateway, nil, nil)
			active := &activeSkill{}
			stop := func() {
				active.stopDecisions()
				current, _ := store.GetSkillExecution(execution.ID)
				current.Status = SkillStopping
				if err := store.SaveSkillExecution(current); err != nil {
					t.Fatal(err)
				}
			}
			if late {
				stop()
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := runtime.requestAgent(ctx, execution, active, map[string]any{"key": "d", "context": map[string]any{"stage": "grasp"}})
				done <- err
			}()
			if !late {
				select {
				case <-reported:
				case <-ctx.Done():
					t.Fatal("decision not reported")
				}
				stop()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("unexpected decision result: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("decision blocks stop")
			}
			current, _ := store.GetSkillExecution(execution.ID)
			if current.Status != SkillStopping {
				t.Fatalf("late decision overwrote stop: %s", current.Status)
			}
			if late {
				select {
				case <-reported:
					t.Fatal("late request reached Agent")
				default:
				}
			}
		})
	}
}

func TestDecisionReplyStillResumesRunningExecution(t *testing.T) {
	store := NewMemorySkillExecutionStore()
	execution := SkillExecution{ID: "e", SkillName: "grasp-object", Status: SkillRunning}
	if err := store.SaveSkillExecution(execution); err != nil {
		t.Fatal(err)
	}
	var gateway *RemoteAgentGateway
	gateway = NewRemoteAgentGateway(func(request AgentRequest) error {
		return gateway.Resolve(AgentReply{ExecutionID: request.ExecutionID, SkillName: request.SkillName,
			Stage: request.Stage, DecisionKey: request.DecisionKey, DecisionRevision: request.DecisionRevision,
			Payload: map[string]any{"action": "abort_subtask"}})
	})
	runtime := NewSkillRuntime(nil, nil, nil, store, WorkerSupervisor{}, gateway, nil, nil)
	_, err := runtime.requestAgent(context.Background(), execution, &activeSkill{}, map[string]any{
		"key": "d", "context": map[string]any{"stage": "grasp", "plan_revision": 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	current, _ := store.GetSkillExecution(execution.ID)
	request, _ := store.GetAgentRequest(execution.ID, "d")
	if current.Status != SkillRunning || request.ResolvedAt == nil {
		t.Fatal("normal decision did not resume")
	}
}
