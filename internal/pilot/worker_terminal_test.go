package pilot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type interruptedMoveAbility struct{ *controllableNavigationAbility }

func (c *interruptedMoveAbility) GetExecution(ctx context.Context, instance, invocation string, after int64) (AbilityExecution, error) {
	c.mu.Lock()
	_, hold := c.tasks[invocation].Input["reason"]
	stopped := c.stopped[invocation]
	c.mu.Unlock()
	if !hold && !stopped {
		return AbilityExecution{Status: "interrupted"}, nil
	}
	return c.controllableNavigationAbility.GetExecution(ctx, instance, invocation, after)
}

func TestInterruptedSkillRetainsOwnerRejectsDuplicateAndStops(t *testing.T) {
	python, sdk := os.Getenv("SEMANTIC_ROBOT_SKILL_PYTHON"), os.Getenv("SEMANTIC_ROBOT_SKILLS_DIR")
	if python == "" || sdk == "" {
		t.Skip("requires Python Skill SDK")
	}
	directory, err := filepath.Abs("testdata/interrupted_stop")
	if err != nil {
		t.Fatal(err)
	}
	action := ActionRef{Type: "navigation.follow_route", SchemaVersion: 1}
	definition := SkillDefinition{Name: "interrupted-stop", Version: "test", Directory: directory,
		Runtime:         SkillRuntimeSpec{APIVersion: 1, Entrypoint: "fixture:run", StopEntrypoint: "fixture:on_stop", InputModel: "fixture:Input"},
		RequiredActions: []ActionRef{action}, StopActions: []ActionRef{action}}
	catalog := testCatalog()
	client := &interruptedMoveAbility{newControllableNavigationAbility()}
	runner := NewRunner(catalog, client)
	runtime := NewSkillRuntime(&SkillCatalog{byName: map[string]SkillDefinition{definition.Name: definition}}, catalog, runner,
		NewMemorySkillExecutionStore(), WorkerSupervisor{PythonExecutable: python, PythonPaths: []string{sdk}}, nil, nil, nil)
	request := SkillStartRequest{RobotID: "r1", SkillName: definition.Name, Version: definition.Version, Input: map[string]any{}}
	execution, err := runtime.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	defer runtime.cleanup(execution.ID, "r1")
	finished, err := runtime.Wait(ctx, execution.ID)
	if err != nil || finished.Status != SkillInterrupted {
		client.mu.Lock()
		t.Logf("Ability calls: %#v", client.counts)
		client.mu.Unlock()
		if action, e := runner.journal.GetAction(execution.ID, "move"); e == nil {
			t.Logf("Action: %#v", action)
		}
		t.Fatalf("物理状态未知必须保留 interrupted: %#v %v", finished, err)
	}
	if _, err := runtime.Start(ctx, request); !errors.Is(err, ErrRobotBusy) {
		t.Fatalf("重复请求应拒绝: %v", err)
	}
	stopped, err := runtime.Stop(ctx, execution.ID, "user", "retry stop", "immediate")
	if err != nil || stopped.Status != SkillStopped {
		t.Fatalf("原执行无法停止: %#v %v", stopped, err)
	}
	if runtime.RobotInUse("r1") || runner.physicalOwner("r1") != "" {
		t.Fatal("确认 hold 后未释放占用")
	}
}

func TestUnknownActionRemainsAttachedToSkillForStop(t *testing.T) {
	client := &fakeAbilityClient{startErr: errors.New("ack lost")}
	runner := NewRunner(testCatalog(), client)
	runtime := NewSkillRuntime(nil, testCatalog(), runner, NewMemorySkillExecutionStore(), WorkerSupervisor{}, nil, nil, nil)
	action := ActionRef{Type: "navigation.follow_route", SchemaVersion: 1}
	active := &activeSkill{definition: SkillDefinition{RequiredActions: []ActionRef{action}}, actions: map[string]string{}}
	execution := SkillExecution{ID: "skill-unknown", RobotID: "r1", Status: SkillRunning}
	_, err := runtime.startWorkerAction(context.Background(), execution, active, map[string]any{
		"key": "move", "action": map[string]any{"type": action.Type, "schema_version": 1},
	})
	if err == nil || active.actions["move"] == "" || !active.hasStartedPhysicalAction() {
		t.Fatalf("启动结果未知时必须保留停止引用: %#v %v", active.actions, err)
	}
	if runner.physicalOwner("r1") != active.actions["move"] {
		t.Fatal("Action 归属不一致")
	}
}

func TestWorkerCompleteIsNotForwardedBeforeExecutionConverges(t *testing.T) {
	store := NewMemorySkillExecutionStore()
	execution := SkillExecution{
		ID:      "skill-worker-complete",
		RobotID: "robot-worker-complete",
		Status:  SkillRunning,
	}
	if err := store.SaveSkillExecution(execution); err != nil {
		t.Fatal(err)
	}
	events := &recordingRuntimeEventSink{}
	runtime := NewSkillRuntime(nil, nil, nil, store, WorkerSupervisor{}, nil, nil, events)

	if _, err := runtime.handleWorker(context.Background(), execution.ID, nil, "complete",
		map[string]any{"status": "completed"}); err != nil {
		t.Fatal(err)
	}
	if len(events.events) != 0 {
		t.Fatalf("Worker complete must not be forwarded with stale execution state: %#v", events.events)
	}
}
