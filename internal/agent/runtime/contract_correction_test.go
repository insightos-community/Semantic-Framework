package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/store"
)

func TestCorrectionBudgetSharedBoundedAndCancelled(t *testing.T) {
	budget := newContractCorrectionBudget(10)
	policy := &contractToolPolicy{next: planningToolPolicy{}, budget: budget}
	failure := func(context.Context, string) (string, error) {
		return `{"ok":false,"error":{"code":"INVALID","message":"filter required"}}`, nil
	}
	if _, err := policy.WrapToolCall(context.Background(), kernel.ToolCallMeta{Name: "map.query"}, `{}`, failure); err != nil {
		t.Fatal(err)
	}
	if err := policy.BeforeModelRound(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := budget.request(context.Background(), errors.New("output schema error")); err != nil {
		t.Fatal(err)
	}
	if err := budget.request(context.Background(), errors.New("different field error")); err == nil {
		t.Fatal("different errors bypassed shared cap")
	}
	if err := newContractCorrectionBudget(1).request(context.Background(), errors.New("bad")); err == nil {
		t.Fatal("profile cap ignored")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := newContractCorrectionBudget(10).request(ctx, errors.New("bad")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

func TestContractToolPolicyConcurrentBatch(t *testing.T) {
	policy := &contractToolPolicy{next: planningToolPolicy{}, budget: newContractCorrectionBudget(10)}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := policy.WrapToolCall(context.Background(), kernel.ToolCallMeta{Name: "map.query"}, `{}`,
				func(context.Context, string) (string, error) {
					return `{"ok":false,"error":{"code":"INVALID","message":"filter required"}}`, nil
				})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := policy.BeforeModelRound(context.Background()); err != nil || policy.budget.used != 1 {
		t.Fatalf("one batch, one correction: %v %+v", err, policy.budget)
	}
}

// Exercise the real Eino tool-batch -> model middleware boundary, not just
// the policy in isolation. Reproduces two bad map queries in ONE response.
func TestContractCorrectionRealRunnerBatchFeedback(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprint(persistent), func(t *testing.T) {
			var hits, completed atomic.Int32
			query, err := utils.InferTool("query", "query with a filter", func(_ context.Context, in struct {
				Filter string `json:"filter"`
			}) (string, error) {
				hits.Add(1)
				if in.Filter == "" {
					return `{"ok":false,"error":{"code":"INVALID","message":"filter required"}}`, nil
				}
				return `{"ok":true,"data":[]}`, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			commit, err := utils.InferTool("commit", "already completed operation", func(context.Context, struct{}) (string, error) {
				completed.Add(1)
				return `{"ok":true}`, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			call := func(id, name, args string) schema.ToolCall {
				return schema.ToolCall{ID: id, Type: "function", Function: schema.FunctionCall{Name: name, Arguments: args}}
			}
			m := kernel.NewMockChatModel()
			first := kernel.MockReply{ToolCalls: []schema.ToolCall{call("q1", "query", `{}`), call("q2", "query", `{}`), call("done", "commit", `{}`)}}
			if persistent {
				m.SetScript(first, kernel.MockReply{ToolCalls: []schema.ToolCall{call("q3", "query", `{}`), call("q4", "query", `{}`)}}, kernel.MockReply{ToolCalls: []schema.ToolCall{call("q5", "query", `{}`), call("q6", "query", `{}`)}})
			} else {
				m.SetScript(first, kernel.MockReply{ToolCalls: []schema.ToolCall{call("q3", "query", `{"filter":"active"}`)}}, kernel.MockReply{Content: "done"})
			}
			policy := &contractToolPolicy{next: taskDecisionToolPolicy{allowed: map[string]struct{}{"query": {}, "commit": {}}}, budget: newContractCorrectionBudget(10)}
			runner, err := kernel.BuildAgent(context.Background(), kernel.AgentConfig{Name: "round-test", Model: m, Tools: []kernel.Tool{query, commit}, ToolPolicy: policy, MaxTurns: 10})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			stream, err := runner.Run(ctx, nil, "query")
			if err != nil {
				t.Fatal(err)
			}
			var runErr error
			for {
				event, ok := stream.Next()
				if !ok {
					break
				}
				if event.Kind == kernel.EventError {
					runErr = event.Err
				}
			}
			if (runErr != nil) != persistent {
				t.Fatalf("persistent=%v err=%v", persistent, runErr)
			}
			if persistent && !strings.Contains(runErr.Error(), "纠正预算耗尽") {
				t.Fatalf("must stop at correction cap, not max_turns or a tool error: %v", runErr)
			}
			inputs := m.CallInputs()
			if len(inputs) != 3 {
				t.Fatalf("wanted 3 model exchanges, got %d: %v", len(inputs), runErr)
			}
			feedback := 0
			for _, message := range inputs[1] {
				if message.Role == schema.Tool && strings.Contains(message.Content, "filter required") {
					feedback++
				}
			}
			if feedback != 2 {
				t.Fatalf("second model exchange must see BOTH failures: %d", feedback)
			}
			wantHits := int32(3)
			if persistent {
				wantHits = 6
			}
			if hits.Load() != wantHits || completed.Load() != 1 {
				t.Fatalf("tool replay or skipped calls: query=%d completed=%d", hits.Load(), completed.Load())
			}
		})
	}
}

func TestContractToolPolicyDoesNotRetryInfrastructureErrors(t *testing.T) {
	policy := &contractToolPolicy{next: planningToolPolicy{}, budget: newContractCorrectionBudget(10)}
	// Arbitrary Go errors (including cancellation/approval interrupts) are not
	// model-correctable schema results. Preserve identity for existing handlers.
	want := errors.New("infrastructure failure")
	_, got := policy.WrapToolCall(context.Background(), kernel.ToolCallMeta{Name: "map.query"}, `{}`,
		func(context.Context, string) (string, error) { return "", want })
	if got != want {
		t.Fatalf("error identity changed: %v", got)
	}
	if err := policy.BeforeModelRound(context.Background()); err != nil || policy.budget.used != 0 {
		t.Fatalf("infrastructure failure incorrectly scheduled model correction: %v", err)
	}
}

func TestPlanSubmissionStopsBeforeNextModelRoundButFailureCanCorrect(t *testing.T) {
	for _, failFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(failFirst), func(t *testing.T) {
			var calls atomic.Int32
			publish, err := utils.InferTool("plan_suggest", "submit reviewable plan", func(context.Context, struct{}) (string, error) {
				if failFirst && calls.Add(1) == 1 {
					return `{"ok":false,"error":{"code":"BAD_ARGUMENTS","message":"missing field","retryable":false}}`, nil
				}
				if !failFirst {
					calls.Add(1)
				}
				return `{"ok":true,"data":{"plan_proposal_id":"plan-1","revision":1}}`, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			toolCall := func(id string) kernel.MockReply {
				return kernel.MockReply{ToolCalls: []schema.ToolCall{{ID: id, Type: "function",
					Function: schema.FunctionCall{Name: "plan_suggest", Arguments: `{}`}}}}
			}
			m := kernel.NewMockChatModel()
			if failFirst {
				m.SetScript(toolCall("bad"), toolCall("good"), kernel.MockReply{Content: "must not run"})
			} else {
				m.SetScript(toolCall("good"), kernel.MockReply{Content: "must not run"})
			}
			policy := &contractToolPolicy{next: taskDecisionToolPolicy{allowed: map[string]struct{}{"plan_suggest": {}}},
				budget: newContractCorrectionBudget(10)}
			runner, err := kernel.BuildAgent(context.Background(), kernel.AgentConfig{
				Name: "leader", Model: m, Tools: []kernel.Tool{publish}, ToolPolicy: policy, MaxTurns: 10,
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			stream, err := runner.Run(ctx, nil, "make proposal")
			if err != nil {
				t.Fatal(err)
			}
			var terminal kernel.Event
			for {
				event, ok := stream.Next()
				if !ok {
					break
				}
				if event.Kind == kernel.EventError || event.Kind == kernel.EventDone {
					terminal = event
				}
			}
			want := 1
			if failFirst {
				want = 2
			}
			if terminal.Kind != kernel.EventError || !errors.Is(terminal.Err, errPlanSubmissionComplete) {
				t.Fatalf("success must be terminal: %+v", terminal)
			}
			if terminal.Turns != want || len(m.CallInputs()) != want || int(calls.Load()) != want {
				t.Fatalf("unexpected exchanges or calls: event=%+v model=%d tools=%d", terminal,
					len(m.CallInputs()), calls.Load())
			}
		})
	}
}

func TestJSONDiagnosticsKeepStrictnessAndPosition(t *testing.T) {
	for _, text := range []string{"{\n\"摘要\": 1, {\"x\":2}}", `{"x":`, ""} {
		var out map[string]any
		err := decodeStrictJSON(text, &out)
		var detail *jsonContractError
		if !errors.As(err, &detail) || detail.Line < 1 || detail.Column < 1 || detail.Offset < 1 {
			t.Fatalf("missing location: %v", err)
		}
		if len([]rune(detail.Snippet)) > 104 {
			t.Fatal("unbounded feedback")
		}
	}
	var out map[string]any
	err := decodeStrictJSON(`{"x":1, {"y":2}}`, &out)
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) {
		t.Fatalf("original syntax error lost: %v", err)
	}
	for _, text := range []string{`{} {}`, "```json\n{}\n```"} {
		if decodeStrictJSON(text, &out) == nil {
			t.Fatal("invalid output accepted")
		}
	}
}

func TestTaskPlanningRepeatedSyntaxErrorCanRecoverOnThirdOutput(t *testing.T) {
	fx := newTestFixture(t)
	m := kernel.NewMockChatModel()
	bad1 := `{"summary":"one","subtasks":[{"id":"first", {"id":"second"}}]}`
	bad2 := `{"summary":"a changed output","subtasks":[{"id":"first changed", {"id":"second"}}]}`
	// Both have the SAME json.SyntaxError text, but different offending offsets.
	m.SetScript(kernel.MockReply{Content: bad1}, kernel.MockReply{Content: bad2}, kernel.MockReply{Content: `{"summary":"valid","subtasks":[]}`})
	svc := newWorkflowRuntimeForTest(t, fx, m)
	project, err := fx.st.EnsureDefaultProject("usr-repeat-syntax")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	conversation := store.ChatSession{ID: "cs-repeat-syntax", UserID: project.OwnerID, ProjectID: project.ID, Title: "correction", CreatedAt: now, UpdatedAt: now}
	if err := fx.st.CreateChatSession(conversation); err != nil {
		t.Fatal(err)
	}
	view := approveRuntimeWorkflow(t, fx.st, project, conversation, store.WorkflowDraft{Goal: "test", Tasks: []store.TaskDraft{{ID: "task", RequiredRole: "developer", Goal: "test"}}}, now)
	task := view.Tasks[0]
	_, err = svc.runTaskAgent(context.Background(), project.OwnerID, project, conversation, view.Workflow, "", &task, "developer-1", buildTaskPlanningPrompt([]byte(`{}`), ""), nil, nil, store.RunKindTaskPlanning, "", func(body string) error { var value map[string]any; return decodeStrictJSON(body, &value) })
	if err != nil || len(m.CallInputs()) != 3 {
		t.Fatalf("third valid output must be reached: calls=%d err=%v", len(m.CallInputs()), err)
	}
	last := m.CallInputs()[2]
	if !strings.Contains(last[len(last)-1].Content, "byte_offset=") {
		t.Fatal("correction lacks position")
	}
	runs, _, err := fx.st.ListRunSessions(store.RunFilter{ChatSessionID: conversation.ID}, 0, 0)
	if err != nil || len(runs) != 1 || runs[0].Status != store.RunStatusCompleted {
		t.Fatalf("must remain one completed planning Run: %+v %v", runs, err)
	}
}
