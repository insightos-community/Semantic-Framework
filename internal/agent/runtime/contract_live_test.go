package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/workflow"
	"insightos.cn/semantic-framework/pkg/llm"
)

// Explicitly opt-in real-model component test. No physical tools are installed;
// this supplements, never substitutes for, the four-box system regression.
// Mutate only invalid outputs; subsequent corrected answers come from the model.
type liveContractFaultModel struct {
	kernel.Model
	sequence        []string
	calls, injected int
	t               *testing.T
}

func (m *liveContractFaultModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	start := time.Now()
	reply, err := m.Model.Generate(ctx, input, opts...)
	m.calls++
	if err != nil {
		return nil, err
	}
	m.t.Logf("live_call ordinal=%d duration_ms=%d usage=%+v", m.calls, time.Since(start).Milliseconds(), reply.ResponseMeta)
	if m.injected < len(m.sequence) {
		var value map[string]any
		if len(reply.ToolCalls) != 0 || json.Unmarshal([]byte(reply.Content), &value) != nil || value == nil {
			return nil, fmt.Errorf("cannot inject into non-object real model output")
		}
		switch m.sequence[m.injected] {
		case "state":
			value["action"] = "continue"
		case "revision":
			value["expected_plan_revision"] = 8
		case "recovery":
			value["decision"] = "invalid_recovery_decision"
		}
		encoded, _ := json.Marshal(value)
		copy := *reply
		copy.Content = string(encoded)
		reply = &copy
		m.injected++
	}
	return reply, nil
}

func (m *liveContractFaultModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	reply, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{reply}), nil
}

func TestLiveContractDecisionMatrix(t *testing.T) {
	if os.Getenv("SEMANTIC_CONTRACT_LIVE") != "1" {
		t.Skip("explicit real-model opt-in required")
	}
	endpoint, modelID, key := os.Getenv("SEMANTIC_CONTRACT_LIVE_URL"), os.Getenv("SEMANTIC_CONTRACT_LIVE_MODEL"), os.Getenv("SEMANTIC_CONTRACT_LIVE_KEY")
	if endpoint == "" || modelID == "" || key == "" {
		t.Fatal("missing explicit live provider configuration")
	}
	for _, tc := range []struct {
		name     string
		sequence []string
		safeStop bool
	}{
		{"checkpoint_state", []string{"state"}, false},
		{"checkpoint_mixed", []string{"state", "revision"}, false},
		{"checkpoint_exhaust", []string{"state", "state", "state"}, true},
		{"recovery_enum", []string{"recovery"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
			defer cancel()
			realModel, err := kernel.NewChatModel(ctx, llm.Provider{Name: "contract-live", Component: "openai", BaseURL: endpoint, Model: modelID}, key)
			if err != nil {
				t.Fatal(err)
			}
			m := &liveContractFaultModel{Model: realModel, sequence: tc.sequence, t: t}
			fx := newTestFixture(t)
			svc := newWorkflowRuntimeForTest(t, fx, m)
			project, err := fx.st.EnsureDefaultProject("usr-live-contract")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			conversation := store.ChatSession{ID: "cs-live-contract", UserID: project.OwnerID, ProjectID: project.ID, Title: "read-only component validation", CreatedAt: now, UpdatedAt: now}
			if err := fx.st.CreateChatSession(conversation); err != nil {
				t.Fatal(err)
			}
			view := approveRuntimeWorkflow(t, fx.st, project, conversation, store.WorkflowDraft{Goal: "generic contract validation", Tasks: []store.TaskDraft{{ID: "task-live-contract", RequiredRole: "developer", Goal: "no physical tools available"}}}, now)
			task := view.Tasks[0]
			if strings.HasPrefix(tc.name, "checkpoint") {
				event := map[string]any{"reason": "only stop is currently authorized", "context": map[string]any{"plan_revision": 7, "allowed_actions": []string{"stop"}},
					"response_schema": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"action", "expected_plan_revision"}, "properties": map[string]any{
						"action": map[string]any{"type": "string", "enum": []string{"continue", "stop"}}, "expected_plan_revision": map[string]any{"type": "integer"}}}}
				value, e := svc.ResolveRobotAgentRequest(ctx, workflow.RobotAgentDecisionRequest{UserID: project.OwnerID, Project: project, Conversation: conversation, Workflow: view.Workflow, Task: task,
					SubTask: store.SubTask{ID: "sub-live", TaskID: task.ID, Goal: "choose only authorized decision"}, Execution: store.RobotExecution{ID: "fixture-only-not-dispatched", SkillName: "generic-contract-fixture"}, Event: event})
				err = e
				if !tc.safeStop && (value["action"] != "stop" || value["expected_plan_revision"] != float64(7)) {
					t.Fatalf("invalid accepted decision: %v", value)
				}
				if tc.safeStop && len(value) != 0 {
					t.Fatal("unsafe decision returned")
				}
			} else {
				_, err = svc.runTaskAgent(ctx, project.OwnerID, project, conversation, view.Workflow, "", &task, task.AssignedAgentID,
					buildTaskRecoveryPrompt([]byte(`{"pending_subtasks":[],"robot_catalog":{"skills":[]},"reason":"No authorized replacement work exists; fail the task safely."}`)), nil, nil,
					store.RunKindTaskExecution, runtimePurposeTaskRecovery, func(text string) error {
						_, e := decodeTaskRecoveryDecision(text, task, robotTaskPlanningView{})
						return e
					})
			}
			if tc.safeStop {
				if err == nil || !strings.Contains(err.Error(), "纠正预算耗尽") || m.calls != 3 {
					t.Fatalf("expected bounded stop: calls=%d err=%v", m.calls, err)
				}
			} else if err != nil || m.calls > 3 {
				t.Fatalf("correction failed: calls=%d err=%v", m.calls, err)
			}
			if m.injected != len(tc.sequence) || (!tc.safeStop && m.calls <= m.injected) {
				t.Fatalf("injection/recovery not exercised: calls=%d injections=%d", m.calls, m.injected)
			}
			executions, e := fx.st.ListRobotExecutions(project.ID, "", 100)
			if e != nil || len(executions) != 0 {
				t.Fatalf("read-only decision dispatched an execution: count=%d err=%v", len(executions), e)
			}
		})
	}
}
