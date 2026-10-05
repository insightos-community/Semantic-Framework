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

package pilot

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestSQLiteStoreRestoresSkillCheckpointAndActionJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pilot.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	skill := SkillExecution{ID: "skill-1", RobotID: "robot-1", SkillName: "grasp-object", SkillVersion: "0.1.0", Status: SkillRunning, Input: map[string]any{"object_ref": "box-1"}, Checkpoint: map[string]any{"stage": "grasp"}, CreatedAt: now, UpdatedAt: now}
	if err := store.SaveSkillExecution(skill); err != nil {
		t.Fatal(err)
	}
	action := ActionExecution{ID: "action-1", SkillExecutionID: "skill-1", Key: "grasp:1", RequestJSON: `{"robot_id":"robot-1"}`, RobotID: "robot-1", Action: ActionRef{Type: "gripper.close", SchemaVersion: 1}, InvocationID: "invocation-1", InstanceID: "gripper-r1", Physical: true, Status: ActionRunning, StartedAt: now, UpdatedAt: now}
	if err := store.SaveAction(action); err != nil {
		t.Fatal(err)
	}
	request := AgentRequest{ExecutionID: "skill-1", DecisionKey: "decision-1", Reason: "budget exhausted", CreatedAt: now}
	if err := store.SaveAgentRequest(request); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := NewSQLiteStore(reopened)
	if err != nil {
		t.Fatal(err)
	}
	gotSkill, err := restored.GetSkillExecution("skill-1")
	if err != nil {
		t.Fatal(err)
	}
	if gotSkill.Checkpoint["stage"] != "grasp" {
		t.Fatalf("checkpoint lost: %#v", gotSkill)
	}
	gotAction, err := restored.GetAction("skill-1", "grasp:1")
	if err != nil {
		t.Fatal(err)
	}
	if gotAction.InvocationID != "invocation-1" {
		t.Fatalf("invocation lost: %#v", gotAction)
	}
	active, err := restored.ListActiveActions()
	if err != nil || len(active) != 1 {
		t.Fatalf("active: %#v %v", active, err)
	}
	gotRequest, err := restored.GetAgentRequest("skill-1", "decision-1")
	if err != nil || gotRequest.Reason != "budget exhausted" {
		t.Fatalf("request: %#v %v", gotRequest, err)
	}

	gotSkill.Status = SkillStopped
	if err := restored.SaveSkillExecution(gotSkill); err != nil {
		t.Fatal(err)
	}
	if err := restored.UpdateSkillStopOutcome(gotSkill.ID, map[string]any{"safe": true}); err != nil {
		t.Fatal(err)
	}
	withOutcome, err := restored.GetSkillExecution(gotSkill.ID)
	if err != nil || withOutcome.Status != SkillStopped {
		t.Fatalf("停止证据更新不得回退终态: execution=%#v err=%v", withOutcome, err)
	}
	if safe, _ := withOutcome.StopOutcome["safe"].(bool); !safe {
		t.Fatalf("停止证据没有原子保存: %#v", withOutcome.StopOutcome)
	}
}

func TestSkillExecutionTerminalStateDoesNotRegress(t *testing.T) {
	tests := []struct {
		name  string
		store SkillExecutionStore
	}{
		{name: "memory", store: NewMemorySkillExecutionStore()},
	}

	path := filepath.Join(t.TempDir(), "terminal.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sqliteStore, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatal(err)
	}
	tests = append(tests, struct {
		name  string
		store SkillExecutionStore
	}{name: "sqlite", store: sqliteStore})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			completed := SkillExecution{ID: "rex-terminal", RobotID: "robot-1", SkillName: "place-object",
				SkillVersion: "0.3.0", Status: SkillCompleted, Result: map[string]any{"posture": "travel"},
				CreatedAt: now, UpdatedAt: now}
			if err := test.store.SaveSkillExecution(completed); err != nil {
				t.Fatal(err)
			}
			stale := completed
			stale.Status = SkillRunning
			stale.Result = nil
			stale.UpdatedAt = now.Add(time.Second)
			if err := test.store.SaveSkillExecution(stale); err != nil {
				t.Fatal(err)
			}
			got, err := test.store.GetSkillExecution(completed.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != SkillCompleted || got.Result["posture"] != "travel" {
				t.Fatalf("终态被旧快照回退: %#v", got)
			}
			recoverable, err := test.store.ListRecoverableSkillExecutions()
			if err != nil {
				t.Fatal(err)
			}
			if len(recoverable) != 0 {
				t.Fatalf("已完成执行仍被当作活动任务: %#v", recoverable)
			}
		})
	}
}
