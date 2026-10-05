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

package workflow

import (
	"context"
	"encoding/json"
	"errors"

	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/store"
)

var (
	ErrPlannerUnavailable        = errors.New("Planning Runtime 尚未装配")
	ErrExecutorUnavailable       = errors.New("Developer Task Runtime 尚未装配")
	ErrTaskWaitingInput          = errors.New("Task 正在等待结构化输入")
	ErrTaskBusy                  = errors.New("Task 已被另一个运行占用")
	ErrTaskWaitingResource       = errors.New("Task 正在等待兼容资源")
	ErrRobotAgentDecisionInvalid = errors.New("robot_agent_decision_invalid")
)

const MaxRobotAgentDecisionAttempts = 3

type TaskPlanRequest struct {
	UserID       string
	Project      store.Project
	Conversation store.ChatSession
	Workflow     store.Workflow
	Task         store.TaskDraft
	Answer       *store.Interaction
}

type TaskAssignmentRequest struct {
	Project  store.Project
	Workflow store.Workflow
	Task     store.Task
}

type TaskAssignment struct {
	AgentID string
	RobotID string
}

// TaskPlanResult 保留 Task Agent 对本次拆解的可读摘要和机器可执行步骤。
// Workflow Service 仍只持久化 SubTask；摘要用于 Conversation 里程碑，不能
// 反向参与调度或替代结构化 SubTask。
type TaskPlanResult struct {
	Summary  string               `json:"summary"`
	SubTasks []store.SubTaskDraft `json:"subtasks"`
}

// TaskAssignmentResolver 由现有 Agent Runtime 实现。Workflow Service 只负责
// 何时分配，不复制 Agent/Robot 目录；没有兼容资源时返回等待，而不是让计划失败。
type TaskAssignmentResolver interface {
	ResolveTaskAssignment(context.Context, TaskAssignmentRequest) (TaskAssignment, error)
}

type Planner interface {
	PlanTask(context.Context, TaskPlanRequest) ([]store.SubTaskDraft, error)
}

// TaskPlanResultProvider 是 Planner 的增量接口。现有 Scheduler 可以继续调用
// PlanTask；需要把 Agent 规划摘要写入 Conversation 时再读取完整结果，避免为
// 展示字段一次性改写全部 Planner 测试桩。
type TaskPlanResultProvider interface {
	PlanTaskWithResult(context.Context, TaskPlanRequest) (TaskPlanResult, error)
}

// ConversationInteractionResumer 把 Conversation 中已经回答的结构化问题
// 交回原 Leader。Interaction 回答不会阻塞原模型请求，因此必须创建一个新
// Run；Workflow Service 只负责确定路由，不复制 Agent Runtime 的上下文装配。
type ConversationInteractionResumer interface {
	ResumeConversationInteraction(context.Context, store.Interaction) error
	ResumeWorkflowTerminal(context.Context, store.WorkflowView) error
}

type TaskExecution struct {
	UserID       string
	Project      store.Project
	Conversation store.ChatSession
	Workflow     store.Workflow
	Task         store.Task
	SubTasks     []store.SubTask
	Answer       *store.Interaction
	Run          store.RunSession
}

type TaskResult struct {
	Summary  string
	Evidence json.RawMessage
}

type TaskExecutor interface {
	ExecuteTask(context.Context, TaskExecution) (TaskResult, error)
}

type TaskRecoveryRequest struct {
	UserID       string
	Project      store.Project
	Conversation store.ChatSession
	Workflow     store.Workflow
	Task         store.Task
	SubTask      store.SubTask
	Execution    store.RobotExecution
	Answer       *store.Interaction
}

type TaskRecoveryDecision struct {
	Decision     string
	Replacements []store.SubTaskDraft
	Summary      string
}

// TaskRecoveryExecutor 只处理已经明确 failed 的 Robot Execution。它可以
// 重写尚未开始的剩余步骤或收敛 Task，但没有权限修改 Store，更不能重放
// interrupted/unknown 的物理动作；最终变更仍由 Workflow Service 事务提交。
type TaskRecoveryExecutor interface {
	RecoverTask(context.Context, TaskRecoveryRequest) (TaskRecoveryDecision, error)
}

type RobotAgentDecisionRequest struct {
	UserID       string
	Project      store.Project
	Conversation store.ChatSession
	Workflow     store.Workflow
	Task         store.Task
	SubTask      store.SubTask
	Execution    store.RobotExecution
	Event        map[string]any
	Answer       *store.Interaction
}

// RobotAgentDecisionExecutor 只在 Robot Skill 到达安全 checkpoint 并明确
// 请求 agent.requested 时运行一次 Task Agent。它不拥有 Workflow 状态，也
// 不能重新下发物理 Action；返回值只是发回原 Worker 的类型化决策。
type RobotAgentDecisionExecutor interface {
	ResolveRobotAgentRequest(context.Context, RobotAgentDecisionRequest) (map[string]any, error)
}

// RobotAgentReplySink 是 Workflow 到现有 Robot/Pilot 通道的最小端口。
// Workflow 不关心 Pilot 传输细节，只把匹配 execution/stage/revision 的回复
// 送回仍在等待的 Worker。
type RobotAgentReplySink interface {
	ReplyAgentRequest(executionID string, payload map[string]any) error
}

// RobotExecutionStopper 复用 Robot Service 已有的安全停止协议。Workflow 只
// 决定何时停止，不直接理解 Pilot、Ability 或 SDK；真正的 hold 证据仍由
// Robot Execution 终态事件返回，避免在任务层伪造物理设备已经停止。
type RobotExecutionStopper interface {
	Stop(ctx context.Context, projectID, executionID, reason string) (store.RobotExecution, error)
	ConfirmOperatorStop(ctx context.Context, expected store.RobotExecution,
		userID, reason string) (store.RobotExecution, error)
}

type Deps struct {
	Store               *store.Store
	Bus                 *event.Bus
	Planner             Planner
	Executor            TaskExecutor
	RobotReply          RobotAgentReplySink
	RobotStop           RobotExecutionStopper
	ConversationResumer ConversationInteractionResumer
}
