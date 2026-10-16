package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/skill"
	"insightos.cn/semantic-framework/internal/store"
)

func TestPlanningToolCommentaryDoesNotCorruptFinalJSON(t *testing.T) {
	fx := newTestFixture(t)
	model := kernel.NewMockChatModel()
	model.SetScript(
		kernel.MockReply{Content: "先读取实际流程说明。", ToolCalls: []schema.ToolCall{{
			ID: "flow-doc", Type: "function", Function: schema.FunctionCall{
				Name: "skill", Arguments: `{"skill":"flow-guide"}`,
			},
		}}},
		kernel.MockReply{Content: `{"summary":"已按实际说明规划","subtasks":[]}`},
	)
	svc := newWorkflowRuntimeForTest(t, fx, model)
	project, err := fx.st.EnsureDefaultProject("usr-tool-commentary")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	conversation := store.ChatSession{ID: "cs-tool-commentary", UserID: project.OwnerID,
		ProjectID: project.ID, Title: "tool commentary", CreatedAt: now, UpdatedAt: now}
	if err := fx.st.CreateChatSession(conversation); err != nil {
		t.Fatal(err)
	}
	view := approveRuntimeWorkflow(t, fx.st, project, conversation, store.WorkflowDraft{
		Goal: "test", Tasks: []store.TaskDraft{{ID: "task-tool-commentary", RequiredRole: "developer", Goal: "test"}},
	}, now)
	task := view.Tasks[0]
	guide := &taskPlanningSkillStore{robot: map[string]skill.Skill{
		"flow-guide": {Name: "flow-guide", Description: "Planning flow", Body: "Read the actual dependency graph."},
	}}
	_, err = svc.runTaskAgent(context.Background(), project.OwnerID, project, conversation,
		view.Workflow, "", &task, "developer-1", buildTaskPlanningPrompt([]byte(`{}`), ""),
		nil, guide, store.RunKindTaskPlanning, "", func(body string) error {
			var result map[string]json.RawMessage
			return decodeStrictJSON(body, &result)
		})
	if err != nil || len(model.CallInputs()) != 2 {
		t.Fatalf("Valid final JSON after a documented query must not need correction: %v (calls=%d)", err, len(model.CallInputs()))
	}
}
