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
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
)

func TestPurposeProgressStreamsActualReasoningBeforeDone(t *testing.T) {
	fx := newTestFixture(t)
	svc := NewService(Deps{Store: fx.st, Bus: fx.bus, Logger: fx.logger})
	rt := &sessionRuntime{profile: &profile.Profile{Name: "robot", Mode: profile.ModeWorker}}
	run := store.RunSession{ID: "task-run", AgentID: "robot:r1", ProjectID: "project", ChatSessionID: "conversation", TraceID: "trace"}
	p := &purposeProgress{}
	ctx := context.WithValue(context.Background(), purposeProgressKey{}, p)
	svc.publishPurposeProgress(ctx, rt, run, kernel.Event{Kind: kernel.EventReasoningDelta, Text: "checking target", Turns: 1})
	select {
	case event := <-fx.events:
		env := event.Payload.(ws.Envelope)
		payload := env.Payload.(ReasoningDeltaPayload)
		if env.Type != EventTypeReasoningDelta || env.Agent.ID != run.AgentID || env.Parent.TraceID != run.TraceID || payload.Turn != 1 {
			t.Fatalf("incorrect correlation: %+v", env)
		}
	case <-time.After(time.Second):
		t.Fatal("reasoning buffered until completion")
	}
	svc.publishPurposeProgress(ctx, rt, run, kernel.Event{Kind: kernel.EventDone, Turns: 1})
	svc.publishPurposeProgress(ctx, rt, run, kernel.Event{Kind: kernel.EventReasoningDelta, Text: "correcting input", Turns: 1})
	if len(p.metadata.ReasoningRounds) != 2 || p.metadata.ReasoningRounds[1].Turn != 2 {
		t.Fatalf("lost correction round: %+v", p.metadata)
	}
	rt.profile.ReasoningVisibility = "hide"
	svc.publishPurposeProgress(ctx, rt, run, kernel.Event{Kind: kernel.EventReasoningDelta, Text: "hidden", Turns: 2})
	if p.metadata.Reasoning != "checking targetcorrecting input" {
		t.Fatal("hidden reasoning leaked")
	}
	svc.publishPurposeProgress(ctx, rt, run, kernel.Event{Kind: kernel.EventTextDelta, Text: `{"summary":"public","subtasks":[{"private":"protocol"}]}`})
	if p.metadata.Reasoning != "checking targetcorrecting input" {
		t.Fatal("protocol JSON entered reasoning")
	}
}

func TestPartialPurposeSummaryNeverLeaksProtocol(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{`{"summary":"first`, "first"},
		{`{"summary":"first\nsecond","subtasks":[{"secret":"not public"}]}`, "first\nsecond"},
		{`{"summary":"say \"hi\"`, `say "hi"`},
		{`{"summary":"partial\u4e`, "partial"},
		{`{"nested":{"summary":"private"},"summary":"public`, "public"},
		{`{"subtasks":[{"summary":"private"}]}`, ""},
		{`{"summary":{"private":true}}`, ""},
		{`not JSON`, ""},
	} {
		if got := partialPurposeSummary(tc.input); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.input, got, tc.want)
		}
	}
}
