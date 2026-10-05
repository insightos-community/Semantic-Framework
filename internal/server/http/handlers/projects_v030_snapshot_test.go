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

package handlers

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

func TestStudioSnapshotKeepsOnlyLatestEndedWorkflowAlongsideActive(t *testing.T) {
	st, _, router, project, session := newV030HTTPTest(t, true)
	base := time.Now().UTC().Add(-time.Hour)
	createWorkflow := func(index int, status string) store.Workflow {
		t.Helper()
		now := base.Add(time.Duration(index) * time.Minute)
		conversation := session
		conversation.ID = fmt.Sprintf("snapshot-conversation-%d", index)
		if err := st.CreateChatSession(conversation); err != nil {
			t.Fatal(err)
		}
		proposal, err := st.SubmitPlanProposal(project.ID, conversation.ID, store.WorkflowDraft{
			Goal: fmt.Sprintf("工作 %d", index),
			Tasks: []store.TaskDraft{{ID: fmt.Sprintf("snapshot-task-%d", index),
				RequiredRole: "developer", Goal: "验证结果"}},
		}, "", nil, "# 计划", now)
		if err != nil {
			t.Fatal(err)
		}
		view, err := st.ApprovePlanProposal(proposal.ID, proposal.Revision, now)
		if err != nil {
			t.Fatal(err)
		}
		workflow := view.Workflow
		if status != store.WorkflowStatusRunning {
			workflow, err = st.TransitionWorkflow(workflow.ID, workflow.Revision, status, now.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
		}
		return workflow
	}
	old := createWorkflow(1, store.WorkflowStatusCompleted)
	latest := createWorkflow(2, store.WorkflowStatusFailed)
	paused := createWorkflow(3, store.WorkflowStatusPaused)
	url := "/api/v1/projects/" + project.ID + "/studio/snapshot"
	code, body := doJSON(t, router, http.MethodGet, url, "", "")
	if code != http.StatusOK {
		t.Fatalf("Snapshot 失败: %d %+v", code, body)
	}
	snapshot := body["snapshot"].(map[string]any)
	workflows := snapshot["workflows"].([]any)
	if len(workflows) != 2 {
		t.Fatalf("应仅返回活动 Workflow + 最近一条终态: %+v", workflows)
	}
	seen := map[string]bool{}
	for _, value := range workflows {
		workflow := value.(map[string]any)
		seen[workflow["id"].(string)] = true
		if _, exists := workflow["tasks"]; exists {
			t.Fatal("Workflow 摘要不得嵌入历史 Task")
		}
	}
	if !seen[paused.ID] || !seen[latest.ID] || seen[old.ID] {
		t.Fatalf("快照业务范围错误: %+v", seen)
	}
	view := snapshot["workflow_view"].(map[string]any)
	if view["workflow"].(map[string]any)["id"] != paused.ID {
		t.Fatalf("详细 view 必须仍为活动 Workflow: %+v", view)
	}

	// 活动工作均结束后，仅留最近结果摘要，不把历史误判为占用 / 重置阻挡。
	stopping, err := st.TransitionWorkflow(paused.ID, paused.Revision, store.WorkflowStatusStopping, base.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ended, err := st.TransitionWorkflow(stopping.ID, stopping.Revision, store.WorkflowStatusStopped, base.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	code, body = doJSON(t, router, http.MethodGet, url, "", "")
	snapshot = body["snapshot"].(map[string]any)
	workflows = snapshot["workflows"].([]any)
	if code != http.StatusOK || len(workflows) != 1 || workflows[0].(map[string]any)["id"] != ended.ID || snapshot["workflow_view"] != nil {
		t.Fatalf("全终态时应仅保留最新结果摘要且无活动 view: %+v", snapshot)
	}
	code, body = doJSON(t, router, http.MethodGet, workflowURL(project.ID)+"/active", "", "")
	if code != http.StatusOK || body["workflow_view"] != nil {
		t.Fatalf("保留历史不得改变活动查询: %d %+v", code, body)
	}
	if unfinished, err := st.ListWorkflows(project.ID, false); err != nil || len(unfinished) != 0 {
		t.Fatalf("终态摘要不得产生 Workflow 占用: %+v %v", unfinished, err)
	}
	code, body = doJSON(t, router, http.MethodGet, workflowURL(project.ID)+"/"+ended.ID+"/view", "", "")
	if code != http.StatusOK || len(body["workflow_view"].(map[string]any)["tasks"].([]any)) != 1 {
		t.Fatalf("最近结果的详细 Task 仍能按既有 ID 读取: %d %+v", code, body)
	}
}
