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

package store

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func directAdmissionFixture(t *testing.T, st *Store) (Project, ChatSession, RobotExecution) {
	t.Helper()
	project, session := v030ProjectConversation(t, st)
	now := time.Now().UTC()
	if err := st.CreateRunSession(RunSession{ID: "run-direct", ProjectID: project.ID,
		ChatSessionID: session.ID, AgentID: "robot:robot-direct", Status: RunStatusRunning}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRobotPilot(RobotPilot{PilotInstanceID: "pilot-direct", RobotID: "robot-direct",
		RobotStatus: "idle", Status: "online", LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	return project, session, RobotExecution{ID: "exec-direct", ProjectID: project.ID,
		RunID: "run-direct", RobotID: "robot-direct", PilotInstanceID: "pilot-direct",
		SkillName: "skill", SkillVersion: "1", RequestKey: "direct-1", Status: "queued",
		Revision: 1, CreatedAt: now, UpdatedAt: now}
}

func TestDirectRunOwnsRobotBetweenSkillsAndReleasesAtTerminal(t *testing.T) {
	st := openTestStore(t)
	project, session, first := directAdmissionFixture(t, st)
	view := approveSingleTaskProposal(t, st, project, session, "等待 Robot", "robot", time.Now().UTC())
	if run, err := st.GetRunSession(first.RunID); err != nil || run.RobotID != "" {
		t.Fatalf("普通 Robot 对话不能预先占用: %+v %v", run, err)
	}
	if _, created, err := st.AdmitRobotExecution(first, "robot:robot-direct"); err != nil || !created {
		t.Fatalf("首次直接动作准入: created=%v err=%v", created, err)
	}
	first.Status = "completed"
	if err := st.SaveRobotExecution(first); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssignTask(view.Tasks[0].ID, view.Tasks[0].Revision,
		"robot:robot-direct", first.RobotID, time.Now().UTC()); !errors.Is(err, ErrRobotReserved) {
		t.Fatalf("模型等待期间 Task 不得抢占: %v", err)
	}
	independent := first
	independent.ID, independent.RequestKey, independent.RunID, independent.Status = "exec-http", "http", "", "queued"
	if _, _, err := st.AdmitRobotExecution(independent, ""); !errors.Is(err, ErrRobotReserved) {
		t.Fatalf("模型等待期间 HTTP 不得抢占: %v", err)
	}
	second := first
	second.ID, second.RequestKey, second.Status = "exec-direct-2", "direct-2", "queued"
	if _, created, err := st.AdmitRobotExecution(second, "robot:robot-direct"); err != nil || !created {
		t.Fatalf("同一 Run 下一 Skill 应可继续: created=%v err=%v", created, err)
	}
	second.Status = "completed"
	if err := st.SaveRobotExecution(second); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FinishRunSession(first.RunID, nil, RunStatusCompleted, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssignTask(view.Tasks[0].ID, view.Tasks[0].Revision,
		"robot:robot-direct", first.RobotID, time.Now().UTC()); err != nil {
		t.Fatalf("Run 和物理执行结束后应可调度: %v", err)
	}
}

func TestTaskAndDirectRunAdmissionRaceHasOneWinner(t *testing.T) {
	for round := 0; round < 10; round++ {
		st := openTestStore(t)
		project, session, execution := directAdmissionFixture(t, st)
		view := approveSingleTaskProposal(t, st, project, session, "竞抢 Robot", "robot", time.Now().UTC())
		start := make(chan struct{})
		results := make(chan error, 2)
		go func() {
			<-start
			_, _, err := st.AdmitRobotExecution(execution, "robot:robot-direct")
			results <- err
		}()
		go func() {
			<-start
			_, err := st.AssignTask(view.Tasks[0].ID, view.Tasks[0].Revision,
				"robot:robot-direct", execution.RobotID, time.Now().UTC())
			results <- err
		}()
		close(start)
		first, second := <-results, <-results
		if (first == nil) == (second == nil) {
			t.Fatalf("只能有一个准入赢家: %v / %v", first, second)
		}
		if first != nil && !errors.Is(first, ErrRobotReserved) || second != nil && !errors.Is(second, ErrRobotReserved) {
			t.Fatalf("竞抢失败必须报告占用: %v / %v", first, second)
		}
	}
}

func TestConcurrentHTTPAdmissionCannotCreateTwoExecutions(t *testing.T) {
	st := openTestStore(t)
	_, _, execution := directAdmissionFixture(t, st)
	execution.RunID = ""
	var wg sync.WaitGroup
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, id := range []string{"exec-http-1", "exec-http-2"} {
		item := execution
		item.ID, item.RequestKey = id, id
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := st.AdmitRobotExecution(item, "")
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	first, second := <-results, <-results
	if (first == nil) == (second == nil) {
		t.Fatalf("独立 HTTP 请求不能同时准入: %v / %v", first, second)
	}
}

func TestCancelledDirectRunAndStoppedRequestCannotLaunchAgain(t *testing.T) {
	st := openTestStore(t)
	_, _, execution := directAdmissionFixture(t, st)
	if _, err := st.TransitionRunStatus(execution.RunID, nil, RunStatusCancelling, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AdmitRobotExecution(execution, "robot:robot-direct"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("取消先于准入时不得下发: %v", err)
	}
	if rows, err := st.ListRobotExecutionsByRun(execution.RunID); err != nil || len(rows) != 0 {
		t.Fatalf("被拒绝动作不能生成执行或队列: %v %v", rows, err)
	}
}

func TestTaskCannotTakeRobotAfterRunEndsWithUnconfirmedPhysicalState(t *testing.T) {
	st := openTestStore(t)
	project, session, execution := directAdmissionFixture(t, st)
	view := approveSingleTaskProposal(t, st, project, session, "等待安全对账", "robot", time.Now().UTC())
	if _, _, err := st.AdmitRobotExecution(execution, "robot:robot-direct"); err != nil {
		t.Fatal(err)
	}
	execution.Status = "interrupted"
	if err := st.SaveRobotExecution(execution); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FinishRunSession(execution.RunID, nil, RunStatusFailed, "disconnected", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	pilot, err := st.GetActiveRobotPilot(execution.RobotID)
	if err != nil {
		t.Fatal(err)
	}
	pilot.CurrentExecutionID, pilot.RobotStatus = execution.ID, "interrupted"
	if err := st.SaveRobotPilot(pilot); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssignTask(view.Tasks[0].ID, view.Tasks[0].Revision,
		"robot:robot-direct", execution.RobotID, time.Now().UTC()); !errors.Is(err, ErrRobotReserved) {
		t.Fatalf("Run终态不能释放尚未对账的Pilot物理占用: %v", err)
	}
}
