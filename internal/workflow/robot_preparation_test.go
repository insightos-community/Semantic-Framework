package workflow

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

type preparationExecutor struct {
	started chan TaskExecution
	results chan error
}

func TestCancelledPreparationWhileWorkflowPausedPreservesAndResumesSuffix(t *testing.T) {
	st, project, conversation, svc, _, _ := newWorkflowFixture(t)
	view, task, steps := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "cancelled-preparation", Kind: "robot_skill", Goal: "prepare input"},
			{ID: "untouched-dependent", Kind: "robot_skill", Goal: "later action", DependsOn: []string{"cancelled-preparation"}}})
	paused, err := st.TransitionSubTask(steps[0].ID, steps[0].Revision, store.TaskStatusPaused, "server_restarted", nil, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ResumePausedAgentStep(paused.ID, paused.Revision, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	executor := &preparationExecutor{make(chan TaskExecution, 8), make(chan error, 8)}
	svc.executor = executor
	if !svc.reserveTask(task) {
		t.Fatal("reserve")
	}
	if err = svc.launchRunningTask(context.Background(), project.OwnerID, view.Workflow, task); err != nil {
		t.Fatal(err)
	}
	var started TaskExecution
	select {
	case started = <-executor.started:
	case <-time.After(workflowEventWait):
		t.Fatal("start timeout")
	}
	latest, _ := st.GetWorkflow(view.Workflow.ID)
	if _, err = svc.PauseWorkflow(context.Background(), project.OwnerID, project.ID, latest.ID, latest.Revision); err != nil {
		t.Fatal(err)
	}
	executor.results <- context.Canceled
	deadline := time.Now().Add(workflowEventWait)
	for {
		run, _ := st.GetRunSession(started.Run.ID)
		if run.Status == store.RunStatusCancelled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancel timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
	first, _ := st.GetSubTask(steps[0].ID)
	later, _ := st.GetSubTask(steps[1].ID)
	if first.Status != store.TaskStatusPaused || first.ExecutionRef != "" || later.Status != store.TaskStatusPending {
		t.Fatalf("model cancellation destroyed approved suffix: %+v %+v", first, later)
	}
	latest, _ = st.GetWorkflow(view.Workflow.ID)
	if _, err = svc.ResumeWorkflow(context.Background(), project.OwnerID, project.ID, latest.ID, latest.Revision); err != nil {
		t.Fatal(err)
	}
	select {
	case resumed := <-executor.started:
		if len(resumed.SubTasks) != 1 || resumed.SubTasks[0].ID != first.ID {
			t.Fatalf("wrong resumed step: %+v", resumed.SubTasks)
		}
	case <-time.After(workflowEventWait):
		t.Fatal("resume timeout")
	}
	svc.Shutdown()
	executor.results <- context.Canceled
}

func TestShutdownDuringPreparationPreservesPendingStepsForRestart(t *testing.T) {
	st, project, conversation, svc, _, _ := newWorkflowFixture(t)
	view, task, steps := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "prepare-before-restart", Kind: "robot_skill", Goal: "prepare input"},
			{ID: "later-action", Kind: "robot_skill", Goal: "later action", DependsOn: []string{"prepare-before-restart"}}})
	paused, err := st.TransitionSubTask(steps[0].ID, steps[0].Revision, store.TaskStatusPaused, "server_restarted", nil, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ResumePausedAgentStep(paused.ID, paused.Revision, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	executor := &preparationExecutor{make(chan TaskExecution, 8), make(chan error, 8)}
	svc.executor = executor
	if !svc.reserveTask(task) {
		t.Fatal("reserve")
	}
	if err := svc.launchRunningTask(context.Background(), project.OwnerID, view.Workflow, task); err != nil {
		t.Fatal(err)
	}
	var started TaskExecution
	select {
	case started = <-executor.started:
	case <-time.After(workflowEventWait):
		t.Fatal("start timeout")
	}
	svc.Shutdown()
	executor.results <- context.Canceled
	deadline := time.Now().Add(workflowEventWait)
	for {
		run, err := st.GetRunSession(started.Run.ID)
		if err == nil && run.Status == store.RunStatusCancelled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancel timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
	before, _ := st.GetSubTask(steps[0].ID)
	later, _ := st.GetSubTask(steps[1].ID)
	if before.Status != store.TaskStatusRunning || later.Status != store.TaskStatusPending {
		t.Fatalf("Server shutdown must not terminate approved work: %+v %+v", before, later)
	}
	restarted, err := NewService(Deps{Store: st, Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.RecoverInterruptedWorkflows(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	after, _ := st.GetSubTask(steps[0].ID)
	later, _ = st.GetSubTask(steps[1].ID)
	if after.Status != store.TaskStatusPaused || after.WaitingReason != "server_restarted" ||
		after.ExecutionRef != "" || later.Status != store.TaskStatusPending {
		t.Fatalf("restart lost the exact unstarted step: %+v %+v", after, later)
	}
}

func (e *preparationExecutor) ExecuteTask(ctx context.Context, value TaskExecution) (TaskResult, error) {
	e.started <- value
	select {
	case err := <-e.results:
		return TaskResult{}, err
	case <-ctx.Done():
		return TaskResult{}, ctx.Err()
	}
}

func TestDisconnectedPreparationWaitsAndResumesExactStepOnce(t *testing.T) {
	st, project, conversation, svc, _, _ := newWorkflowFixture(t)
	view, task, steps := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "read-state", Kind: "robot_skill", Goal: "读取新鲜状态"},
			{ID: "next-step", Kind: "robot_skill", Goal: "后续动作", DependsOn: []string{"read-state"}}})
	paused, err := st.TransitionSubTask(steps[0].ID, steps[0].Revision, store.TaskStatusPaused, "server_restarted", nil, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ResumePausedAgentStep(paused.ID, paused.Revision, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	availability := &testRobotAvailability{}
	svc.robotAvailability = availability
	executor := &preparationExecutor{make(chan TaskExecution, 8), make(chan error, 8)}
	svc.executor = executor
	if !svc.reserveTask(task) {
		t.Fatal("reserve")
	}
	if err := svc.launchRunningTask(context.Background(), project.OwnerID, view.Workflow, task); err != nil {
		t.Fatal(err)
	}
	select {
	case <-executor.started:
	case <-time.After(workflowEventWait):
		t.Fatal("start timeout")
	}
	executor.results <- ErrTaskWaitingResource
	waitForPreparation(t, st, task.ID, store.TaskStatusPaused)
	step, _ := st.GetSubTask(steps[0].ID)
	later, _ := st.GetSubTask(steps[1].ID)
	wf, _ := st.GetWorkflow(view.Workflow.ID)
	if step.WaitingReason != "waiting_resource" || step.ExecutionRef != "" ||
		later.Status != store.TaskStatusPending || wf.Status != store.WorkflowStatusRunning {
		t.Fatalf("disconnect terminated or skipped work: %+v %+v %+v", step, later, wf)
	}
	svc.OnRobotAvailabilityChanged(context.Background(), "another-robot")
	select {
	case <-executor.started:
		t.Fatal("unrelated robot resumed step")
	default:
	}
	availability.available.Store(true)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); svc.OnRobotAvailabilityChanged(context.Background(), task.AssignedRobotID) }()
	}
	wg.Wait()
	select {
	case resumed := <-executor.started:
		if resumed.SubTasks[0].ID != steps[0].ID {
			t.Fatalf("wrong step: %+v", resumed.SubTasks)
		}
	case <-time.After(workflowEventWait):
		t.Fatal("resume timeout")
	}
	select {
	case <-executor.started:
		t.Fatal("duplicate resume")
	default:
	}
	// Deterministic contract/business errors still fail; they do not auto-loop.
	executor.results <- errors.New("invalid contract")
	waitWorkflowStatus(t, st, view.Workflow.ID, store.WorkflowStatusFailed)
}

func waitForPreparation(t *testing.T, st *store.Store, taskID, status string) {
	t.Helper()
	deadline := time.Now().Add(workflowEventWait)
	for time.Now().Before(deadline) {
		current, _ := st.GetTask(taskID)
		if current.Status == status {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("preparation status timeout", status)
}

func TestResourceWaitCannotReplayAdmittedExecution(t *testing.T) {
	for _, attached := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_attach", true: "attached"}[attached], func(t *testing.T) {
			st, project, conversation, svc, _, _ := newWorkflowFixture(t)
			_, task, steps := prepareRunningRobotTask(t, st, project, conversation,
				[]store.SubTaskDraft{{ID: "physical-step", Kind: "robot_skill", Goal: "现有执行"}})
			now := time.Now().UTC()
			step := steps[0]
			if err := st.SaveRobotExecution(store.RobotExecution{ID: "rex-existing", ProjectID: project.ID,
				RobotID: task.AssignedRobotID, TaskID: task.ID, SubtaskID: step.ID, RequestKey: "existing", Status: "running", Revision: 1,
				CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			if attached {
				if _, err := st.AttachSubTaskExecution(step.ID, step.Revision, "rex-existing", now); err != nil {
					t.Fatal(err)
				}
			}
			if err := svc.pauseRobotPreparation(task, ErrTaskWaitingResource, nil); !errors.Is(err, store.ErrInvalidState) {
				t.Fatalf("admitted execution paused: %v", err)
			}
			unchanged, _ := st.GetTask(task.ID)
			if unchanged.Status != store.TaskStatusRunning {
				t.Fatal("task mutated", unchanged)
			}
		})
	}
}

func TestPreparationResumeAfterRestartPreservesAnsweredInput(t *testing.T) {
	st, project, conversation, svc, _, _ := newWorkflowFixture(t)
	view, task, _ := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "query-after-answer", Kind: "robot_skill", Goal: "按已回答的选择读取状态"}})
	answer := store.Interaction{ID: "answer-before-disconnect", ProjectID: project.ID, SessionID: conversation.ID,
		WorkflowID: view.Workflow.ID, TaskID: task.ID, Status: store.InteractionStatusAnswered,
		Type: store.InteractionTypeInput, Reply: `{"planning_basis":"scene-atlas"}`, Payload: `{}`, CreatedAt: time.Now().UTC()}
	if err := st.CreateInteraction(answer); err != nil {
		t.Fatal(err)
	}
	if err := svc.pauseRobotPreparation(task, ErrTaskWaitingResource, &answer); err != nil {
		t.Fatal(err)
	}
	availability := &testRobotAvailability{}
	availability.available.Store(true)
	executor := &preparationExecutor{make(chan TaskExecution, 8), make(chan error, 8)}
	restarted, err := NewService(Deps{Store: st, Executor: executor, RobotAvailability: availability})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.RecoverInterruptedWorkflows(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	restarted.OnRobotAvailabilityChanged(context.Background(), task.AssignedRobotID)
	select {
	case resumed := <-executor.started:
		if resumed.Answer == nil || resumed.Answer.ID != answer.ID || resumed.Answer.Reply != answer.Reply {
			t.Fatalf("lost answer: %+v", resumed.Answer)
		}
	case <-time.After(workflowEventWait):
		t.Fatal("restart resume timeout")
	}
	executor.results <- errors.New("end test")
	waitWorkflowStatus(t, st, view.Workflow.ID, store.WorkflowStatusFailed)
}

func TestStoppedWorkflowDoesNotResumePreparation(t *testing.T) {
	st, project, conversation, svc, _, executor := newWorkflowFixture(t)
	view, task, _ := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "query-before-stop", Kind: "robot_skill", Goal: "读取状态"}})
	if err := svc.pauseRobotPreparation(task, ErrTaskWaitingResource, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StopWorkflow(context.Background(), project.OwnerID, project.ID, view.Workflow.ID, view.Workflow.Revision); err != nil {
		t.Fatal(err)
	}
	availability := &testRobotAvailability{}
	availability.available.Store(true)
	svc.robotAvailability = availability
	svc.OnRobotAvailabilityChanged(context.Background(), task.AssignedRobotID)
	select {
	case <-executor.started:
		t.Fatal("user-stopped workflow resumed")
	default:
	}
	waitWorkflowStatus(t, st, view.Workflow.ID, store.WorkflowStatusStopped)
}
