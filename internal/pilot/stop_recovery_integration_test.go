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

type controllableNavigationAbility struct {
	mu             sync.Mutex
	tasks          map[string]DepalletizingMockInvocation
	counts         map[string]int
	followStarted  chan struct{}
	completeFollow bool
	stopped        map[string]bool
}

func newControllableNavigationAbility() *controllableNavigationAbility {
	return &controllableNavigationAbility{tasks: map[string]DepalletizingMockInvocation{}, counts: map[string]int{}, followStarted: make(chan struct{}, 1), stopped: map[string]bool{}}
}
func (f *controllableNavigationAbility) StartTask(_ context.Context, _ string, task string, input map[string]any) (AbilityTask, error) {
	inv := stringValue(input["invocation_id"])
	f.mu.Lock()
	f.counts[task]++
	f.tasks[inv] = DepalletizingMockInvocation{Task: task, Input: cloneMap(input), Ordinal: f.counts[task]}
	f.mu.Unlock()
	if task == "navigation.follow_route" {
		select {
		case f.followStarted <- struct{}{}:
		default:
		}
	}
	return AbilityTask{TaskID: "task-" + inv}, nil
}
func (f *controllableNavigationAbility) GetExecution(_ context.Context, _ string, inv string, _ int64) (AbilityExecution, error) {
	f.mu.Lock()
	item := f.tasks[inv]
	complete := f.completeFollow
	stopped := f.stopped[inv]
	f.mu.Unlock()
	if stopped {
		return AbilityExecution{Status: "stopped", Result: map[string]any{"safe": true, "physical_state": "base_stopped_and_braked"}}, nil
	}
	switch item.Task {
	case "navigation.plan_route":
		return AbilityExecution{Status: "succeeded", Result: map[string]any{"route_ref": "route://blocking/1"}}, nil
	case "navigation.follow_route":
		if _, isStop := item.Input["reason"]; isStop {
			return AbilityExecution{Status: "succeeded", Result: map[string]any{"safe": true, "physical_state": "base_stopped_and_braked"}}, nil
		}
		if complete {
			return AbilityExecution{Status: "succeeded", Result: map[string]any{"final_pose_ref": "pose://final", "distance_to_target_m": .1}}, nil
		}
		return AbilityExecution{Status: "running"}, nil
	case "navigation.verify_arrival":
		return AbilityExecution{Status: "succeeded", Result: map[string]any{"verdict": "achieved", "final_pose_ref": "pose://final", "distance_to_target_m": .1}}, nil
	}
	return AbilityExecution{Status: "interrupted"}, nil
}
func (f *controllableNavigationAbility) StopExecution(_ context.Context, _ string, inv string, _ string) (AbilityExecution, error) {
	f.mu.Lock()
	f.stopped[inv] = true
	f.mu.Unlock()
	return AbilityExecution{Status: "stopped", Result: map[string]any{"safe": true, "physical_state": "base_stopped_and_braked"}}, nil
}

func navigationRuntime(t *testing.T, client AbilityClient) (*SkillRuntime, *controllableNavigationAbility) {
	t.Helper()
	repository := os.Getenv("SEMANTIC_ROBOT_SKILLS_DIR")
	if repository == "" {
		t.Skip("set SEMANTIC_ROBOT_SKILLS_DIR")
	}
	skills, err := ScanSkillCatalog(filepath.Join(repository, "semantic_robot_skills", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	bindings := []AbilityBinding{binding("navigation.plan_route", "navigation-r1", false), binding("navigation.follow_route", "navigation-r1", true), binding("navigation.verify_arrival", "navigation-r1", false)}
	catalog := NewCatalog(profile("robot://r1pro/fake-1", bindings...))
	runner := NewRunner(catalog, client, NewMemoryActionJournal())
	runtime := NewSkillRuntime(skills, catalog, runner, NewMemorySkillExecutionStore(), WorkerSupervisor{PythonExecutable: "python", PythonPaths: []string{repository}}, nil, nil, nil)
	typed, _ := client.(*controllableNavigationAbility)
	return runtime, typed
}
func startBlockingNavigation(t *testing.T, runtime *SkillRuntime) SkillExecution {
	t.Helper()
	input := map[string]any{"target": map[string]any{"target_ref": "semantic://target", "pose": MockPose(1, 2, 0, "r1"), "map_type": "simulation", "map_generation": "g1", "map_revision": 1}}
	execution, err := runtime.Start(context.Background(), SkillStartRequest{RobotID: "robot://r1pro/fake-1", SkillName: "semantic-navigation", Version: "0.1.0", Input: input})
	if err != nil {
		t.Fatal(err)
	}
	return execution
}

func cloneMapString(source map[string]string) map[string]string {
	result := map[string]string{}
	for k, v := range source {
		result[k] = v
	}
	return result
}
func cloneMapInt(source map[string]int) map[string]int {
	result := map[string]int{}
	for k, v := range source {
		result[k] = v
	}
	return result
}

func TestRuntimeStopReachesAbilityAndWorkerAndReturnsEvidence(t *testing.T) {
	client := newControllableNavigationAbility()
	runtime, _ := navigationRuntime(t, client)
	execution := startBlockingNavigation(t, runtime)
	select {
	case <-client.followStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("follow action did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stopped, err := runtime.Stop(ctx, execution.ID, "user", "operator stop", "immediate")
	if err != nil {
		active := runtime.getActive(execution.ID)
		active.mu.Lock()
		actions := cloneMapString(active.actions)
		active.mu.Unlock()
		client.mu.Lock()
		counts := cloneMapInt(client.counts)
		client.mu.Unlock()
		t.Fatalf("stop failed: %v actions=%v counts=%v execution=%#v", err, actions, counts, stopped)
	}
	if stopped.Status != SkillStopped {
		t.Fatalf("unexpected stop: %#v", stopped)
	}
	if safe, _ := stopped.StopOutcome["safe"].(bool); !safe {
		t.Fatalf("missing stop evidence: %#v", stopped.StopOutcome)
	}
}

func TestRuntimeConcurrentStopReturnsSameTerminalEvidence(t *testing.T) {
	client := newControllableNavigationAbility()
	runtime, _ := navigationRuntime(t, client)
	execution := startBlockingNavigation(t, runtime)
	select {
	case <-client.followStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("follow action did not start")
	}

	// 同时停止用于模拟 HTTP 重试与实例退出并发。两个调用必须复用
	// 同一次物理停止结果，不能因 Worker 已清理而回退为 interrupted。
	start := make(chan struct{})
	type result struct {
		execution SkillExecution
		err       error
	}
	results := make(chan result, 2)
	for index := 0; index < 2; index++ {
		go func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			stopped, err := runtime.Stop(ctx, execution.ID, "runtime", "duplicate stop", "safe")
			results <- result{execution: stopped, err: err}
		}()
	}
	close(start)
	for index := 0; index < 2; index++ {
		result := <-results
		if result.err != nil || result.execution.Status != SkillStopped {
			t.Fatalf("并发停止没有复用终态: execution=%#v err=%v", result.execution, result.err)
		}
		if safe, _ := result.execution.StopOutcome["safe"].(bool); !safe {
			t.Fatalf("并发停止缺少物理证据: %#v", result.execution.StopOutcome)
		}
	}
}

func TestWorkerCrashRecoversByQueryingOriginalInvocation(t *testing.T) {
	client := newControllableNavigationAbility()
	runtime, _ := navigationRuntime(t, client)
	execution := startBlockingNavigation(t, runtime)
	select {
	case <-client.followStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("follow action did not start")
	}
	active := runtime.getActive(execution.ID)
	active.mu.Lock()
	worker := active.worker
	active.mu.Unlock()
	if err := worker.Kill(); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	client.completeFollow = true
	client.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finished, err := runtime.Wait(ctx, execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != SkillCompleted {
		t.Fatalf("recovery failed: %#v", finished)
	}
	client.mu.Lock()
	starts := client.counts["navigation.follow_route"]
	client.mu.Unlock()
	if starts != 1 {
		t.Fatalf("physical action replayed %d times", starts)
	}
}
