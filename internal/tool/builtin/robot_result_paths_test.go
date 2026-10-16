package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	robotdomain "insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
)

func TestRobotResultProjectionReadsStoredEvidenceWithoutPilot(t *testing.T) {
	_, st := newTestRegistry(t)
	if err := st.SaveRobotExecution(store.RobotExecution{ID: "rex-result", ProjectID: "project",
		RobotID: "robot", Status: "completed", Result: map[string]any{
			"payload": map[string]any{"views": []string{"view-1", "view-2"}, "packet": strings.Repeat("large", 10000)},
		}}); err != nil {
		t.Fatal(err)
	}
	get := &robotGetTool{service: robotdomain.NewService(st, nil)}
	scope := tool.ExecutionScope{ProjectID: "project", RobotID: "robot", TaskID: "task", SessionID: "session", WorkspaceRoot: t.TempDir()}
	ctx := tool.WithExecutionScope(context.Background(), scope)
	value, err := get.Run(ctx, `{"execution_id":"rex-result","result_paths":["/payload/views"]}`)
	if err != nil || !strings.Contains(value, "view-2") || strings.Contains(value, "packet") || len(value) > 1500 {
		t.Fatalf("projection should contain only requested facts: %s, %v", value, err)
	}
	if _, err := get.Run(ctx, `{"result_paths":["/payload"]}`); err == nil {
		t.Fatal("missing execution identity accepted")
	}
	value, err = get.Run(ctx, `{"execution_id":"rex-result","result_paths":["/payload/views","/payload/sessions"]}`)
	if err != nil {
		t.Fatalf("a missing read field must not exhaust invocation-correction retries: %v", err)
	}
	var response struct {
		Data struct {
			Values   map[string]json.RawMessage `json:"result_values"`
			Errors   map[string]string          `json:"result_path_errors"`
			Complete bool                       `json:"result_query_complete"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(value), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Complete || response.Data.Values["/payload/sessions"] != nil ||
		!strings.Contains(string(response.Data.Values["/payload/views"]), "view-2") ||
		!strings.Contains(response.Data.Errors["/payload/sessions"], "available fields") {
		t.Fatalf("partial lookup must preserve valid facts and explicitly identify missing ones: %s", value)
	}
	for _, altered := range []tool.ExecutionScope{
		{ProjectID: "other", RobotID: "robot", TaskID: "task", SessionID: "session", WorkspaceRoot: t.TempDir()},
		{ProjectID: "project", RobotID: "other", TaskID: "task", SessionID: "session", WorkspaceRoot: t.TempDir()},
	} {
		if _, err := get.Run(tool.WithExecutionScope(context.Background(), altered),
			`{"execution_id":"rex-result","result_paths":["/payload/views"]}`); err == nil {
			t.Fatal("projection bypassed execution scope")
		}
	}
}

func TestRobotResultJSONPointersPreserveValues(t *testing.T) {
	result := json.RawMessage(`{"payload":{"a/b":{"~key":[9007199254740993,null,-0.12345678901234567]}}}`)
	values, err := robotdomain.SelectResultPaths(result, []string{"/payload/a~1b/~0key/0", "/payload/a~1b/~0key/1", "/payload/a~1b/~0key/2"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(values)
	for _, exact := range []string{"9007199254740993", "null", "-0.12345678901234567"} {
		if !strings.Contains(string(encoded), exact) {
			t.Fatal("projection changed numeric or null evidence", string(encoded))
		}
	}
	for _, invalid := range []string{"payload", "/payload/~", "/payload/~2", "/payload/a~1b/~0key/-1", "/payload/a~1b/~0key/00", "/payload/a~1b/~0key/3"} {
		if _, err := robotdomain.SelectResultPaths(result, []string{invalid}); err == nil {
			t.Fatal("invalid pointer accepted", invalid)
		}
	}
}

func TestRobotExecutionLookupOmitsEventsAndNeedsNoLivePilot(t *testing.T) {
	_, st := newTestRegistry(t)
	if err := st.SaveRobotExecution(store.RobotExecution{ID: "rex-stored", ProjectID: "project", RobotID: "robot",
		Status: "completed", Input: map[string]any{"task": "plan"}, Result: map[string]any{"payload": "exact-result"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendRobotExecutionEvent(store.RobotExecutionEvent{ExecutionID: "rex-stored", Sequence: 1,
		Type: "action.terminal", Payload: map[string]any{"result": "duplicated-event-result"}}); err != nil {
		t.Fatal(err)
	}
	get := &robotGetTool{service: robotdomain.NewService(st, nil)}
	scope := tool.ExecutionScope{ProjectID: "project", RobotID: "robot", TaskID: "task", SessionID: "session", WorkspaceRoot: t.TempDir()}
	value, err := get.Run(tool.WithExecutionScope(context.Background(), scope), `{"execution_id":"rex-stored"}`)
	if err != nil || !strings.Contains(value, "exact-result") || !strings.Contains(value, `"task":"plan"`) || strings.Contains(value, "duplicated-event-result") {
		t.Fatalf("stored input/result must remain available without live state or duplicated events: %s %v", value, err)
	}
	scope.RobotID = "other"
	if _, err := get.Run(tool.WithExecutionScope(context.Background(), scope), `{"execution_id":"rex-stored"}`); err == nil {
		t.Fatal("execution lookup bypassed Robot scope")
	}
}

func TestRobotGetRecoversScopedSubTaskBeforeExecutionExists(t *testing.T) {
	_, st := newTestRegistry(t)
	project, err := st.CreateProject("context-user", "context")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.CreateChatSession(store.ChatSession{ID: "cs-scope", ProjectID: project.ID,
		UserID: project.OwnerID, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	proposal, err := st.SubmitPlanProposal(project.ID, "cs-scope", store.WorkflowDraft{Goal: "context",
		Tasks: []store.TaskDraft{{ID: "task-context", RequiredRole: "robot", Goal: "select posture",
			SubTasks: []store.SubTaskDraft{{ID: "sub-context", Kind: "robot_skill", Goal: "select target posture",
				Spec: json.RawMessage(`{"skill_name":"manipulation","skill_version":"1.2.3","intent":{"task":"plan"}}`)}}}}},
		"context", nil, "context", now)
	if err != nil {
		t.Fatal(err)
	}
	view, err := st.ApprovePlanProposal(proposal.ID, proposal.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssignTask(view.Tasks[0].ID, view.Tasks[0].Revision, "robot:robot", "robot", now); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRobotPilot(store.RobotPilot{PilotInstanceID: "context-pilot", RobotID: "robot",
		RobotStatus: "idle", LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	scope := tool.ExecutionScope{ProjectID: project.ID, WorkflowID: view.Workflow.ID,
		TaskID: "task-context", SubtaskID: "sub-context", RobotID: "robot", SessionID: "cs-scope", WorkspaceRoot: t.TempDir()}
	get := &robotGetTool{service: robotdomain.NewService(st, nil)}
	value, err := get.Run(tool.WithExecutionScope(context.Background(), scope), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var response struct{ Data map[string]json.RawMessage }
	if err := json.Unmarshal([]byte(value), &response); err != nil {
		t.Fatal(err)
	}
	current := string(response.Data["current_subtask"])
	if !strings.Contains(current, "sub-context") || !strings.Contains(current, "1.2.3") ||
		!strings.Contains(current, `"task":"plan"`) || response.Data["completed_subtasks"] == nil {
		t.Fatalf("missing recoverable call context before admission: %s", value)
	}
	scope.ProjectID = "other-project"
	value, err = get.Run(tool.WithExecutionScope(context.Background(), scope), `{}`)
	if err == nil && strings.Contains(value, "sub-context") {
		t.Fatal("scoped SubTask leaked across Project")
	}
}
