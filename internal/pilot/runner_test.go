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
	"testing"
	"time"
)

type fakeAbilityClient struct {
	startCalls int
	startedOn  string
	startedAs  string
	startedIn  map[string]any
	startErr   error
	getState   AbilityExecution
	getErr     error
	stopState  AbilityExecution
	stopErr    error
}

func (f *fakeAbilityClient) StartTask(_ context.Context, instanceID, taskName string, input map[string]any) (AbilityTask, error) {
	f.startCalls++
	f.startedOn = instanceID
	f.startedAs = taskName
	f.startedIn = cloneMap(input)
	return AbilityTask{TaskID: "task-1"}, f.startErr
}
func (f *fakeAbilityClient) GetExecution(_ context.Context, _, _ string, _ int64) (AbilityExecution, error) {
	return f.getState, f.getErr
}
func (f *fakeAbilityClient) StopExecution(_ context.Context, _, _ string, _ string) (AbilityExecution, error) {
	return f.stopState, f.stopErr
}

func testCatalog() *Catalog {
	return NewCatalog(profile("r1",
		binding("navigation.follow_route", "navigation-r1", true),
		binding("perception.locate_object", "perception-r1", false),
	))
}
func actionRequest(actionType, key string) ActionRequest {
	return ActionRequest{SkillExecutionID: "skill-1", Key: key, RobotID: "r1", Action: ActionRef{Type: actionType, SchemaVersion: 1}, Input: map[string]any{"target": "a"}}
}

func TestRunnerUsesStableKeyAndExactInstance(t *testing.T) {
	client := &fakeAbilityClient{getState: AbilityExecution{Status: "running"}}
	runner := NewRunner(testCatalog(), client)
	first, err := runner.StartAction(context.Background(), actionRequest("navigation.follow_route", "route:1"))
	if err != nil {
		t.Fatal(err)
	}
	if client.startedOn != "navigation-r1" || client.startedAs != "navigation.follow_route" {
		t.Fatalf("wrong route: %#v", client)
	}
	second, err := runner.StartAction(context.Background(), actionRequest("navigation.follow_route", "route:1"))
	if err != nil || first.ID != second.ID || client.startCalls != 1 {
		t.Fatalf("idempotency failed: %#v %#v %v", first, second, err)
	}
	changed := actionRequest("navigation.follow_route", "route:1")
	changed.Input["target"] = "b"
	if _, err := runner.StartAction(context.Background(), changed); !errors.Is(err, ErrActionKeyConflict) {
		t.Fatalf("expected key conflict: %v", err)
	}
}

func TestRunnerKeepsPhysicalLockWhenStartOrStopIsUnknown(t *testing.T) {
	client := &fakeAbilityClient{startErr: errors.New("lost")}
	runner := NewRunner(testCatalog(), client)
	request := actionRequest("navigation.follow_route", "route:1")
	first, err := runner.StartAction(context.Background(), request)
	if err == nil || first.Status != ActionInterrupted {
		t.Fatalf("expected interrupted: %#v %v", first, err)
	}
	if _, err := runner.StartAction(context.Background(), request); !errors.Is(err, ErrReplayForbidden) {
		t.Fatalf("must not replay: %v", err)
	}
	other := request
	other.Key = "route:2"
	if _, err := runner.StartAction(context.Background(), other); !errors.Is(err, ErrRobotBusy) {
		t.Fatalf("lock must remain: %v", err)
	}
	if _, err := runner.ConfirmInterrupted(first.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerReleasesPhysicalLockAfterConfirmedStartRejection(t *testing.T) {
	client := &fakeAbilityClient{startErr: &AbilityStartRejectedError{StatusCode: 422, Message: "invalid input"}}
	runner := NewRunner(testCatalog(), client)
	first, err := runner.StartAction(context.Background(), actionRequest("navigation.follow_route", "route:rejected"))
	if err == nil || first.Status != ActionFailed || first.Error["code"] != "ABILITY_START_REJECTED" {
		t.Fatalf("明确拒绝必须收敛为 failed: %#v %v", first, err)
	}

	client.startErr = nil
	second, err := runner.StartAction(context.Background(), actionRequest("navigation.follow_route", "route:next"))
	if err != nil {
		t.Fatalf("明确拒绝后物理锁未释放: %v", err)
	}
	if second.Status != ActionRunning {
		t.Fatalf("后续 Action 未启动: %#v", second)
	}
}

func TestRunnerStopsInterruptedActionWithoutReplaying(t *testing.T) {
	client := &fakeAbilityClient{startErr: errors.New("lost acknowledgement"), stopErr: errors.New("offline")}
	runner := NewRunner(testCatalog(), client)
	first, _ := runner.StartAction(context.Background(), actionRequest("navigation.follow_route", "unknown"))
	if _, err := runner.Stop(context.Background(), first.ID, "user"); err == nil {
		t.Fatal("停止未确认必须报告错误并保留锁")
	}
	if runner.physicalOwner("r1") != first.ID {
		t.Fatal("未确认时丢失原执行")
	}
	client.stopErr = nil
	client.stopState = AbilityExecution{Status: "stopped"}
	stopped, err := runner.Stop(context.Background(), first.ID, "retry")
	if err != nil || stopped.Status != ActionStopped || runner.physicalOwner("r1") != "" {
		t.Fatalf("重试停止没有释放已确认的物理锁: %#v %v", stopped, err)
	}
	if client.startCalls != 1 {
		t.Fatal("停止不允许重放动作")
	}
}

func TestRunnerReconcilesExplicitFailedExecutionAfterStartError(t *testing.T) {
	client := &fakeAbilityClient{
		startErr: errors.New("Ability Task failed"),
		getState: AbilityExecution{Status: "failed", Error: map[string]any{
			"code": "ROUTE_GENERATION_STALE", "message": "路线 generation 已失效",
		}},
	}
	runner := NewRunner(testCatalog(), client)
	first, err := runner.StartAction(context.Background(), actionRequest("navigation.follow_route", "route:stale"))
	if err == nil || first.Status != ActionFailed || first.Error["code"] != "ROUTE_GENERATION_STALE" {
		t.Fatalf("可对账的失败必须收敛为 failed: %#v %v", first, err)
	}

	client.startErr = nil
	client.getState = AbilityExecution{Status: "running"}
	second, err := runner.StartAction(context.Background(), actionRequest("navigation.follow_route", "route:fresh"))
	if err != nil || second.Status != ActionRunning {
		t.Fatalf("明确失败后 Robot 锁未释放: %#v %v", second, err)
	}
}

func TestRunnerFeedbackCursorAndConfirmedStop(t *testing.T) {
	client := &fakeAbilityClient{}
	runner := NewRunner(testCatalog(), client)
	execution, err := runner.StartAction(context.Background(), actionRequest("navigation.follow_route", "route:1"))
	if err != nil {
		t.Fatal(err)
	}
	client.getState = AbilityExecution{Status: "running", Feedback: []AbilityFeedback{{Sequence: 1, Message: "start"}, {Sequence: 2, Message: "move"}}}
	items, terminal, err := runner.Feedback(context.Background(), execution.ID, 0)
	if err != nil || terminal || len(items) != 2 {
		t.Fatalf("feedback: %#v %v %v", items, terminal, err)
	}
	client.stopState = AbilityExecution{Status: "stopped", Feedback: []AbilityFeedback{{Sequence: 3, Message: "hold"}}}
	stopped, err := runner.Stop(context.Background(), execution.ID, "user")
	if err != nil || stopped.Status != ActionStopped {
		t.Fatalf("stop: %#v %v", stopped, err)
	}
	if _, err := runner.StartAction(context.Background(), actionRequest("navigation.follow_route", "route:2")); err != nil {
		t.Fatalf("confirmed stop should release: %v", err)
	}
}

func TestRunnerThrottlesProgressUsingRequestedFeedbackInterval(t *testing.T) {
	client := &fakeAbilityClient{}
	runner := NewRunner(testCatalog(), client)
	now := time.Unix(100, 0)
	runner.now = func() time.Time { return now }
	request := actionRequest("navigation.follow_route", "route:feedback")
	request.FeedbackInterval = 200 * time.Millisecond
	execution, err := runner.StartAction(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if interval := runner.actionPollInterval(execution.ID); interval != 200*time.Millisecond {
		t.Fatalf("Action状态轮询应沿用请求间隔且普通进度不超过5Hz: %v", interval)
	}

	client.getState = AbilityExecution{Status: "running", Feedback: []AbilityFeedback{
		{Sequence: 1, Status: "running", Phase: "navigate", Progress: 0.1, Message: "start"},
		{Sequence: 2, Status: "running", Phase: "navigate", Progress: 0.2, Message: "moving"},
	}}
	items, _, err := runner.Feedback(context.Background(), execution.ID, 0)
	if err != nil || len(items) != 1 || items[0].Sequence != 1 {
		t.Fatalf("首次反馈应立即保存、相邻进度应合并: %#v %v", items, err)
	}

	now = now.Add(50 * time.Millisecond)
	client.getState = AbilityExecution{Status: "running"}
	items, _, err = runner.Feedback(context.Background(), execution.ID, 1)
	if err != nil || len(items) != 0 {
		t.Fatalf("请求间隔内不应发送普通进度: %#v %v", items, err)
	}

	now = now.Add(150 * time.Millisecond)
	items, _, err = runner.Feedback(context.Background(), execution.ID, 1)
	if err != nil || len(items) != 1 || items[0].Sequence != 2 {
		t.Fatalf("达到请求间隔后应发送最新合并进度: %#v %v", items, err)
	}

	client.getState = AbilityExecution{Status: "running", Feedback: []AbilityFeedback{
		{Sequence: 3, Status: "running", Phase: "verify", Progress: 0.3, Message: "phase changed"},
	}}
	items, _, err = runner.Feedback(context.Background(), execution.ID, 2)
	if err != nil || len(items) != 1 || items[0].Sequence != 3 {
		t.Fatalf("阶段变化必须立即发送: %#v %v", items, err)
	}
}
