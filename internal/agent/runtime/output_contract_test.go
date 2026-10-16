package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/workflow"
)

func testCheckpointEvent() map[string]any {
	var event map[string]any
	_ = json.Unmarshal([]byte(`{"context":{"plan_revision":7},"response_schema":{"type":"object","additionalProperties":false,"required":["choice","expected_plan_revision"],"properties":{"choice":{"enum":["continue","abort"]},"expected_plan_revision":{"type":"integer"}}}}`), &event)
	return event
}

func TestCheckpointContractRejectsBeforePublishing(t *testing.T) {
	for _, text := range []string{`{}`, `null`, `[]`, `{"choice":"invented","expected_plan_revision":7}`,
		`{"choice":"continue","expected_plan_revision":8}`, `{"choice":"continue","expected_plan_revision":7,"python":"x"}`} {
		t.Run(text, func(t *testing.T) {
			accepted := false
			review, err := checkpointReviewer(testCheckpointEvent(), func(map[string]any) { accepted = true })
			if err != nil {
				t.Fatal(err)
			}
			if review(text) == nil || accepted {
				t.Fatal("invalid decision escaped")
			}
		})
	}
	for _, event := range []map[string]any{{}, {"response_schema": map[string]any{"$ref": "https://example.invalid/schema"}}} {
		if _, err := checkpointReviewer(event, func(map[string]any) {}); err == nil {
			t.Fatal("broken developer contract accepted")
		}
	}
}

func TestCheckpointContractModelCorrectionAndBoundedFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replies []kernel.MockReply
		calls   int
		success bool
	}{
		{"valid", []kernel.MockReply{{Content: `{"choice":"continue","expected_plan_revision":7}`}}, 1, true},
		{"corrected", []kernel.MockReply{{Content: `{"choice":"continue"}`}, {Content: `{"choice":"continue","expected_plan_revision":7}`}}, 2, true},
		{"repeated", []kernel.MockReply{{Content: `{"choice":"continue"}`}, {Content: `{"choice":"continue"}`}, {Content: `{"choice":"continue"}`}}, 3, false},
		{"second correction succeeds", []kernel.MockReply{{Content: `{"choice":"continue"}`}, {Content: `{"choice":"continue"}`}, {Content: `{"choice":"continue","expected_plan_revision":7}`}}, 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newTestFixture(t)
			model := kernel.NewMockChatModel()
			model.SetScript(tc.replies...)
			svc := newWorkflowRuntimeForTest(t, fx, model)
			project, err := fx.st.EnsureDefaultProject("usr-checkpoint-contract")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			conversation := store.ChatSession{ID: "cs-checkpoint-contract", UserID: project.OwnerID, ProjectID: project.ID, Title: "contract", CreatedAt: now, UpdatedAt: now}
			if err := fx.st.CreateChatSession(conversation); err != nil {
				t.Fatal(err)
			}
			view := approveRuntimeWorkflow(t, fx.st, project, conversation, store.WorkflowDraft{Goal: "generic decision", Tasks: []store.TaskDraft{{ID: "task-checkpoint", RequiredRole: "developer", Goal: "generic"}}}, now)
			task := view.Tasks[0]
			decision, err := svc.ResolveRobotAgentRequest(context.Background(), workflow.RobotAgentDecisionRequest{
				UserID: project.OwnerID, Project: project, Conversation: conversation, Workflow: view.Workflow, Task: task,
				SubTask:   store.SubTask{ID: "sub-checkpoint", TaskID: task.ID, Goal: "generic"},
				Execution: store.RobotExecution{ID: "rex-checkpoint", SkillName: "unrelated-robot-skill"}, Event: testCheckpointEvent(),
			})
			if (err == nil) != tc.success || len(model.CallInputs()) != tc.calls {
				t.Fatalf("decision=%v err=%v calls=%d", decision, err, len(model.CallInputs()))
			}
			if tc.success && decision["choice"] != "continue" {
				t.Fatal("valid decision changed")
			}
		})
	}
}

func TestWorkerOutcomeContract(t *testing.T) {
	for _, text := range []string{`{"kind":"result","summary":"ok","evidence":[]}`, `{"kind":"result","summary":"ok"}`} {
		if _, err := validateTaskOutcome(text); err != nil {
			t.Fatal(err)
		}
	}
	for _, text := range []string{`{"kind":"result","summary":""}`, `{"kind":"other","summary":"ok"}`, `{"kind":"result","summary":"ok","evidence":123}`} {
		if _, err := validateTaskOutcome(text); err == nil {
			t.Fatal("malformed outcome accepted")
		}
	}
}

func TestRecoveryUsesBoundedCorrectionWithoutPhysicalExecution(t *testing.T) {
	fx := newTestFixture(t)
	model := kernel.NewMockChatModel()
	model.SetScript(kernel.MockReply{Content: `{"decision":"revise_pending","summary":"retry","replacements":[]}`},
		kernel.MockReply{Content: `{"decision":"fail_task","summary":"cannot recover safely","replacements":[]}`})
	svc := newWorkflowRuntimeForTest(t, fx, model)
	project, err := fx.st.EnsureDefaultProject("usr-recovery-contract")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	conversation := store.ChatSession{ID: "cs-recovery-contract", UserID: project.OwnerID, ProjectID: project.ID, Title: "recovery", CreatedAt: now, UpdatedAt: now}
	if err := fx.st.CreateChatSession(conversation); err != nil {
		t.Fatal(err)
	}
	view := approveRuntimeWorkflow(t, fx.st, project, conversation, store.WorkflowDraft{Goal: "recover", Tasks: []store.TaskDraft{{ID: "task-recovery", RequiredRole: "developer", Goal: "generic"}}}, now)
	task := view.Tasks[0]
	if _, err := fx.st.SaveProjectMemory(project.OwnerID, project.ID,
		"RECOVERY_REUSABLE_METHOD: inspect fresh state before replanning", 0); err != nil {
		t.Fatal(err)
	}
	if err := fx.st.AppendContextMessage(store.ContextMessage{
		ID: store.NewChatMessageID(), ContextID: task.ContextID, ProjectID: project.ID,
		SessionID: conversation.ID, TaskID: task.ID, AgentID: task.AssignedAgentID,
		Message: schema.UserMessage("OLD_EXECUTION_PACKET"), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.st.SaveContextSummary(store.ContextSummary{
		ContextID: task.ContextID, ProjectID: project.ID, SessionID: conversation.ID,
		TaskID: task.ID, Summary: "OLD_TASK_STATE_SUMMARY",
	}, 0); err != nil {
		t.Fatal(err)
	}
	text, err := svc.runTaskAgent(context.Background(), project.OwnerID, project, conversation, view.Workflow, "", &task, task.AssignedAgentID,
		buildTaskRecoveryPrompt([]byte(`{}`)), nil, nil, store.RunKindTaskExecution, runtimePurposeTaskRecovery, func(text string) error {
			_, err := decodeTaskRecoveryDecision(text, task, robotTaskPlanningView{})
			return err
		})
	if err != nil || len(model.CallInputs()) != 2 {
		t.Fatalf("err=%v calls=%d", err, len(model.CallInputs()))
	}
	// Recovery may summarize its user context while loading long execution
	// records. Its response schema must also remain in the system instruction.
	var persistent string
	for _, message := range model.CallInputs()[0] {
		if message.Role == schema.System {
			persistent += message.Content
		}
	}
	if !strings.Contains(persistent, taskRecoveryInstruction) ||
		!strings.Contains(persistent, robotResultLookupInstruction) {
		t.Fatal("recovery contract and result lookup must survive context summarization")
	}
	if !strings.Contains(persistent, "RECOVERY_REUSABLE_METHOD") {
		t.Fatal("recovery lost Project Memory with the excluded execution history")
	}
	for _, call := range model.CallInputs() {
		for _, message := range call {
			if strings.Contains(message.Content, "OLD_EXECUTION_PACKET") ||
				strings.Contains(message.Content, "OLD_TASK_STATE_SUMMARY") {
				t.Fatal("recovery reloaded stale execution history")
			}
		}
	}
	decision, err := decodeTaskRecoveryDecision(text, task, robotTaskPlanningView{})
	if err != nil || decision.Decision != "fail_task" {
		t.Fatalf("decision=%v err=%v", decision, err)
	}
}

func TestCheckpointAllowedDecisionsSubset(t *testing.T) {
	event := testCheckpointEvent()
	event["context"].(map[string]any)["allowed_decisions"] = []string{"abort"}
	review, err := checkpointReviewer(event, func(map[string]any) {})
	if err != nil {
		t.Fatal(err)
	}
	if review(`{"choice":"continue","expected_plan_revision":7}`) == nil {
		t.Fatal("state-inapplicable decision escaped")
	}
	if err := review(`{"choice":"abort","expected_plan_revision":7}`); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkCheckpointContractValidation(b *testing.B) {
	review, err := checkpointReviewer(testCheckpointEvent(), func(map[string]any) {})
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := review(`{"choice":"continue","expected_plan_revision":7}`); err != nil {
			b.Fatal(err)
		}
	}
}
