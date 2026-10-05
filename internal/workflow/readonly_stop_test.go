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

package workflow

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

func TestStopFailedExecutionClassifiesPhysicalEvidence(t *testing.T) {
	for _, tc := range []struct {
		name         string
		payload      map[string]any
		missingStart bool
		late         bool
		wantStopped  bool
	}{
		{"read_only", map[string]any{"physical": false, "physical_started": false}, false, false, true},
		{"physical", map[string]any{"physical": true, "physical_started": true}, false, false, false},
		{"physical_start_unconfirmed", map[string]any{"physical": true, "physical_started": false}, false, false, false},
		{"legacy_unknown", map[string]any{}, false, false, false},
		{"missing_start", map[string]any{}, true, false, false},
		{"physical_after_first_page", map[string]any{"physical": true, "physical_started": true}, false, true, false},
	} {
		for _, observed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/observer=%v", tc.name, observed), func(t *testing.T) {
				st, project, conversation, service, _, _ := newWorkflowFixture(t)
				view, task, subs := prepareRunningRobotTask(t, st, project, conversation,
					[]store.SubTaskDraft{{ID: "sub-failed", Kind: "robot_skill", Goal: "抓取"}})
				now := time.Now().UTC()
				execution := store.RobotExecution{ID: "rex-failed", ProjectID: project.ID,
					WorkflowID: view.Workflow.ID, TaskID: task.ID, SubtaskID: subs[0].ID,
					RobotID: "r1pro-test", SkillName: "grasp-object", SkillVersion: "0.4.22",
					Status: "failed", Error: map[string]any{"code": "WORKER_EXITED"},
					Revision: 2, CreatedAt: now, UpdatedAt: now}
				if err := st.SaveRobotExecution(execution); err != nil {
					t.Fatal(err)
				}
				sub, err := st.AttachSubTaskExecution(subs[0].ID, subs[0].Revision, execution.ID, now)
				if err != nil {
					t.Fatal(err)
				}
				var sequence int64
				appendEvent := func(kind string, payload map[string]any) {
					t.Helper()
					sequence++
					if err := st.AppendRobotExecutionEvent(store.RobotExecutionEvent{
						ExecutionID: execution.ID, Sequence: sequence, Type: kind, Payload: payload, CreatedAt: now,
					}); err != nil {
						t.Fatal(err)
					}
				}
				appendEvent("action.started", map[string]any{"action_id": "capture", "physical": false, "physical_started": false})
				appendEvent("action.terminal", map[string]any{"action_id": "capture", "status": "succeeded"})
				if tc.late {
					for sequence < 1000 {
						appendEvent("skill.log", map[string]any{})
					}
				}
				payload := map[string]any{"action_id": "candidate"}
				for k, v := range tc.payload {
					payload[k] = v
				}
				if !tc.missingStart {
					appendEvent("action.started", payload)
				}
				appendEvent("action.terminal", map[string]any{"action_id": "candidate", "status": "interrupted"})
				if observed {
					if _, err := st.TransitionWorkflowWithReason(view.Workflow.ID, view.Workflow.Revision, store.WorkflowStatusStopping, "user_stopped", now); err != nil {
						t.Fatal(err)
					}
					if _, err := service.moveTaskTowardStop(task); err != nil {
						t.Fatal(err)
					}
					if _, err := service.moveSubTaskTowardStop(sub); err != nil {
						t.Fatal(err)
					}
					err = service.OnRobotExecutionChanged(context.Background(), execution, "execution.terminal", execution.Error)
				} else {
					if tc.wantStopped {
						// Reproduce the installed version's paused/unknown state,
						// then retry stop after upgrading without changing history.
						if err := service.pauseRobotSubTask(task, sub, "execution_state_unknown", nil); err != nil {
							t.Fatal(err)
						}
					}
					current, err := st.GetWorkflow(view.Workflow.ID)
					if err != nil {
						t.Fatal(err)
					}
					// 普通 stop 对"物理状态未知"必须明确失败，让前端走人工确认；
					// 不能返回 200 让界面以为已经停住。
					_, err = service.StopWorkflow(context.Background(), project.OwnerID, project.ID, view.Workflow.ID, current.Revision)
					if tc.wantStopped {
						if err != nil {
							t.Fatal(err)
						}
					} else {
						if !errors.Is(err, store.ErrOperatorConfirmationRequired) {
							t.Fatalf("物理状态未知必须要求人工确认: %v", err)
						}
						err = nil
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				result, err := st.GetWorkflowView(view.Workflow.ID)
				if err != nil {
					t.Fatal(err)
				}
				if tc.wantStopped {
					if result.Workflow.Status != store.WorkflowStatusStopped {
						t.Fatalf("read-only failure did not stop: %+v", result.Workflow)
					}
					// The failed execution and its error must remain in history.
					stored, _ := st.GetRobotExecution(execution.ID)
					if stored.Status != "failed" || stored.Error["code"] != "WORKER_EXITED" {
						t.Fatalf("lost failure: %+v", stored)
					}
					if _, err := service.StopWorkflow(context.Background(), project.OwnerID, project.ID, view.Workflow.ID, view.Workflow.Revision); err != nil {
						t.Fatal(err)
					}
				} else if result.Workflow.Status != store.WorkflowStatusPaused || result.Workflow.Reason != "execution_state_unknown" {
					t.Fatalf("unsafe execution bypassed confirmation: %+v", result.Workflow)
				}
			})
		}
	}
}
