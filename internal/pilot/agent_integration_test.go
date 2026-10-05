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
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type failingLocateAbility struct{}

func (failingLocateAbility) StartTask(_ context.Context, _ string, _ string, input map[string]any) (AbilityTask, error) {
	return AbilityTask{TaskID: "task-" + stringValue(input["invocation_id"])}, nil
}
func (failingLocateAbility) GetExecution(context.Context, string, string, int64) (AbilityExecution, error) {
	return AbilityExecution{Status: "failed", Error: map[string]any{"code": "TARGET_NOT_FOUND", "message": "target is absent"}}, nil
}
func (failingLocateAbility) StopExecution(context.Context, string, string, string) (AbilityExecution, error) {
	return AbilityExecution{Status: "stopped"}, nil
}

type recordingAgent struct {
	mu       sync.Mutex
	requests []AgentRequest
}

func (a *recordingAgent) Request(_ context.Context, request AgentRequest) (AgentReply, error) {
	a.mu.Lock()
	a.requests = append(a.requests, request)
	a.mu.Unlock()
	return AgentReply{ExecutionID: request.ExecutionID, SkillName: request.SkillName, Stage: request.Stage, DecisionKey: request.DecisionKey, DecisionRevision: request.DecisionRevision, Payload: map[string]any{"expected_plan_revision": request.DecisionRevision, "action": "abort_subtask", "reason": "目标已离开当前区域"}}, nil
}

func TestRecoveryBudgetCreatesTypedPersistentAgentRequest(t *testing.T) {
	repository := os.Getenv("SEMANTIC_ROBOT_SKILLS_DIR")
	if repository == "" {
		t.Skip("set SEMANTIC_ROBOT_SKILLS_DIR")
	}
	skills, err := ScanSkillCatalog(filepath.Join(repository, "semantic_robot_skills", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	robotID := "robot://r1pro/fake-1"
	catalog := NewCatalog(profile(robotID, DepalletizingMockBindings()...))
	journal := NewMemoryActionJournal()
	runner := NewRunner(catalog, failingLocateAbility{}, journal)
	store := NewMemorySkillExecutionStore()
	agent := &recordingAgent{}
	runtime := NewSkillRuntime(skills, catalog, runner, store, WorkerSupervisor{PythonExecutable: "python", PythonPaths: []string{repository}}, agent, nil, nil)
	started, err := runtime.Start(context.Background(), SkillStartRequest{RobotID: robotID, SkillName: "grasp-object", Version: "0.1.0", Input: map[string]any{"object_ref": "object://missing", "maximum_observation_attempts": 1}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finished, err := runtime.Wait(ctx, started.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != SkillFailed || finished.Error["code"] != "AGENT_ABORTED_GRASP" {
		t.Fatalf("unexpected terminal: %#v", finished)
	}
	agent.mu.Lock()
	requests := append([]AgentRequest(nil), agent.requests...)
	agent.mu.Unlock()
	if len(requests) != 1 {
		t.Fatalf("agent requests: %#v", requests)
	}
	saved, err := store.GetAgentRequest(started.ID, requests[0].DecisionKey)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Response["action"] != "abort_subtask" || saved.ResolvedAt == nil {
		t.Fatalf("reply was not persisted: %#v", saved)
	}
}
