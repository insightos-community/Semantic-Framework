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

package ws

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"insightos.cn/semantic-framework/pkg/log"
)

type fakeStudioAccess struct {
	writable bool
}

func (f *fakeStudioAccess) ProjectOwnedByUser(userID, projectID string) error {
	if userID == "usr-1" && projectID == "proj-1" {
		return nil
	}
	return errors.New("not found")
}

func (f *fakeStudioAccess) ProjectWritableByUser(userID, projectID string) error {
	if f.writable {
		return f.ProjectOwnedByUser(userID, projectID)
	}
	return errors.New("inactive")
}

func (f *fakeStudioAccess) ConversationBelongsToProject(_, projectID, sessionID string) error {
	if projectID == "proj-1" && sessionID == "cs-1" {
		return nil
	}
	return errors.New("not found")
}

func (f *fakeStudioAccess) RunBelongsToProject(_, projectID, runID string) error {
	if projectID == "proj-1" && runID == "run-1" {
		return nil
	}
	return errors.New("not found")
}

func (f *fakeStudioAccess) InteractionBelongsToProject(_, projectID, interactionID string) error {
	if projectID == "proj-1" && interactionID == "int-1" {
		return nil
	}
	return errors.New("not found")
}

type fakeStudioHandler struct {
	message chan string
	runID   string
}

type fakeStudioInteractionHandler struct {
	*fakeReplyHandler
	cancelled []string
}

func (f *fakeStudioInteractionHandler) CancelStructured(_ context.Context,
	userID, projectID, interactionID string) error {
	f.cancelled = append(f.cancelled, userID+"/"+projectID+"/"+interactionID)
	return nil
}

func (f *fakeStudioHandler) HandleMessage(_ context.Context, _, _, text string) (string, error) {
	f.message <- text
	return "run-new", nil
}

func (f *fakeStudioHandler) CancelRunByID(_ context.Context, _, _, runID string) error {
	f.runID = runID
	return nil
}

type fakeProjectReplayer struct{}

func (fakeProjectReplayer) ReplayProjectEvents(projectID string, afterSequence int64) ([]Envelope, error) {
	return []Envelope{{
		ID: "evt-3", ProjectID: projectID, Sequence: afterSequence + 1,
		Channel: ChannelDialogue, Type: "run.updated",
	}}, nil
}

func TestStudioGatewayCommandsAndSync(t *testing.T) {
	handler := &fakeStudioHandler{message: make(chan string, 1)}
	access := &fakeStudioAccess{writable: true}
	replies := &fakeStudioInteractionHandler{fakeReplyHandler: &fakeReplyHandler{}}
	gateway := &StudioGateway{
		handler: handler, access: access, replies: replies,
		syncer: fakeProjectReplayer{},
		logger: log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
	}
	connection := newTestConn()

	gateway.handleUplink(connection, "usr-1", "proj-1", newUplink(t, map[string]any{
		"type": "chat.message", "session_id": "cs-1", "text": "继续验证",
	}))
	select {
	case text := <-handler.message:
		if text != "继续验证" {
			t.Fatalf("消息内容未透传: %q", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Studio 消息未进入 Runtime")
	}

	gateway.handleUplink(connection, "usr-1", "proj-1", newUplink(t, map[string]any{
		"type": "run.cancel", "run_id": "run-1",
	}))
	if handler.runID != "run-1" {
		t.Fatalf("必须按明确 Run ID 取消，实际: %q", handler.runID)
	}

	gateway.handleUplink(connection, "usr-1", "proj-1", newUplink(t, map[string]any{
		"type": "interaction.reply", "interaction_id": "int-1", "approved": true,
	}))
	if len(replies.calls) != 1 || replies.calls[0][0] != "int-1" {
		t.Fatalf("Interaction 应答未透传: %+v", replies.calls)
	}
	gateway.handleUplink(connection, "usr-1", "proj-1", newUplink(t, map[string]any{
		"type": "interaction.cancel", "interaction_id": "int-1",
	}))
	if len(replies.cancelled) != 1 || replies.cancelled[0] != "usr-1/proj-1/int-1" {
		t.Fatalf("Interaction取消未透传: %+v", replies.cancelled)
	}

	gateway.handleUplink(connection, "usr-1", "proj-1", newUplink(t, map[string]any{
		"type": "sync", "after_sequence": 2,
	}))
	first := <-connection.send
	event, ok := first.(Envelope)
	if !ok || event.Sequence != 3 || event.ProjectID != "proj-1" {
		t.Fatalf("补发事件不符: %#v", first)
	}
	second := <-connection.send
	done, ok := second.(studioSyncDoneReply)
	if !ok || done.Count != 1 || done.LastSequence != 3 {
		t.Fatalf("sync.done 不符: %#v", second)
	}
}

func TestStudioGatewayRejectsCrossProjectAndInactiveWrites(t *testing.T) {
	handler := &fakeStudioHandler{message: make(chan string, 1)}
	access := &fakeStudioAccess{writable: false}
	gateway := &StudioGateway{
		handler: handler, access: access,
		logger: log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
	}
	connection := newTestConn()

	gateway.handleUplink(connection, "usr-1", "proj-1", newUplink(t, map[string]any{
		"type": "chat.message", "session_id": "cs-1", "text": "不能写入",
	}))
	if reply := readReply(t, connection); reply.Code != "PROJECT_INACTIVE" {
		t.Fatalf("非活动 Project 应拒绝写入: %+v", reply)
	}
	select {
	case <-handler.message:
		t.Fatal("非活动 Project 的消息不得进入 Runtime")
	default:
	}

	access.writable = true
	gateway.handleUplink(connection, "usr-1", "proj-1", newUplink(t, map[string]any{
		"type": "run.cancel", "run_id": "run-other",
	}))
	if reply := readReply(t, connection); reply.Code != CodeWSBadMessage {
		t.Fatalf("跨 Project Run 应被拒绝: %+v", reply)
	}
}

func TestHubProjectDeliveryDeduplicatesSessionSubscription(t *testing.T) {
	hub := NewHub(log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	connection := newTestConn()
	hub.Subscribe("cs-1", connection)
	hub.SubscribeProject("proj-1", connection)
	envelope := NewEnvelope("cs-1", ChannelDialogue, "message.done",
		ImportanceNormal, nil)
	envelope.ProjectID = "proj-1"
	hub.Publish(envelope)

	select {
	case <-connection.send:
	case <-time.After(time.Second):
		t.Fatal("Project 连接未收到事件")
	}
	select {
	case duplicate := <-connection.send:
		t.Fatalf("同一连接同时命中 Session/Project 时不应重复投递: %#v", duplicate)
	default:
	}
}
