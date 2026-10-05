package runtime

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/workflow"
)

// State restrictions must reach the same output gate for any Skill, independently
// of its name or robot. A schema-valid but state-invalid answer is not accepted.
func TestCheckpointStateRestrictionMatrix(t *testing.T) {
	for _, selector := range []string{"decision", "choice", "action"} {
		for _, key := range []string{"allowed_decisions", "allowed_actions"} {
			if key == "allowed_actions" && selector != "action" {
				continue
			}
			t.Run(key+"/"+selector, func(t *testing.T) {
				event := map[string]any{
					"context": map[string]any{key: []string{"stop"}},
					"response_schema": map[string]any{"type": "object", "required": []string{selector}, "additionalProperties": false,
						"properties": map[string]any{selector: map[string]any{"type": "string", "enum": []string{"continue", "stop"}}}},
				}
				count := 0
				review, err := checkpointReviewer(event, func(map[string]any) { count++ })
				if err != nil {
					t.Fatal(err)
				}
				// Caller mutation cannot relax the contract of an already-started run.
				event["context"].(map[string]any)[key] = []string{"continue"}
				for _, text := range []string{`{}`, `[]`, `null`, `{"` + selector + `":123}`, `{"` + selector + `":"continue"}`, `{"` + selector + `":"stop","extra":1}`, `{"` + selector + `":"stop"} {}`} {
					if review(text) == nil || count != 0 {
						t.Fatalf("invalid escaped: %s", text)
					}
				}
				if err := review(`{"` + selector + `":"stop"}`); err != nil || count != 1 {
					t.Fatalf("valid rejected: %v", err)
				}
			})
		}
	}
}

func TestPlanningGraphCorrectionWithinSameRun(t *testing.T) {
	fx := newTestFixture(t)
	m := kernel.NewMockChatModel()
	m.SetScript(kernel.MockReply{Content: `{"subtasks":[{"id":"a","kind":"agent_step","goal":"first","depends_on":["b"]},{"id":"b","kind":"agent_step","goal":"second","depends_on":["a"]}]}`},
		kernel.MockReply{Content: `{"subtasks":[{"id":"a","kind":"agent_step","goal":"first"},{"id":"b","kind":"agent_step","goal":"second","depends_on":["a"]}]}`})
	svc := newWorkflowRuntimeForTest(t, fx, m)
	project, err := fx.st.EnsureDefaultProject("usr-graph-contract")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	conversation := store.ChatSession{ID: "cs-graph", UserID: project.OwnerID, ProjectID: project.ID, Title: "graph", CreatedAt: now, UpdatedAt: now}
	if err := fx.st.CreateChatSession(conversation); err != nil {
		t.Fatal(err)
	}
	view := approveRuntimeWorkflow(t, fx.st, project, conversation, store.WorkflowDraft{Goal: "graph", Tasks: []store.TaskDraft{{ID: "graph-task", RequiredRole: "developer", Goal: "two steps"}}}, now)
	items, err := svc.PlanTask(context.Background(), workflow.TaskPlanRequest{UserID: project.OwnerID, Project: project, Conversation: conversation, Workflow: view.Workflow, Task: store.TaskDraft{ID: view.Tasks[0].ID, RequiredRole: "developer", Goal: "two steps"}})
	if err != nil || len(items) != 2 || len(m.CallInputs()) != 2 {
		t.Fatalf("graph was not corrected: items=%v calls=%d err=%v", items, len(m.CallInputs()), err)
	}
	runs, _, err := fx.st.ListRunSessions(store.RunFilter{ChatSessionID: conversation.ID}, 0, 0)
	if err != nil || len(runs) != 1 || runs[0].Status != store.RunStatusCompleted {
		t.Fatalf("not one successful run: %v %v", runs, err)
	}
	executions, err := fx.st.ListRobotExecutions(project.ID, "", 100)
	if err != nil || len(executions) != 0 {
		t.Fatal("planning dispatched physical execution")
	}
}

func TestCheckpointRejectsMalformedStateContractBeforeModel(t *testing.T) {
	for _, key := range []string{"allowed_decisions", "allowed_actions"} {
		for _, value := range []any{nil, "stop", []string{}, []any{1}, []string{""}, []string{" "}} {
			event := testCheckpointEvent()
			event["context"].(map[string]any)[key] = value
			if _, err := checkpointReviewer(event, func(map[string]any) { t.Fatal("accepted") }); err == nil {
				t.Fatalf("bad request admitted: %s=%v", key, value)
			}
		}
	}
}

func TestPlanningGraphContractMatrix(t *testing.T) {
	base := []store.SubTaskDraft{{ID: "a", Kind: "agent_step", Goal: "first"}, {ID: "b", Kind: "agent_step", Goal: "next", DependsOn: []string{"a"}}}
	cases := []struct {
		name   string
		mutate func([]store.SubTaskDraft)
	}{
		{"missing goal", func(x []store.SubTaskDraft) { x[0].Goal = "" }},
		{"invalid kind", func(x []store.SubTaskDraft) { x[0].Kind = "invented" }},
		{"duplicate id", func(x []store.SubTaskDraft) { x[1].ID = "a" }},
		{"unknown dependency", func(x []store.SubTaskDraft) { x[1].DependsOn = []string{"outside"} }},
		{"self dependency", func(x []store.SubTaskDraft) { x[0].DependsOn = []string{"a"} }},
		{"cycle", func(x []store.SubTaskDraft) { x[0].DependsOn = []string{"b"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items := append([]store.SubTaskDraft(nil), base...)
			tc.mutate(items)
			encoded, _ := json.Marshal(map[string]any{"subtasks": items})
			if _, err := decodeAndValidateTaskPlanResult(string(encoded), store.Task{RequiredRole: "developer"}, robotTaskPlanningView{}); err == nil {
				t.Fatal("invalid plan escaped correction gate")
			}
			encoded, _ = json.Marshal(map[string]any{"decision": "revise_pending", "summary": "retry", "replacements": items})
			if _, err := decodeTaskRecoveryDecision(string(encoded), store.Task{RequiredRole: "developer"}, robotTaskPlanningView{}); err == nil {
				t.Fatal("invalid recovery escaped correction gate")
			}
		})
	}
	// Existing API permits omitted IDs; validation must not invent IDs downstream.
	items := []store.SubTaskDraft{{Kind: "agent_step", Goal: "valid"}}
	before := append([]store.SubTaskDraft(nil), items...)
	if err := store.ValidateSubTaskDrafts(items); err != nil || !reflect.DeepEqual(items, before) {
		t.Fatalf("valid plan changed: %v", err)
	}
}
