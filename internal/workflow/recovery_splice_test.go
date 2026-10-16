package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

func TestRecoveryPrefixPreservesPendingIdentityAndDependencies(t *testing.T) {
	st, project, conversation, service, _, executor := newWorkflowFixture(t)
	service.executor = &recoveringExecutor{controlledExecutor: executor, requests: make(chan TaskRecoveryRequest, 1),
		decision: TaskRecoveryDecision{Decision: "revise_pending", RetainPendingIDs: []string{"observe", "sweep", "release"},
			Replacements: []store.SubTaskDraft{{ID: "refresh", Kind: "robot_skill", Goal: "refresh"},
				{ID: "new-navigation", Kind: "robot_skill", Goal: "new navigation", DependsOn: []string{"refresh"}}}}}
	keepSpec := json.RawMessage(`{"skill_name":"r1pro-target-search","skill_version":"exact","intent":{"task":"sweep","context_from":"observe"}}`)
	view, task, steps := prepareRunningRobotTask(t, st, project, conversation, []store.SubTaskDraft{
		{ID: "failed-nav", Kind: "robot_skill", Goal: "navigation"},
		{ID: "obsolete", Kind: "robot_skill", Goal: "old repair", DependsOn: []string{"failed-nav"}},
		{ID: "observe", Kind: "robot_skill", Goal: "refresh context", DependsOn: []string{"obsolete"}},
		{ID: "sweep", Kind: "robot_skill", Goal: "sweep", Spec: keepSpec, CompletionCriteria: json.RawMessage(`{"consensus":true}`), DependsOn: []string{"observe"}},
		{ID: "release", Kind: "robot_skill", Goal: "release", DependsOn: []string{"sweep"}},
	})
	ex := store.RobotExecution{ID: "failed-execution", WorkflowID: view.Workflow.ID, TaskID: task.ID,
		SubtaskID: steps[0].ID, RobotID: task.AssignedRobotID, Status: "failed", Revision: 1, UpdatedAt: time.Now().UTC()}
	if err := st.SaveRobotExecution(ex); err != nil {
		t.Fatal(err)
	}
	if err := service.OnRobotExecutionChanged(context.Background(), ex, "execution.failed", nil); err != nil {
		t.Fatal(err)
	}
	next := receiveExecution(t, executor)
	defer func() { executor.release <- struct{}{} }()
	if len(next.SubTasks) != 1 || next.SubTasks[0].ID != "refresh" {
		t.Fatalf("wrong first step: %+v", next.SubTasks)
	}
	kept, _ := st.GetSubTask("sweep")
	if kept.ID != steps[3].ID || kept.Status != store.TaskStatusPending || kept.ExecutionRef != "" ||
		string(kept.Spec) != string(steps[3].Spec) || string(kept.CompletionCriteria) != string(steps[3].CompletionCriteria) {
		t.Fatalf("retained step rewritten: %+v", kept)
	}
	obsolete, _ := st.GetSubTask("obsolete")
	if obsolete.Status != store.TaskStatusStopped {
		t.Fatal("obsolete repair retained")
	}
	current, err := st.GetWorkflowView(view.Workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	edges := map[string]string{}
	for _, dep := range current.SubTaskDependencies {
		edges[dep.SubTaskID] = dep.DependsOnSubTaskID
	}
	for id, want := range map[string]string{"new-navigation": "refresh", "observe": "new-navigation", "sweep": "observe", "release": "sweep"} {
		if edges[id] != want {
			t.Fatalf("%s dependency = %s, want %s", id, edges[id], want)
		}
	}
	preserved, _ := st.GetRobotExecution(ex.ID)
	if preserved.Status != "failed" {
		t.Fatal("failure evidence changed")
	}
}
