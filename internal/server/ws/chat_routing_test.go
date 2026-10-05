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
	"encoding/json"
	"testing"
)

type routedMessageRecorder struct{ agentID, mode, sessionID string }

func (h *routedMessageRecorder) HandleMessage(context.Context, string, string, string) (string, error) {
	panic("routing was lost")
}
func (h *routedMessageRecorder) HandleMessageToAgent(_ context.Context, _, sessionID, _ string, _ []string, _, _, mode, agentID string) (string, error) {
	h.agentID, h.mode, h.sessionID = agentID, mode, sessionID
	return "run-routed", nil
}

func TestBothChatGatewaysForwardStructuredRecipient(t *testing.T) {
	var message uplinkMessage
	if err := json.Unmarshal([]byte(`{"session_id":"cs-1","text":"查询状态","send_scope":{"type":"conversation","target_agent_id":"robot:arm-1","intent":"message"}}`), &message); err != nil {
		t.Fatal(err)
	}
	for _, studio := range []bool{false, true} {
		handler := &routedMessageRecorder{}
		connection := newTestConn()
		if studio {
			gateway := &StudioGateway{handler: handler}
			gateway.executeMessage(connection, "u-1", message)
		} else {
			gateway := &ChatGateway{handler: handler}
			gateway.handleChatMessage(connection, "u-1", message.SessionID, message.Text, nil, "", "", message.conversationMode(), message.SendScope.TargetAgentID)
		}
		if handler.agentID != "robot:arm-1" || handler.mode != "collaboration" || handler.sessionID != "cs-1" {
			t.Fatalf("routed fields lost: %+v", handler)
		}
	}
}
