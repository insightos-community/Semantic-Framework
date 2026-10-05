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
	"errors"
	"strings"
	"sync"
)

type agentRequestResult struct {
	reply AgentReply
	err   error
}

type agentWaiter struct {
	result chan agentRequestResult
}

// RemoteAgentGateway 把 Worker 的类型化 AgentRequest 暂停在 Pilot，等待
// Semantic Server 返回与 Execution/Stage/revision 完全一致的回复。
type RemoteAgentGateway struct {
	Report  func(AgentRequest) error
	mu      sync.Mutex
	waiters map[string]*agentWaiter
}

func NewRemoteAgentGateway(report func(AgentRequest) error) *RemoteAgentGateway {
	return &RemoteAgentGateway{Report: report, waiters: make(map[string]*agentWaiter)}
}

func agentRequestKey(executionID, decisionKey string) string {
	return executionID + "\x00" + decisionKey
}

func (g *RemoteAgentGateway) Request(ctx context.Context, request AgentRequest) (AgentReply, error) {
	key := agentRequestKey(request.ExecutionID, request.DecisionKey)
	waiter := &agentWaiter{result: make(chan agentRequestResult, 1)}
	g.mu.Lock()
	if err := ctx.Err(); err != nil {
		g.mu.Unlock()
		return AgentReply{}, err
	}
	if _, exists := g.waiters[key]; exists {
		g.mu.Unlock()
		return AgentReply{}, errors.New("AgentRequest 已经在等待回复")
	}
	g.waiters[key] = waiter
	g.mu.Unlock()
	defer func() { g.mu.Lock(); delete(g.waiters, key); g.mu.Unlock() }()
	if g.Report == nil {
		return AgentReply{}, errors.New("AgentRequest 上报未装配")
	}
	if err := g.Report(request); err != nil {
		return AgentReply{}, err
	}
	select {
	case result := <-waiter.result:
		if result.err != nil {
			return AgentReply{}, result.err
		}
		return result.reply, nil
	case <-ctx.Done():
		return AgentReply{}, ctx.Err()
	}
}

func (g *RemoteAgentGateway) Resolve(reply AgentReply) error {
	key := agentRequestKey(reply.ExecutionID, reply.DecisionKey)
	g.mu.Lock()
	waiter := g.waiters[key]
	if waiter == nil {
		g.mu.Unlock()
		return errors.New("没有匹配的 AgentRequest")
	}
	delete(g.waiters, key)
	g.mu.Unlock()
	waiter.result <- agentRequestResult{reply: reply}
	return nil
}

// CancelExecution 只解除指定 Robot Execution 正在等待的模型决策，不影响
// 其他 Robot。安全停止已经锁存后，Worker 必须先离开 agent.request，才能处理
// skill.stop 并通过 Ability/SDK 形成 hold 证据；这里不伪造 AgentReply，也不把
// “等待用户/模型”误当作物理执行完成。
func (g *RemoteAgentGateway) CancelExecution(executionID string) {
	prefix := executionID + "\x00"
	g.mu.Lock()
	canceled := make([]*agentWaiter, 0, 1)
	for key, waiter := range g.waiters {
		if strings.HasPrefix(key, prefix) {
			delete(g.waiters, key)
			canceled = append(canceled, waiter)
		}
	}
	g.mu.Unlock()
	for _, waiter := range canceled {
		waiter.result <- agentRequestResult{err: errors.New("AgentRequest 因 Robot Execution 停止而取消")}
	}
}
