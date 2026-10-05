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

package tool

import (
	"context"
	"strings"
)

const (
	// ExecuteToolName 是 Docker 沙箱执行工具的稳定名称。
	ExecuteToolName = "execute"

	// ExecuteHostToolName 是受控宿主执行工具的稳定名称。
	ExecuteHostToolName = "execute_host"
)

// ExecutionScope 是一次 Agent Run 可访问的执行边界。工具从 context 读取
// 该值，而不是把会话或工作区写入全局可变状态，避免并发会话串用 Project。
type ExecutionScope struct {
	// RunKind 区分普通对话、规划和 Task 执行。工具执行器会对规划类 Run
	// 再做一次只读 allowlist 校验，避免只靠模型侧工具列表。
	RunKind string

	// InteractionMode 是普通 Conversation 本轮的交互模式。plan 不改变
	// RunKind，但会在执行器层启用与 Planning Run 等价的只读门禁。
	InteractionMode string

	// RunID 与 AgentID 供只创建领域请求、不直接执行业务动作的内部工具
	// 精确关联来源 Run/Agent。
	RunID   string
	AgentID string

	// Workflow/Task/SubTask/Robot 由调度器从当前运行事实补齐，不允许模型在
	// robot.run 参数中改选设备或伪造任务归属。
	WorkflowID string
	TaskID     string
	SubtaskID  string
	RobotID    string
	// SessionID 是本次工具调用所属的对话会话。
	SessionID string

	// ProjectID 是本次工具调用所属的 Project。
	ProjectID string

	// OwnerID 是当前会话用户，用于新建 Artifact 时写入归属。
	OwnerID string

	// WorkspaceRoot 是 Store 创建并校验过的 Project 工作区绝对路径。
	WorkspaceRoot string

	// SkillsRoot 是当前已安装 Skill 根目录；空表示未装配 Skill Store。
	SkillsRoot string

	// ExecutionMode 是本会话的 ask/auto/full 执行模式。
	ExecutionMode string

	// HostExecutionEnabled 表示用户已在当前会话显式开启宿主执行。
	HostExecutionEnabled bool

	// HostExecutionAllowed 表示 Server 全局允许宿主执行。
	HostExecutionAllowed bool
}

// PlanningToolAllowed 是 Planning Run 在执行层允许的公共工具清单。
// Filesystem 的四个只读工具由 kernel policy 校验，不经过本执行器。
func PlanningToolAllowed(name string) bool {
	return strings.HasPrefix(name, "system.") || name == "artifact.get" ||
		name == "artifact.list" || name == "map.query" || name == "interaction.ask"
}

// ConversationPlanToolAllowed 比独立 Planning Run 额外允许 Proposal 的提交。
// Proposal 批准由用户界面携带精确 revision 直接提交，不能交给模型猜测。
func ConversationPlanToolAllowed(name string) bool {
	return PlanningToolAllowed(name) || name == "plan.suggest" || name == "robot.get"
}

// executionScopeKey 使用包内私有类型作为 context key，防止其他包误覆盖。
type executionScopeKey struct{}

// WithExecutionScope 把已校验的 Project 执行边界附加到单次 Run context。
func WithExecutionScope(ctx context.Context, scope ExecutionScope) context.Context {
	return context.WithValue(ctx, executionScopeKey{}, scope)
}

// ExecutionScopeFromContext 读取当前工具调用的执行边界。返回 false 表示工具
// 不在会话 Run 内执行，此时执行类工具必须拒绝运行。
func ExecutionScopeFromContext(ctx context.Context) (ExecutionScope, bool) {
	scope, ok := ctx.Value(executionScopeKey{}).(ExecutionScope)
	if !ok || scope.SessionID == "" || scope.ProjectID == "" || scope.WorkspaceRoot == "" {
		return ExecutionScope{}, false
	}
	return scope, true
}
