// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/store"
)

func authenticatedGet(t *testing.T, url, token string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s 失败: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("GET %s 响应不是 JSON: %v", url, err)
	}
	return resp.StatusCode, body
}

func TestV030ProductionPlanTaskFlow(t *testing.T) {
	t.Setenv(kernel.MockScriptEnv, `{"scripts":[
		{"match":"请生成验证报告的计划","replies":[{"tool_calls":[{"id":"plan-v030","name":"plan_suggest","arguments":"{\"goal\":\"生成验证报告\",\"summary\":\"生成并验证报告\",\"constraints\":{},\"completion_criteria\":{\"done\":true},\"tasks\":[{\"id\":\"task-v030-e2e\",\"required_role\":\"developer\",\"goal\":\"生成报告\",\"input\":{},\"completion_criteria\":{\"done\":true}}],\"dependencies\":[]}"}]},{"content":"计划已可审阅。"}]},
		{"match":"你是 Developer Task 的结果责任人","replies":[{"content":"{\"kind\":\"result\",\"summary\":\"报告已生成\",\"evidence\":[\"trace\"]}"}]},
		{"match":"请为这个 Worker Task 生成可独立推进的类型化 SubTask","replies":[{"content":"{\"subtasks\":[{\"id\":\"sub-v030-e2e\",\"kind\":\"agent_step\",\"goal\":\"完成报告\",\"spec\":{},\"completion_criteria\":{\"done\":true},\"depends_on\":[]}]}"}]},
		{"match":"普通聊天不会进入 Plan","replies":[{"content":"这是普通对话回答。"}]},
		{"match":"","replies":[{"content":"兜底回答"}]}
	]}`)
	httpBase, wsBase, app, stop := startTeamApp(t)
	defer stop()
	token := login(t, httpBase)
	code, projectBody := chatPost(t, httpBase+"/api/v1/projects", token,
		`{"name":"v0.3 组合验收"}`)
	if code != http.StatusCreated {
		t.Fatalf("创建 Project 失败: %d %+v", code, projectBody)
	}
	projectID, _ := projectBody["project"].(map[string]any)["id"].(string)
	project, err := app.Store().GetProject(projectID)
	if err != nil || project.ID == "" {
		t.Fatalf("读取活动 Project 失败: %+v %v", project, err)
	}
	code, conversationBody := chatPost(t, fmt.Sprintf("%s/api/v1/projects/%s/conversations", httpBase, project.ID),
		token, `{"title":"v0.3 组合验收"}`)
	if code != http.StatusCreated {
		t.Fatalf("创建 Conversation 失败: %d %+v", code, conversationBody)
	}
	conversationID, _ := conversationBody["conversation"].(map[string]any)["id"].(string)
	conn := dialChat(t, wsBase, token, conversationID)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	sendChatMessage(t, conn, conversationID, "普通聊天不会进入 Plan")
	_ = waitDialogueDone(t, conn)
	if workflows, err := app.Store().ListWorkflows(project.ID, false); err != nil || len(workflows) != 0 {
		t.Fatalf("普通聊天不得隐式创建 Workflow: %+v %v", workflows, err)
	}

	sendPlanChatMessage(t, conn, conversationID, "请生成验证报告的计划")
	_ = waitDialogueDone(t, conn)
	proposalCollectionURL := fmt.Sprintf("%s/api/v1/projects/%s/plan-proposals", httpBase, project.ID)
	code, proposalBody := authenticatedGet(t, proposalCollectionURL+"/active", token)
	if code != http.StatusOK {
		t.Fatalf("Plan Run 未生成 ready Proposal: %d %+v", code, proposalBody)
	}
	if proposalBody["plan_proposal"] == nil {
		runs, _, _ := app.Store().ListRunSessions(store.RunFilter{ProjectID: project.ID}, 0, 0)
		t.Fatalf("Plan Run 未提交 Proposal: body=%v runs=%v", proposalBody, runs)
	}
	readyProposal := proposalBody["plan_proposal"].(map[string]any)
	proposalID, _ := readyProposal["id"].(string)
	if proposalID == "" || readyProposal["status"] != store.PlanProposalStatusReady {
		t.Fatalf("Plan Run 应直接提交完整 Proposal: %+v", readyProposal)
	}
	if workflows, err := app.Store().ListWorkflows(project.ID, false); err != nil || len(workflows) != 0 {
		t.Fatalf("用户批准前不能创建 Workflow: workflows=%+v err=%v", workflows, err)
	}
	revision := int64(readyProposal["revision"].(float64))
	structuredPlan := readyProposal["structured_plan"].(map[string]any)
	if len(structuredPlan["tasks"].([]any)) != 1 {
		t.Fatalf("Proposal 应只保存 Leader Task TODO: %+v", readyProposal)
	}
	code, snapshotBody := authenticatedGet(t,
		fmt.Sprintf("%s/api/v1/projects/%s/studio/snapshot", httpBase, project.ID), token)
	if code != http.StatusOK {
		t.Fatalf("读取 Studio Snapshot 失败: %d %+v", code, snapshotBody)
	}
	snapshot := snapshotBody["snapshot"].(map[string]any)
	if snapshot["plan_proposal"] == nil || snapshot["workflow_view"] != nil {
		t.Fatalf("批准前 Snapshot 应只有 Proposal: %+v", snapshot)
	}
	proposalURL := proposalCollectionURL + "/" + proposalID
	code, confirmed := chatPost(t, proposalURL+"/approve", token,
		fmt.Sprintf(`{"revision":%d}`, revision))
	if code != http.StatusOK {
		t.Fatalf("批准计划失败: %d %+v", code, confirmed)
	}
	approvedView := confirmed["workflow_view"].(map[string]any)
	workflowMap := approvedView["workflow"].(map[string]any)
	workflowID, _ := workflowMap["id"].(string)
	if workflowID == "" {
		t.Fatalf("批准 Proposal 未原子创建 Workflow: %+v", approvedView)
	}
	workflowURL := fmt.Sprintf("%s/api/v1/projects/%s/workflows", httpBase, project.ID)
	viewURL := workflowURL + "/" + workflowID + "/view"

	deadline := time.Now().Add(8 * time.Second)
	var finalView map[string]any
	for time.Now().Before(deadline) {
		code, body := authenticatedGet(t, viewURL, token)
		if code != http.StatusOK {
			t.Fatalf("读取 Workflow View 失败: %d %+v", code, body)
		}
		finalView = body["workflow_view"].(map[string]any)
		workflowValue := finalView["workflow"].(map[string]any)
		if workflowValue["status"] == store.WorkflowStatusCompleted {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if finalView == nil || finalView["workflow"].(map[string]any)["status"] != store.WorkflowStatusCompleted {
		t.Fatalf("Worker Task 未完成: %+v", finalView)
	}

	storedView, err := app.Store().GetWorkflowView(workflowID)
	if err != nil || len(storedView.Tasks) != 1 {
		t.Fatalf("读取最终 Workflow 失败: %+v %v", storedView, err)
	}
	contextMessages, err := app.Store().ListContextMessagesAfter(storedView.Tasks[0].ContextID, "", 0)
	if err != nil || len(contextMessages) < 4 {
		t.Fatalf("Task Planning 与执行未写入独立 Context: %+v %v", contextMessages, err)
	}
	for _, message := range contextMessages {
		if message.Message != nil && strings.Contains(message.Message.Content, "普通聊天不会进入 Plan") {
			t.Fatalf("Task Context 串入 Leader 历史: %+v", message)
		}
	}
	var runs []any
	summaryDeadline := time.Now().Add(3 * time.Second)
	summaryCompleted := false
	for time.Now().Before(summaryDeadline) {
		code, runsBody := authenticatedGet(t,
			fmt.Sprintf("%s/api/v1/projects/%s/runs?page_size=100", httpBase, project.ID), token)
		if code != http.StatusOK {
			t.Fatalf("读取 Run 列表失败: %d %+v", code, runsBody)
		}
		runs = runsBody["runs"].([]any)
		completed := false
		for _, raw := range runs {
			run := raw.(map[string]any)
			if run["kind"] == store.RunKindConversation && run["workflow_id"] == workflowID &&
				run["status"] == store.RunStatusCompleted {
				completed = true
				break
			}
		}
		if completed {
			summaryCompleted = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !summaryCompleted {
		t.Fatalf("Workflow 终态后缺少已完成的 Leader 总结 Run: %+v", runs)
	}
	wantKinds := map[string]bool{store.RunKindConversation: false,
		store.RunKindTaskPlanning: false, store.RunKindTaskExecution: false}
	for _, raw := range runs {
		run := raw.(map[string]any)
		kind, _ := run["kind"].(string)
		if _, ok := wantKinds[kind]; !ok {
			continue
		}
		if run["trace_id"] == "" || run["status"] != store.RunStatusCompleted {
			t.Fatalf("Planning/Task Run 缺 Trace 或终态错误: %+v", run)
		}
		if kind == store.RunKindConversation {
			if value, _ := run["workflow_id"].(string); value != "" && value != workflowID {
				t.Fatalf("Conversation Run 关联了错误 Workflow: %+v", run)
			}
		} else if run["workflow_id"] != workflowID {
			t.Fatalf("Task Run 未归属已批准 Workflow: %+v", run)
		}
		if kind != store.RunKindConversation &&
			run["task_id"] != storedView.Tasks[0].ID {
			t.Fatalf("Task Run 未精确关联 Task: %+v", run)
		}
		wantKinds[kind] = true
	}
	for kind, found := range wantKinds {
		if !found {
			t.Errorf("缺少 %s Run: %+v", kind, runs)
		}
	}
}
