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
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type agentGatewayResult struct {
	reply AgentReply
	err   error
}

func TestRemoteAgentGatewayCancelExecutionDoesNotAffectAnotherRobot(t *testing.T) {
	reported := make(chan AgentRequest, 2)
	gateway := NewRemoteAgentGateway(func(request AgentRequest) error {
		reported <- request
		return nil
	})
	results := map[string]chan agentGatewayResult{
		"rex-a": make(chan agentGatewayResult, 1),
		"rex-b": make(chan agentGatewayResult, 1),
	}
	for _, executionID := range []string{"rex-a", "rex-b"} {
		executionID := executionID
		go func() {
			reply, err := gateway.Request(context.Background(), AgentRequest{
				ExecutionID: executionID,
				DecisionKey: "decision-1",
			})
			results[executionID] <- agentGatewayResult{reply: reply, err: err}
		}()
	}
	for range 2 {
		select {
		case <-reported:
		case <-time.After(time.Second):
			t.Fatal("AgentRequest 未进入等待状态")
		}
	}

	gateway.CancelExecution("rex-a")
	select {
	case result := <-results["rex-a"]:
		if result.err == nil || !strings.Contains(result.err.Error(), "停止而取消") {
			t.Fatalf("停止中的 Execution 应取消等待: %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("取消 AgentRequest 后没有唤醒等待方")
	}
	if err := gateway.Resolve(AgentReply{ExecutionID: "rex-a", DecisionKey: "decision-1"}); err == nil {
		t.Fatal("已取消的 AgentRequest 不应再接受迟到回复")
	}

	expected := AgentReply{ExecutionID: "rex-b", DecisionKey: "decision-1",
		Payload: map[string]any{"action": "continue"}}
	if err := gateway.Resolve(expected); err != nil {
		t.Fatalf("停止 Robot A 不应取消 Robot B: %v", err)
	}
	select {
	case result := <-results["rex-b"]:
		if result.err != nil || result.reply.Payload["action"] != "continue" {
			t.Fatalf("Robot B 回复错误: %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("Robot B 没有收到回复")
	}
}

func TestRemoteAgentGatewayRegistersWaiterBeforeReporting(t *testing.T) {
	expected := AgentReply{ExecutionID: "rex-fast", DecisionKey: "decision-fast",
		Payload: map[string]any{"action": "continue"}}
	var gateway *RemoteAgentGateway
	gateway = NewRemoteAgentGateway(func(AgentRequest) error {
		return gateway.Resolve(expected)
	})
	reply, err := gateway.Request(context.Background(), AgentRequest{
		ExecutionID: "rex-fast", DecisionKey: "decision-fast"})
	if err != nil || reply.Payload["action"] != "continue" {
		t.Fatalf("同步快速回复不得早于等待句柄: reply=%#v err=%v", reply, err)
	}
}

func TestAgentReplyDecodesServerWireFormat(t *testing.T) {
	var reply AgentReply
	if err := json.Unmarshal([]byte(`{"execution_id":"rex-1","decision_key":"decision-1","payload":{"action":"abort"}}`), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.ExecutionID != "rex-1" || reply.DecisionKey != "decision-1" || reply.Payload["action"] != "abort" {
		t.Fatalf("Server snake_case AgentReply 解码错误: %#v", reply)
	}
}
