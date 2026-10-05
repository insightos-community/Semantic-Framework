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

package subagent

import (
	"fmt"
	"sort"
	"time"

	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/agent/team"
	"insightos.cn/semantic-framework/internal/tool"
)

// DelegationTimeout 是单次委派的执行硬上限（工具 annotation.Timeout 语义）：
// SubAgent 是 max_turns 受限的短调用，2 分钟覆盖最坏轮次；超时由 kernel
// 委派工具适配层强制（context.WithTimeout），结果归一为结构化 TIMEOUT。
const DelegationTimeout = 2 * time.Minute

// toolParamsJSON 是委派工具的参数 schema 文本：task 必填，artifact_refs
// 是可选图片 ArtifactRef 数组。安全管线 L2 会在 Eino AgentTool 执行前按
// 本契约校验，因此必须与 kernel.agentToolInputSchema 保持一致；遗漏字段会
// 造成模型已经正确生成图片委派参数，却被门禁提前判为 PARAM_VIOLATION。
const toolParamsJSON = `{"type":"object","properties":{"task":{"type":"string",` +
	`"description":"委派给该助手的任务描述（一次性说清背景与期望产物）"},` +
	`"artifact_refs":{"type":"array","items":{"type":"string"},` +
	`"description":"需要共享给该助手的图片 ArtifactRef"}},` +
	`"required":["task"],"additionalProperties":false}`

// Def 是一个可委派成员的定义：从 Team 成员声明与角色 profile
// （subagent 段）派生，是 runtime 装配委派工具（kernel.BuildAgentTool）
// 与安全门禁契约（ToolDefinition）的输入。
type Def struct {
	// ID 成员实例标识（Team 定义中的 id，如 query-1）：SubAgent 的 Agent 名，
	// 也是冒泡事件与 roster 的归因基准。
	ID string

	// Role 角色名（profile 目录名，如 query）。
	Role string

	// Description 角色职责描述。
	Description string

	// ToolName 委派工具名（leader 模型侧函数名，如 ask_query）。
	ToolName string

	// ToolDescription 委派工具描述（leader 模型判断委派时机的依据）。
	ToolDescription string

	// Namespaces SubAgent 可用的工具命名空间（profile.tools.namespaces
	// 透传，装配其独立工具链用）。
	Namespaces []string

	// MaxTurns 委派执行的 ReAct 轮次上限（subagent.max_turns 覆盖，
	// 缺省沿用 limits.max_turns）。
	MaxTurns int
}

// Registry 是 Team 启用 SubAgent 的成员目录：组建期一次性填充，之后只读
// （profile 运行期不变，与 Loader 缓存语义一致）。
type Registry struct {
	// defs 成员 ID → 定义。
	defs map[string]Def
}

// NewRegistry 从 Team 定义派生 SubAgent 目录：遍历全部成员，
// profile.subagent.enabled 的成员登记入册（仅 service 模式可启用，
// profile 加载期已校验，此处不重复）。无启用成员时返回空目录
// （非 nil——调用方统一 List 语义，无需判空）。
func NewRegistry(def *team.Def, loader *profile.Loader) (*Registry, error) {
	reg := &Registry{defs: make(map[string]Def)}
	for _, member := range def.All() {
		prof, err := loader.Load(member.Role)
		if err != nil {
			return nil, fmt.Errorf("加载成员 %q 的角色 %q 失败: %w", member.ID, member.Role, err)
		}
		if !prof.SubAgent.Enabled {
			continue
		}
		maxTurns := prof.SubAgent.MaxTurns
		if maxTurns <= 0 {
			maxTurns = prof.Limits.MaxTurns
		}
		reg.defs[member.ID] = Def{
			ID:              member.ID,
			Role:            prof.Name,
			Description:     prof.Description,
			ToolName:        prof.SubAgent.ToolName,
			ToolDescription: prof.SubAgent.ToolDescription,
			Namespaces:      prof.Tools.Namespaces,
			MaxTurns:        maxTurns,
		}
	}
	return reg, nil
}

// List 返回全部 SubAgent 定义（按 ID 升序，装配顺序确定性）。
func (r *Registry) List() []Def {
	list := make([]Def, 0, len(r.defs))
	for _, def := range r.defs {
		list = append(list, def)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list
}

// Get 按成员 ID 查询定义；第二个返回值表示该成员是否启用的 SubAgent。
func (r *Registry) Get(id string) (Def, bool) {
	def, ok := r.defs[id]
	return def, ok
}

// ToolDefinition 返回委派工具在安全管线的契约登记：命名空间按 tool_name
// 归属（approval_required 模式按它匹配），risk=low（委派本身是调用语义——
// SubAgent 内部工具调用的安全判定由其自身门禁管线负责），Timeout 为委派
// 执行硬上限。为什么必须登记：安全门禁对未登记工具 fail-closed（UNKNOWN_TOOL
// 拒绝），委派工具不在册会被整体拒执行。
func ToolDefinition(def Def) tool.Definition {
	return tool.Definition{
		Name:           def.ToolName,
		Namespace:      def.ToolName,
		Description:    def.ToolDescription,
		ParametersJSON: toolParamsJSON,
		Annotations:    tool.Annotations{Risk: tool.RiskLow, Timeout: DelegationTimeout},
	}
}
