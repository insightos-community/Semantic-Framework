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
	"fmt"
	"sort"
	"strings"

	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/agent/subagent"
	"insightos.cn/semantic-framework/internal/mcpregistry"
	"insightos.cn/semantic-framework/internal/tool"
)

const (
	// ToolDeliveryDirect 表示工具 schema 在首轮直接交给模型。
	ToolDeliveryDirect = "direct"
	// ToolDeliverySearch 表示工具由 Eino ToolSearch 按需发现后再交给模型。
	ToolDeliverySearch = "tool_search"
	// ToolDeliveryMiddleware 表示工具由 Eino Middleware 在运行器构建时注入。
	ToolDeliveryMiddleware = "middleware"
	// ToolDeliveryAgent 表示工具由 Eino AgentTool 在会话运行时生成。
	ToolDeliveryAgent = "agent_tool"
)

// SessionAgentToolView 是某个会话中某个 Agent 的有效工具目录项。Name 是
// Semantic 领域名称，ModelName 是经过 Eino 安全化后真正出现在模型函数表
// 中的名称；两者同时返回便于前端和 Trace 准确解释工具调用。
type SessionAgentToolView struct {
	Name        string          `json:"name"`
	ModelName   string          `json:"model_name"`
	Namespace   string          `json:"namespace"`
	Description string          `json:"description"`
	Risk        string          `json:"risk"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Source      string          `json:"source"`
	Delivery    string          `json:"delivery"`
}

// SessionAgentToolsView 汇总有效工具及 ToolSearch 状态。它描述实际运行装配，
// 与 GET /tools 的“全局已安装目录”语义不同。
type SessionAgentToolsView struct {
	SessionID        string                 `json:"session_id"`
	AgentID          string                 `json:"agent_id"`
	Role             string                 `json:"role"`
	ToolSearchActive bool                   `json:"tool_search_active"`
	Tools            []SessionAgentToolView `json:"tools"`
}

// ListSessionAgentTools 返回会话与 Agent 策略共同决定的有效工具。这里只解析
// 装配元数据，不构建模型客户端、不执行工具，也不会触发 SubAgent Run。
func (s *Service) ListSessionAgentTools(ctx context.Context, userID, sessionID,
	agentID string) (SessionAgentToolsView, error) {
	if err := s.requireOwnedSession(userID, sessionID); err != nil {
		return SessionAgentToolsView{}, err
	}
	prof, err := s.profileForAgent(agentID)
	if err != nil {
		return SessionAgentToolsView{}, err
	}
	defs, mcpEntries, err := s.collectSessionToolDefinitions(sessionID, prof)
	if err != nil {
		return SessionAgentToolsView{}, err
	}
	var agentToolDefs []tool.Definition
	// Coordinator 在对话中、Worker 在 Task 执行中都可以使用只读咨询工具。
	// Service Agent 自身不进入该分支，因此不会形成递归 SubAgent 调用。
	if (prof.Mode == profile.ModeCoordinator || prof.Mode == profile.ModeWorker) && s.subAgents != nil {
		for _, sub := range s.subAgents.List() {
			agentToolDefs = append(agentToolDefs, subagent.ToolDefinition(sub))
		}
	}

	pinned, dynamic := splitToolDefs(defs, prof.Tools.Pinned, s.logger)
	budgetDefs := append([]tool.Definition(nil), defs...)
	budgetDefs = append(budgetDefs, agentToolDefs...)
	searchActive := shouldUseToolSearch(prof.Tools.ToolSearch, budgetDefs, dynamic,
		prof.Limits.ContextTokens)
	delivery := make(map[string]string, len(defs))
	for _, def := range pinned {
		delivery[def.Name] = ToolDeliveryDirect
	}
	for _, def := range dynamic {
		if searchActive {
			delivery[def.Name] = ToolDeliverySearch
		} else {
			delivery[def.Name] = ToolDeliveryDirect
		}
	}
	mcpNames := make(map[string]struct{}, len(mcpEntries))
	for _, entry := range mcpEntries {
		mcpNames[entry.Definition().Name] = struct{}{}
	}
	views := make([]SessionAgentToolView, 0, len(defs)+8)
	for _, def := range defs {
		source := "builtin"
		if _, ok := mcpNames[def.Name]; ok {
			source = mcpregistry.SourceKindMCP
		}
		views = append(views, definitionToolView(def, source, delivery[def.Name]))
	}

	// Deps.Tools 是测试与嵌入场景的额外 Eino 工具入口。生产通常为空，
	// 仍应如实列入有效目录，避免 API 在测试运行器中丢失真实工具。
	for _, extra := range s.tools {
		info, infoErr := extra.Info(ctx)
		if infoErr != nil {
			return SessionAgentToolsView{}, fmt.Errorf("读取额外工具信息失败: %w", infoErr)
		}
		schemaJSON, schemaErr := marshalEinoToolSchema(info.ParamsOneOf)
		if schemaErr != nil {
			return SessionAgentToolsView{}, fmt.Errorf("转换额外工具 %q 的参数 Schema 失败: %w",
				info.Name, schemaErr)
		}
		views = append(views, SessionAgentToolView{Name: info.Name, ModelName: info.Name,
			Namespace: "extra", Description: info.Desc, Risk: tool.RiskLow,
			Schema: schemaJSON, Source: "extra", Delivery: ToolDeliveryDirect})
	}

	// Leader 的委派工具由 AgentTool 运行时生成，不存在于全局 Registry；这里只
	// 从同一个 SubAgent Registry 生成契约，避免为了展示目录而构建子模型。
	for _, def := range agentToolDefs {
		views = append(views, definitionToolView(def, "agent", ToolDeliveryAgent))
	}

	effectiveSkills, err := s.sessionSkillReader(sessionID, prof)
	if err != nil {
		return SessionAgentToolsView{}, err
	}
	if effectiveSkills != nil && len(effectiveSkills.List()) > 0 {
		views = append(views, middlewareToolView(kernel.SkillToolCatalog(
			effectiveSkills.Summary()), "skill"))
	}
	workspaceRoot, err := s.sessionWorkspaceRoot(sessionID)
	if err != nil {
		return SessionAgentToolsView{}, err
	}
	if workspaceRoot != "" {
		for _, info := range kernel.FilesystemToolCatalog() {
			views = append(views, middlewareToolView(info, "filesystem"))
		}
	}
	if searchActive {
		views = append(views, middlewareToolView(kernel.ToolSearchCatalog(), "tool_search"))
	}

	// 名称排序确保刷新、测试与前端 diff 稳定；不同来源重名会在装配阶段被
	// 冲突校验拒绝，Middleware 固定名也由对应构建器保证不与注册表重名。
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	return SessionAgentToolsView{SessionID: sessionID, AgentID: agentID,
		Role: prof.Name, ToolSearchActive: searchActive, Tools: views}, nil
}

// profileForAgent 解析实例 ID 对应的角色 Profile，规则与会话模型控制面一致。
func (s *Service) profileForAgent(agentID string) (*profile.Profile, error) {
	role := agentID
	if strings.HasPrefix(agentID, "robot:") && strings.TrimSpace(strings.TrimPrefix(agentID, "robot:")) != "" {
		role = "robot"
	} else if info, ok := s.roster.get(agentID); ok {
		role = info.Role
	} else if agentID == agentRoleLeader {
		role = agentRoleLeader
	} else if s.subAgents != nil {
		if def, ok := s.subAgents.Get(agentID); ok {
			role = def.Role
		} else {
			return nil, fmt.Errorf("Agent %q 不存在", agentID)
		}
	} else {
		return nil, fmt.Errorf("Agent %q 不存在", agentID)
	}
	return s.profiles.Load(role)
}

// definitionToolView 把统一工具契约转换为会话目录项。
func definitionToolView(def tool.Definition, source, delivery string) SessionAgentToolView {
	return SessionAgentToolView{Name: def.Name, ModelName: kernel.SafeToolName(def.Name),
		Namespace: def.Namespace, Description: def.Description, Risk: def.Annotations.Risk,
		Schema: json.RawMessage(def.ParametersJSON), Source: source, Delivery: delivery}
}

// marshalEinoToolSchema 使用 Eino 提供的公开转换方法生成 JSON Schema。
// ParamsOneOf 的实际字段不对外导出，直接 json.Marshal 会静默得到空对象，
// 导致 Studio 展示的参数与模型实际收到的参数不一致。
func marshalEinoToolSchema(params *schema.ParamsOneOf) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	js, err := params.ToJSONSchema()
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(js)
	return json.RawMessage(encoded), err
}

// middlewareToolView 转换 Eino Middleware 工具目录项；这些工具都由只读或
// 检索边界约束，本阶段统一标记为低风险。
func middlewareToolView(info kernel.MiddlewareToolInfo, source string) SessionAgentToolView {
	return SessionAgentToolView{Name: info.Name, ModelName: info.Name,
		Namespace: source, Description: info.Description, Risk: tool.RiskLow,
		Schema: json.RawMessage(info.SchemaJSON), Source: source, Delivery: ToolDeliveryMiddleware}
}
