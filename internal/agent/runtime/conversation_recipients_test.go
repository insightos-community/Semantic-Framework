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

package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"insightos.cn/semantic-framework/internal/robotruntime"
	"insightos.cn/semantic-framework/internal/store"
)

func TestConversationRecipientSharesTranscriptNotTaskContext(t *testing.T) {
	fx := newTestFixture(t)
	svc := NewService(Deps{Profiles: newLoaderForRoot(t, writeTeamProfiles(t)),
		LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger})
	if err := svc.AssembleTeam(context.Background(), testDef()); err != nil {
		t.Fatal(err)
	}
	defer svc.Shutdown()
	leaderRunID, err := svc.HandleMessage(context.Background(), "u-route", "", "共同约定：先确认状态")
	if err != nil {
		t.Fatal(err)
	}
	leaderRun, _ := fx.st.GetRunSession(leaderRunID)
	runID, err := svc.HandleMessageToAgent(context.Background(), "u-route", leaderRun.ChatSessionID,
		"解释上一条约定", nil, "auto", "auto", "collaboration", "query-1")
	if err != nil {
		t.Fatal(err)
	}
	run, _ := fx.st.GetRunSession(runID)
	if run.AgentID != "query-1" || run.Kind != store.RunKindConversation || run.TaskID != "" || run.ContextID != leaderRun.ContextID {
		t.Fatalf("direct conversation routing: %+v", run)
	}
	records, err := fx.st.ListChatMessagesAfter(run.ChatSessionID, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if records[len(records)-1].AgentID != "query-1" {
		t.Fatalf("lost author: %+v", records[len(records)-1])
	}
	history, err := svc.loadContextHistory(run.ChatSessionID, false, "leader")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, message := range history.Messages {
		if strings.Contains(message.Content, "[Agent query-1 的公开回复]") {
			found = true
			if message.Role != schema.User || len(message.ToolCalls) != 0 {
				t.Fatal("imported another Agent tool protocol")
			}
		}
	}
	if !found {
		t.Fatal("shared transcript lost Agent attribution")
	}
	before := len(records)
	if _, err := svc.HandleMessageToAgent(context.Background(), "u-route", run.ChatSessionID,
		"错误地址", nil, "", "", "collaboration", "robot:not-registered"); err == nil {
		t.Fatal("unregistered Robot silently routed")
	}
	records, _ = fx.st.ListChatMessagesAfter(run.ChatSessionID, "", 0)
	if len(records) != before {
		t.Fatal("invalid recipient persisted a message")
	}
}

func TestConversationRecipientsFilterProjectAndRobotInstance(t *testing.T) {
	fx := newTestFixture(t)
	svc := NewService(Deps{Profiles: newLoaderForRoot(t, writeTeamProfiles(t)),
		LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger})
	if err := svc.AssembleTeam(context.Background(), testDef()); err != nil {
		t.Fatal(err)
	}
	defer svc.Shutdown()
	sess, err := svc.loadOrCreateSession("u-filter", "", "目录")
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.st.SaveRobotPilot(store.RobotPilot{PilotInstanceID: "pilot-other", RobotID: "other", Status: "online", LastSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := fx.st.SaveRuntimeInstance(context.Background(), robotruntime.RuntimeInstance{
		InstanceID: "instance-other", PilotInstanceID: "pilot-other", RobotID: "other", ProjectID: "other-project", Status: robotruntime.StateReady,
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.ListConversationRecipients("u-filter", sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == "robot:other" {
			t.Fatal("another project Robot exposed")
		}
	}
	if _, err := svc.ListConversationRecipients("other-user", sess.ID); err != ErrSessionNotFound {
		t.Fatalf("ownership: %v", err)
	}
}

func TestConversationRecipientsUseLatestRuntimePerRobot(t *testing.T) {
	for _, tc := range []struct {
		name            string
		oldProject      string
		latestProject   string
		latestPilot     string
		registeredPilot string
		wantRecipient   bool
	}{
		{"same_pilot_moved_out", "current", "other", "pilot-shared", "pilot-shared", false},
		{"same_pilot_moved_in", "other", "current", "pilot-shared", "pilot-shared", true},
		{"old_pilot_does_not_restore_old_project", "current", "other", "pilot-new", "pilot-shared", false},
		{"unrecorded_pilot_cannot_bypass_project", "current", "other", "pilot-new", "pilot-unrecorded", false},
		{"latest_unbound_does_not_restore_old_project", "other", "", "pilot-shared", "pilot-shared", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newTestFixture(t)
			svc := NewService(Deps{Profiles: newLoaderForRoot(t, writeTeamProfiles(t)),
				LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger})
			defer svc.Shutdown()
			sess, err := svc.loadOrCreateSession("u-runtime-owner", "", "目录")
			if err != nil {
				t.Fatal(err)
			}
			projectID := func(value string) string {
				if value == "current" {
					return sess.ProjectID
				}
				return value
			}
			now := time.Now().UTC()
			for _, instance := range []robotruntime.RuntimeInstance{
				{InstanceID: "runtime-old", PilotInstanceID: "pilot-shared", RobotID: "migrated",
					ProjectID: projectID(tc.oldProject), Status: robotruntime.StateStopped, UpdatedAt: now.Add(-time.Hour)},
				{InstanceID: "runtime-latest", PilotInstanceID: tc.latestPilot, RobotID: "migrated",
					ProjectID: projectID(tc.latestProject), Status: robotruntime.StateReady, UpdatedAt: now},
			} {
				if err := fx.st.SaveRuntimeInstance(context.Background(), instance); err != nil {
					t.Fatal(err)
				}
			}
			if err := fx.st.SaveRobotPilot(store.RobotPilot{PilotInstanceID: tc.registeredPilot,
				RobotID: "migrated", Status: "online", LastSeenAt: now}); err != nil {
				t.Fatal(err)
			}
			latest, err := fx.st.GetLatestRuntimeByRobot(context.Background(), "migrated")
			if err != nil || latest.InstanceID != "runtime-latest" {
				t.Fatalf("latest runtime: %+v, %v", latest, err)
			}
			rows, err := svc.ListConversationRecipients("u-runtime-owner", sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, row := range rows {
				found = found || row.ID == "robot:migrated"
			}
			if found != tc.wantRecipient {
				t.Fatalf("recipient present = %v, want %v; latest Robot project = %q, current = %q",
					found, tc.wantRecipient, latest.ProjectID, sess.ProjectID)
			}
			_, err = svc.conversationRecipient("u-runtime-owner", sess.ID, "robot:migrated")
			if (err == nil) != tc.wantRecipient {
				t.Fatalf("recipient routing must use the same project filter: %v", err)
			}
		})
	}
}
