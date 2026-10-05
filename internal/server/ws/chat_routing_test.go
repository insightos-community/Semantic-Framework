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
