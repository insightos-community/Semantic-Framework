package workflow

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/event"
	interactionsvc "insightos.cn/semantic-framework/internal/interaction"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/log"
)

type heldRecoveryCall struct {
	ctx     context.Context
	request TaskRecoveryRequest
}

type heldRecoveryExecutor struct {
	*controlledExecutor
	calls chan heldRecoveryCall
	yield chan struct{}
}

func (e *heldRecoveryExecutor) RecoverTask(ctx context.Context, request TaskRecoveryRequest) (TaskRecoveryDecision, error) {
	e.calls <- heldRecoveryCall{ctx, request}
	select {
	case <-e.yield:
		return TaskRecoveryDecision{}, ErrTaskWaitingInput
	case <-ctx.Done():
		return TaskRecoveryDecision{}, ctx.Err()
	}
}

func receiveHeldRecovery(t *testing.T, e *heldRecoveryExecutor) heldRecoveryCall {
	t.Helper()
	select {
	case call := <-e.calls:
		return call
	case <-time.After(time.Second):
		t.Fatal("recovery did not start")
		return heldRecoveryCall{}
	}
}

func TestRecoveryAnswerSurvivesRequestEndAndEarlyReply(t *testing.T) {
	st := openWorkflowStore(t)
	project, conversation := createWorkflowProject(t, st)
	executor := &heldRecoveryExecutor{
		controlledExecutor: &controlledExecutor{started: make(chan TaskExecution, 1), release: make(chan struct{}, 1)},
		calls:              make(chan heldRecoveryCall, 3), yield: make(chan struct{}),
	}
	service, err := NewService(Deps{Store: st, Planner: &fakePlanner{}, Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	view, task, subs := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "sub-canceled-navigation", Kind: "robot_skill", Goal: "retry canceled navigation"}})
	execution := store.RobotExecution{ID: "rex-canceled-navigation", ProjectID: project.ID,
		WorkflowID: view.Workflow.ID, TaskID: task.ID, SubtaskID: subs[0].ID,
		RobotID: task.AssignedRobotID, SkillName: "navigate", SkillVersion: "1",
		Status: "failed", Error: map[string]any{"code": "navigation_cancelled"}}
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	requestCtx, endRequest := context.WithCancel(context.Background())
	defer endRequest()
	if err := service.OnRobotExecutionChanged(requestCtx, execution, "execution.failed", execution.Error); err != nil {
		t.Fatal(err)
	}
	first := receiveHeldRecovery(t, executor)
	endRequest()
	if first.ctx.Err() != nil {
		t.Fatalf("request lifetime canceled task recovery: %v", first.ctx.Err())
	}
	t.Cleanup(func() { service.cancelTaskAttempt(task.ID) })

	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
	interactions := interactionsvc.NewService(st, event.NewBus(logger), logger)
	interactions.SetAnswerRouter(service)
	service.answeredTaskResumer = interactions
	now := time.Now().UTC()
	run := store.RunSession{ID: "run-question-before-yield", ProjectID: project.ID,
		ChatSessionID: conversation.ID, ContextID: task.ContextID, WorkflowID: view.Workflow.ID,
		TaskID: task.ID, AgentID: task.AssignedAgentID, AgentName: task.AssignedAgentID,
		Kind: store.RunKindTaskExecution, Status: store.RunStatusRunning, StartedAt: now, UpdatedAt: now}
	if err := st.CreateRunSession(run); err != nil {
		t.Fatal(err)
	}
	current, _ := st.GetTask(task.ID)
	paused, err := service.PauseTaskForInteraction(project.OwnerID, project.ID, view.Workflow.ID, task.ID, current.Revision)
	if err != nil {
		t.Fatal(err)
	}
	answer := store.Interaction{ID: "int-early-retry-answer", ProjectID: project.ID,
		SessionID: conversation.ID, WorkflowID: view.Workflow.ID, TaskID: task.ID,
		RunID: run.ID, Agent: task.AssignedAgentID, Type: store.InteractionTypeInput,
		Status: store.InteractionStatusAnswered, Reply: `{"retry":true}`, SourceRevision: paused.Revision,
		CreatedAt: now, AnsweredAt: &now}
	if err := st.CreateInteraction(answer); err != nil {
		t.Fatal(err)
	}
	if err := service.RouteInteractionAnswer(context.Background(), answer); !errors.Is(err, ErrTaskBusy) {
		t.Fatalf("early answer should wait for the existing owner, got %v", err)
	}
	if _, err := st.FinishRunSession(run.ID, []string{store.RunStatusRunning}, store.RunStatusCompleted, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	executor.yield <- struct{}{}
	continued := receiveHeldRecovery(t, executor)
	if continued.request.Answer == nil || continued.request.Answer.ID != answer.ID || continued.request.Execution.ID != execution.ID {
		t.Fatalf("saved reply did not return to the same failed execution: %+v", continued.request)
	}
	service.cancelTaskAttempt(task.ID)
	select {
	case <-continued.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("explicit task cancellation must still cancel detached recovery")
	}
	old, _ := st.GetRobotExecution(execution.ID)
	if old.Status != "failed" {
		t.Fatal("recovery must not reset or replay the failed execution")
	}
}
