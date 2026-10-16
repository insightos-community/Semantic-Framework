package runtime

import (
	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/agent/kernel"
)

// A format correction is another model exchange in the same Run. Preserve
// completed tool calls as tool messages: dropping them loses the contracts and
// source facts used to produce the plan. No tool is re-executed, no reasoning is
// retained, and these messages do not become fresh observations in a later Run.
type purposeExchangeEvidence struct {
	pending  map[string]kernel.Event
	messages []*schema.Message
}

func (e *purposeExchangeEvidence) observe(event kernel.Event) {
	if event.CallID == "" {
		return
	}
	switch event.Kind {
	case kernel.EventToolCall:
		if e.pending == nil {
			e.pending = make(map[string]kernel.Event)
		}
		e.pending[event.CallID] = event
	case kernel.EventToolResult:
		call, ok := e.pending[event.CallID]
		if !ok {
			return
		}
		delete(e.pending, event.CallID)
		e.messages = append(e.messages,
			schema.AssistantMessage("", []schema.ToolCall{{
				ID: call.CallID, Type: "function",
				Function: schema.FunctionCall{Name: call.ToolName, Arguments: call.Arguments},
			}}),
			schema.ToolMessage(event.Text, event.CallID))
	}
}
