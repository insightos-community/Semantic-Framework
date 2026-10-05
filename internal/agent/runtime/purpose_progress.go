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
	"encoding/json"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/store"
)

type purposeProgressKey struct{}
type purposeProgress struct {
	metadata RunMetadata
	offset   int
	turn     int
	output   strings.Builder
	summary  string
}

// Task protocols return machine JSON, not conversational text. Publish explicit
// model reasoning and tool activity while it is happening, keeping protocol
// output in the Task transcript. The public plan summary remains the scheduler's.
func (s *Service) publishPurposeProgress(ctx context.Context, rt *sessionRuntime, run store.RunSession, ev kernel.Event) {
	p, _ := ctx.Value(purposeProgressKey{}).(*purposeProgress)
	if p == nil || ev.AgentID != "" {
		return
	}
	ref := eventContextFromRun(run)
	switch ev.Kind {
	case kernel.EventTextDelta:
		p.output.WriteString(ev.Text)
		summary := partialPurposeSummary(p.output.String())
		if summary != "" && summary != p.summary {
			reset := !strings.HasPrefix(summary, p.summary)
			delta := summary
			if !reset {
				delta = strings.TrimPrefix(summary, p.summary)
			}
			p.summary = summary
			s.publish(ref, rt, EventTypeMessageDelta, MessageDeltaPayload{RunID: run.ID, Text: delta, Reset: reset})
		}
	case kernel.EventReasoningDelta:
		if rt.profile.ReasoningVisibility == "hide" {
			return
		}
		turn := p.offset + max(1, ev.Turns)
		p.turn = max(p.turn, turn)
		p.metadata.Reasoning += ev.Text
		rounds := &p.metadata.ReasoningRounds
		if len(*rounds) == 0 || (*rounds)[len(*rounds)-1].Turn != turn {
			*rounds = append(*rounds, ReasoningRound{Turn: turn})
		}
		(*rounds)[len(*rounds)-1].Text += ev.Text
		s.publish(ref, rt, EventTypeReasoningDelta, ReasoningDeltaPayload{RunID: run.ID, Text: ev.Text, Turn: turn})
	case kernel.EventToolCall:
		p.metadata.Tools = append(p.metadata.Tools, ToolActivity{CallID: ev.CallID, Name: ev.ToolName, Arguments: ev.Arguments, Status: "running"})
		s.publish(ref, rt, EventTypeToolCall, ToolCallPayload{RunID: run.ID, CallID: ev.CallID, Name: ev.ToolName, Arguments: ev.Arguments})
	case kernel.EventToolResult:
		result, truncated := boundedToolResult(ev.Text)
		for i := len(p.metadata.Tools) - 1; i >= 0; i-- {
			tool := &p.metadata.Tools[i]
			if tool.CallID == ev.CallID && tool.Name == ev.ToolName {
				tool.Status = "done"
				tool.Result = result
				tool.Truncated = truncated
				break
			}
		}
		s.publish(ref, rt, EventTypeToolResult, ToolResultPayload{RunID: run.ID, CallID: ev.CallID, Name: ev.ToolName, Result: result, Truncated: truncated})
	case kernel.EventDone:
		p.offset = max(p.turn, p.offset+ev.Turns)
		p.output.Reset()
	}
}

func (s *Service) finishPurposeProgress(rt *sessionRuntime, run store.RunSession, p *purposeProgress) {
	if p == nil || (p.metadata.Reasoning == "" && len(p.metadata.Tools) == 0 && p.summary == "") {
		return
	}
	if latest, err := s.st.GetRunSession(run.ID); err == nil {
		run = latest
	}
	p.metadata.Status = run.Status
	p.metadata.StartedAt = run.StartedAt.Format(time.RFC3339Nano)
	p.metadata.Error = run.Error
	p.metadata.Turns = p.offset
	text := "执行准备过程"
	if run.Kind == store.RunKindTaskPlanning {
		text = "任务规划过程"
	}
	if p.summary != "" {
		text = strings.TrimSpace(p.summary)
	}
	encoded, err := json.Marshal(p.metadata)
	if err != nil {
		return
	}
	if err := s.st.AppendChatMessage(store.ChatMessage{ID: store.NewChatMessageID(), SessionID: run.ChatSessionID,
		AgentID: run.AgentID, RunID: run.ID, TraceID: run.TraceID, Message: schema.AssistantMessage(text, nil),
		Metadata: string(encoded), CreatedAt: time.Now().UTC()}); err != nil {
		s.logger.WithError(err).Error("Task 运行活动保存失败", "run_id", run.ID)
	}
	s.publish(eventContextFromRun(run), rt, EventTypeMessageDone, MessageDonePayload{RunID: run.ID, TraceID: run.TraceID,
		Text: text, Turns: p.offset, Error: run.Error, Cancelled: run.Status == store.RunStatusCancelled})
}

// Decode only the public top-level summary string, including a partial final
// JSON string. Never stream SubTask specifications or tool protocol output.
func partialPurposeSummary(body string) string {
	d := json.NewDecoder(strings.NewReader(strings.TrimSpace(body)))
	trimmed := strings.TrimSpace(body)
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return ""
	}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return ""
		}
		if key == "summary" {
			raw := strings.TrimSpace(trimmed[d.InputOffset():])
			if !strings.HasPrefix(raw, ":") {
				return ""
			}
			raw = strings.TrimSpace(raw[1:])
			if !strings.HasPrefix(raw, `"`) {
				return ""
			}
			escaped := false
			for i := 1; i < len(raw); i++ {
				if !escaped && raw[i] == '"' {
					var text string
					if json.Unmarshal([]byte(raw[:i+1]), &text) == nil {
						return text
					}
					return ""
				}
				if !escaped && raw[i] == '\\' {
					escaped = true
				} else {
					escaped = false
				}
			}
			// A chunk may end halfway through an escape or a UTF-8 code point.
			for n := len(raw); n >= 1 && n >= len(raw)-12; n-- {
				var text string
				if json.Unmarshal([]byte(raw[:n]+`"`), &text) == nil {
					return text
				}
			}
			return ""
		}
		var ignored json.RawMessage
		if d.Decode(&ignored) != nil {
			return ""
		}
	}
	return ""
}
