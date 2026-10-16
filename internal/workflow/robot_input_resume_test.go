package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/event"
	interactionsvc "insightos.cn/semantic-framework/internal/interaction"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/log"
)

type testRobotAvailability struct{ available atomic.Bool }

func (r *testRobotAvailability) IsRobotAvailable(string) bool { return r.available.Load() }

func TestRobotPreExecutionAnswerStartsExactlyOneContinuation(t *testing.T) {
	for _, scenario := range []string{"reply", "persisted_answer_after_restart", "answer_waits_for_pilot", "answer_while_workflow_paused"} {
		t.Run(scenario, func(t *testing.T) {
			st, project, conversation, service, _, executor := newWorkflowFixture(t)
			view, task, subs := prepareRunningRobotTask(t, st, project, conversation,
				[]store.SubTaskDraft{{ID: "sub-query", Kind: "robot_skill", Goal: "查询规划依据"}})
			now := time.Now().UTC()
			run := store.RunSession{ID: "run-question", ProjectID: project.ID,
				ChatSessionID: conversation.ID, WorkflowID: view.Workflow.ID, TaskID: task.ID,
				AgentID: task.AssignedAgentID, AgentName: task.AssignedAgentID, ContextID: task.ContextID,
				Kind: store.RunKindTaskExecution, Status: store.RunStatusCompleted, StartedAt: now, UpdatedAt: now}
			if err := st.CreateRunSession(run); err != nil {
				t.Fatal(err)
			}
			paused, err := service.PauseTaskForInteraction(project.OwnerID, project.ID,
				view.Workflow.ID, task.ID, task.Revision)
			if err != nil {
				t.Fatal(err)
			}
			logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
			interactions := interactionsvc.NewService(st, event.NewBus(logger), logger)
			interactions.SetAnswerRouter(service)
			availability := &testRobotAvailability{}
			availability.available.Store(scenario != "answer_waits_for_pilot")
			service.robotAvailability = availability
			service.answeredTaskResumer = interactions
			question, err := interactions.CreateStructured(context.Background(), interactionsvc.StructuredRequest{
				ProjectID: project.ID, SessionID: conversation.ID, WorkflowID: view.Workflow.ID,
				TaskID: task.ID, RunID: run.ID, SourceAgentID: task.AssignedAgentID,
				SourceRevision: paused.Revision, Type: store.InteractionTypeInput,
				UIKind: store.InteractionUIForm, Prompt: "选择已有规划依据", ResponseSchema: json.RawMessage(`{"type":"object"}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			answer := json.RawMessage(`{"planning_basis":"run_scene_atlas"}`)
			if scenario != "reply" {
				if err := st.AnswerInteractionAtRevision(question.ID, string(answer), paused.Revision, now); err != nil {
					t.Fatal(err)
				}
				if err := st.RecordInteractionHandlingError(question.ID, "状态或状态迁移非法"); err != nil {
					t.Fatal(err)
				}
				if scenario == "answer_while_workflow_paused" {
					current, _ := st.GetWorkflow(view.Workflow.ID)
					pausedView, pauseErr := service.PauseWorkflow(context.Background(), project.OwnerID,
						project.ID, current.ID, current.Revision)
					if pauseErr != nil {
						t.Fatal(pauseErr)
					}
					if err := interactions.RecoverAnsweredInteractions(context.Background()); !errors.Is(err, store.ErrInvalidState) {
						t.Fatalf("a saved reply must not bypass an explicit Workflow pause: %v", err)
					}
					if _, err := service.ResumeWorkflow(context.Background(), project.OwnerID, project.ID,
						current.ID, pausedView.Workflow.Revision); err != nil {
						t.Fatal(err)
					}
				}
				if scenario != "answer_while_workflow_paused" {
					if err := service.RecoverInterruptedWorkflows(now); err != nil {
						t.Fatal(err)
					}
					err = interactions.RecoverAnsweredInteractions(context.Background())
				}
				if scenario == "answer_waits_for_pilot" {
					if !errors.Is(err, ErrTaskWaitingResource) {
						t.Fatalf("offline answer should wait: %v", err)
					}
					current, _ := st.GetTask(task.ID)
					step, _ := st.GetSubTask(subs[0].ID)
					saved, _ := st.GetInteraction(question.ID)
					if current.Revision != paused.Revision || current.Status != store.TaskStatusPaused ||
						step.Status != store.TaskStatusPaused || saved.HandledAt != nil {
						t.Fatalf("offline recovery consumed answer or changed task: %+v %+v %+v", current, step, saved)
					}
					if _, runErr := st.GetRunSessionBySourceInteraction(question.ID); !errors.Is(runErr, store.ErrNotFound) {
						t.Fatalf("offline recovery created a run: %v", runErr)
					}
					availability.available.Store(true)
					service.OnRobotAvailabilityChanged(context.Background(), "unrelated-robot")
					select {
					case <-executor.started:
						t.Fatal("unrelated Robot resumed this Task")
					default:
					}
					var notifications sync.WaitGroup
					for i := 0; i < 8; i++ {
						notifications.Add(1)
						go func() {
							defer notifications.Done()
							service.OnRobotAvailabilityChanged(context.Background(), task.AssignedRobotID)
						}()
					}
					notifications.Wait()
					err = nil
				}
			} else {
				err = interactions.ReplyStructured(context.Background(), project.OwnerID, project.ID, question.ID, answer, paused.Revision)
			}
			if err != nil {
				t.Fatal(err)
			}
			continued := receiveExecution(t, executor)
			defer func() { executor.release <- struct{}{} }()
			if continued.Answer == nil || continued.Answer.Reply != string(answer) ||
				continued.Run.SourceInteractionID != question.ID || continued.SubTasks[0].ID != subs[0].ID {
				t.Fatalf("answer was not routed to its exact unstarted step: %+v", continued)
			}
			handled, _ := st.GetInteraction(question.ID)
			if handled.HandledAt == nil || handled.HandlingError != "" {
				t.Fatalf("answer remains unhandled: %+v", handled)
			}
			if err := service.RouteInteractionAnswer(context.Background(), handled); err != nil {
				t.Fatal(err)
			}
			select {
			case duplicate := <-executor.started:
				t.Fatalf("duplicate continuation: %+v", duplicate)
			default:
			}
		})
	}
}

func TestRobotInputResumeNeverReplaysExistingExecution(t *testing.T) {
	for _, attached := range []bool{false, true} {
		t.Run(map[bool]string{false: "persisted_before_attach", true: "attached"}[attached], func(t *testing.T) {
			st, project, conversation, service, _, _ := newWorkflowFixture(t)
			view, task, subs := prepareRunningRobotTask(t, st, project, conversation,
				[]store.SubTaskDraft{{ID: "sub-existing", Kind: "robot_skill", Goal: "已有执行"}})
			now := time.Now().UTC()
			if err := st.SaveRobotExecution(store.RobotExecution{ID: "rex-existing", ProjectID: project.ID,
				WorkflowID: view.Workflow.ID, TaskID: task.ID, SubtaskID: subs[0].ID,
				RobotID: task.AssignedRobotID, SkillName: "test", SkillVersion: "1", RequestKey: "existing",
				Status: "running", Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			if attached {
				if _, err := st.AttachSubTaskExecution(subs[0].ID, subs[0].Revision, "rex-existing", now); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := service.PauseTaskForInteraction(project.OwnerID, project.ID, view.Workflow.ID, task.ID, task.Revision); err != nil {
				t.Fatal(err)
			}
			paused, _ := st.GetSubTask(subs[0].ID)
			if _, err := st.ResumePausedAgentStep(paused.ID, paused.Revision, now); !errors.Is(err, store.ErrInvalidState) {
				t.Fatalf("existing execution must be reconciled instead of replayed: %v", err)
			}
			unchanged, _ := st.GetSubTask(paused.ID)
			if unchanged.Revision != paused.Revision || unchanged.Status != store.TaskStatusPaused {
				t.Fatalf("mutated rejected step: %+v", unchanged)
			}
		})
	}
}

func TestExplicitResumePreservesAnswerAndInterruptedAttempt(t *testing.T) {
	st, project, conversation, service, _, executor := newWorkflowFixture(t)
	view, task, _ := prepareRunningRobotTask(t, st, project, conversation,
		[]store.SubTaskDraft{{ID: "sub-query-retry", Kind: "robot_skill", Goal: "查询场景"}})
	now := time.Now().UTC()
	answer := store.Interaction{ID: "int-explicit-retry", ProjectID: project.ID,
		SessionID: conversation.ID, WorkflowID: view.Workflow.ID, TaskID: task.ID,
		Agent: task.AssignedAgentID, Type: store.InteractionTypeInput, UIKind: store.InteractionUIForm,
		Status: store.InteractionStatusPending, Payload: `{}`, ResponseSchema: `{}`,
		SourceRevision: task.Revision, CreatedAt: now}
	if err := st.CreateInteraction(answer); err != nil {
		t.Fatal(err)
	}
	if err := st.AnswerInteractionAtRevision(answer.ID, `{"planning_basis":"run_scene_atlas"}`, task.Revision, now); err != nil {
		t.Fatal(err)
	}
	old := store.RunSession{ID: "run-interrupted-answer", ProjectID: project.ID,
		ChatSessionID: conversation.ID, WorkflowID: view.Workflow.ID, TaskID: task.ID,
		SourceInteractionID: answer.ID, Kind: store.RunKindTaskExecution,
		ContextID: task.ContextID, AgentID: task.AssignedAgentID, AgentName: task.AssignedAgentID,
		Status: store.RunStatusRunning, StartedAt: now}
	if err := st.CreateRunSession(old); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.MarkInterruptedRuns(now); err != nil {
		t.Fatal(err)
	}
	if err := service.RecoverInterruptedWorkflows(now); err != nil {
		t.Fatal(err)
	}
	paused, _ := st.GetWorkflow(view.Workflow.ID)
	availability := &testRobotAvailability{}
	service.robotAvailability = availability
	if _, err := service.ResumeWorkflow(context.Background(), project.OwnerID, project.ID, paused.ID, paused.Revision); !errors.Is(err, ErrTaskWaitingResource) {
		t.Fatalf("explicit offline resume should wait: %v", err)
	}
	paused, _ = st.GetWorkflow(view.Workflow.ID)
	if paused.Reason != "server_restarted" {
		t.Fatalf("offline attempt lost explicit retry route: %+v", paused)
	}
	availability.available.Store(true)
	if _, err := service.ResumeWorkflow(context.Background(), project.OwnerID, project.ID, paused.ID, paused.Revision); err != nil {
		t.Fatal(err)
	}
	continued := receiveExecution(t, executor)
	defer func() { executor.release <- struct{}{} }()
	if continued.Answer == nil || continued.Answer.ID != answer.ID ||
		continued.Run.ID == old.ID || continued.Run.SourceInteractionID != answer.ID {
		t.Fatalf("explicit retry lost answer or reused old Run: %+v", continued)
	}
	previous, _ := st.GetRunSession(old.ID)
	if previous.SourceInteractionID != answer.ID || previous.Status == store.RunStatusRunning {
		t.Fatalf("interrupted attempt history changed: %+v", previous)
	}
	handled, _ := st.GetInteraction(answer.ID)
	if handled.HandledAt == nil {
		t.Fatal("explicit resume did not consume saved answer")
	}
}

func TestAnsweredQuestionRepairsOnlyErroneouslyStoppedUnstartedDependents(t *testing.T) {
	for _, scenario := range []string{"failed_after_question", "completed_run", "user_stop", "admitted_execution"} {
		t.Run(scenario, func(t *testing.T) {
			st, project, conversation, service, _, _ := newWorkflowFixture(t)
			view, task, subs := prepareRunningRobotTask(t, st, project, conversation, []store.SubTaskDraft{
				{ID: "question", Kind: "robot_skill", Goal: "query"},
				{ID: "next", Kind: "robot_skill", Goal: "navigate", DependsOn: []string{"question"}},
			})
			now := time.Now().UTC()
			paused, err := service.PauseTaskForInteraction(project.OwnerID, project.ID, view.Workflow.ID, task.ID, task.Revision)
			if err != nil {
				t.Fatal(err)
			}
			runStatus := store.RunStatusFailed
			if scenario == "completed_run" {
				runStatus = store.RunStatusCompleted
			}
			run := store.RunSession{ID: "run-question-error", ProjectID: project.ID, ChatSessionID: conversation.ID,
				WorkflowID: view.Workflow.ID, TaskID: task.ID, AgentID: task.AssignedAgentID, AgentName: task.AssignedAgentID,
				ContextID: task.ContextID, Kind: store.RunKindTaskExecution, Status: runStatus, StartedAt: now, UpdatedAt: now}
			if err = st.CreateRunSession(run); err != nil {
				t.Fatal(err)
			}
			reason := "task_failed"
			if scenario == "user_stop" {
				reason = "user_stopped"
			}
			if _, err = st.TransitionSubTask(subs[1].ID, subs[1].Revision, store.TaskStatusStopped, reason, nil, now); err != nil {
				t.Fatal(err)
			}
			if scenario == "admitted_execution" {
				if err = st.SaveRobotExecution(store.RobotExecution{ID: "rex-next", ProjectID: project.ID,
					WorkflowID: view.Workflow.ID, TaskID: task.ID, SubtaskID: subs[1].ID, RobotID: "r1pro-test",
					RequestKey: "once", SkillName: "navigate", SkillVersion: "1", Status: "completed"}); err != nil {
					t.Fatal(err)
				}
			}
			if err = st.CreateInteraction(store.Interaction{ID: "int-question", ProjectID: project.ID, SessionID: conversation.ID,
				WorkflowID: view.Workflow.ID, TaskID: task.ID, RunID: run.ID, Agent: task.AssignedAgentID,
				Type: store.InteractionTypeInput, UIKind: store.InteractionUIForm, Status: store.InteractionStatusAnswered,
				Payload: `{}`, ResponseSchema: `{}`, Reply: `{"tolerance":0.05}`, SourceRevision: paused.Revision, CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
			if err = st.RestoreQuestionDependents(task.ID, run.ID, paused.Revision, now); err != nil {
				t.Fatal(err)
			}
			next, _ := st.GetSubTask(subs[1].ID)
			want := store.TaskStatusStopped
			if scenario == "failed_after_question" {
				want = store.TaskStatusPending
			}
			if next.Status != want {
				t.Fatalf("%s: %+v", scenario, next)
			}
		})
	}
}

type taskExecutorFunc func(context.Context, TaskExecution) (TaskResult, error)

func (f taskExecutorFunc) ExecuteTask(ctx context.Context, e TaskExecution) (TaskResult, error) {
	return f(ctx, e)
}

func TestModelErrorAfterQuestionPreservesPendingWork(t *testing.T) {
	st, project, conversation, service, _, _ := newWorkflowFixture(t)
	view, task, subs := prepareRunningRobotTask(t, st, project, conversation, []store.SubTaskDraft{
		{ID: "question", Kind: "robot_skill", Goal: "query"},
		{ID: "next", Kind: "robot_skill", Goal: "navigate", DependsOn: []string{"question"}},
	})
	paused, err := service.PauseTaskForInteraction(project.OwnerID, project.ID, view.Workflow.ID, task.ID, task.Revision)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := st.GetSubTask(subs[0].ID)
	if _, err = st.ResumePausedAgentStep(first.ID, first.Revision, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	task, err = st.TransitionTask(task.ID, paused.Revision, store.TaskStatusRunning, "", "", nil, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	run := store.RunSession{ID: "run-after-ask", ProjectID: project.ID, ChatSessionID: conversation.ID, WorkflowID: view.Workflow.ID,
		TaskID: task.ID, AgentID: task.AssignedAgentID, AgentName: task.AssignedAgentID, ContextID: task.ContextID,
		Kind: store.RunKindTaskExecution, Status: store.RunStatusRunning, StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err = st.CreateRunSession(run); err != nil {
		t.Fatal(err)
	}
	service.executor = taskExecutorFunc(func(_ context.Context, e TaskExecution) (TaskResult, error) {
		_, err := service.PauseTaskForInteraction(project.OwnerID, project.ID, view.Workflow.ID, e.Task.ID, e.Task.Revision)
		if err != nil {
			return TaskResult{}, err
		}
		return TaskResult{}, errors.New("exceeds max iterations")
	})
	service.executeTask(context.Background(), project.OwnerID, view.Workflow, task, nil, run)
	current, _ := st.GetTask(task.ID)
	next, _ := st.GetSubTask(subs[1].ID)
	finished, _ := st.GetRunSession(run.ID)
	if current.Status != store.TaskStatusPaused || current.WaitingReason != "waiting_input" || next.Status != store.TaskStatusPending || finished.Status != store.RunStatusFailed {
		t.Fatalf("question/run diagnostic/dependents must stay distinct: %+v %+v %+v", current, next, finished)
	}
}
