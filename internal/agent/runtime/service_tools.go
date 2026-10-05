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
	"fmt"
	"sort"
	"strings"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/mcpregistry"
	"insightos.cn/semantic-framework/internal/security"
	"insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/log"
)

const (
	// toolSearchContextRatioDenominator 表示工具 schema 最多占上下文预算 10%。
	toolSearchContextRatioDenominator = 10

	// toolSearchFallbackCount 是无法取得上下文预算时的后备阈值；动态工具
	// 超过 10 个才启用检索，小工具集直接交给 Eino ToolsNode。
	toolSearchFallbackCount = 10
)

// buildToolchain 装配 ToolsNode 工具集、按需 ToolSearch 与安全门禁。
// Profile 的 tool_search 只表示“允许按需检索”；小工具集仍直接加载，避免
// 为一次元工具检索多付模型轮次并改变常用工具的直接可见性。
func (s *Service) buildToolchain(prof *profile.Profile, agentID string,
	extraSafetyDefs ...tool.Definition) ([]kernel.Tool, []kernel.Tool, kernel.ToolCallGuard, error) {
	return s.buildSessionToolchain("", prof, agentID, extraSafetyDefs...)
}

// buildSessionToolchain 在通用工具链基础上应用会话级可见性。execute_host
// 只有 Server 允许且当前会话显式开启时才进入模型工具集；Observer 等没有
// 对话会话的运行器始终看不到宿主执行。
func (s *Service) buildSessionToolchain(sessionID string, prof *profile.Profile, agentID string,
	extraSafetyDefs ...tool.Definition) ([]kernel.Tool, []kernel.Tool, kernel.ToolCallGuard, error) {
	tools := s.tools
	if s.registry == nil && s.mcpReg == nil {
		return tools, nil, nil, nil
	}

	defs, mcpEntries, err := s.collectSessionToolDefinitions(sessionID, prof)
	if err != nil {
		return nil, nil, nil, err
	}

	pinnedDefs, dynamicDefs := splitToolDefs(defs, prof.Tools.Pinned, s.logger)
	injectDefs := pinnedDefs
	var searchDefs []tool.Definition
	budgetDefs := append([]tool.Definition(nil), defs...)
	budgetDefs = append(budgetDefs, extraSafetyDefs...)
	if shouldUseToolSearch(prof.Tools.ToolSearch, budgetDefs, dynamicDefs,
		prof.Limits.ContextTokens) {
		searchDefs = dynamicDefs
		s.logger.Info("ToolSearch 动态检索已启用",
			"role", prof.Name, "pinned", len(pinnedDefs), "dynamic", len(searchDefs),
			"schema_tokens_estimate", estimateToolSchemaTokens(budgetDefs),
			"context_tokens", prof.Limits.ContextTokens)
	} else {
		injectDefs = append(injectDefs, dynamicDefs...)
	}

	adapted, err := s.adaptToolDefs(injectDefs, mcpEntries)
	if err != nil {
		return nil, nil, nil, err
	}
	tools = append(tools, adapted...)

	var dynamic []kernel.Tool
	if len(searchDefs) > 0 {
		dynamic, err = s.adaptToolDefs(searchDefs, mcpEntries)
		if err != nil {
			return nil, nil, nil, err
		}
	}

	safetyDefs := defs
	if len(extraSafetyDefs) > 0 {
		safetyDefs = make([]tool.Definition, 0, len(defs)+len(extraSafetyDefs))
		safetyDefs = append(safetyDefs, defs...)
		safetyDefs = append(safetyDefs, extraSafetyDefs...)
	}
	if len(safetyDefs) == 0 {
		return tools, dynamic, nil, nil
	}
	safety, err := security.NewMiddleware(
		safetyDefs, prof.Interrupt.ApprovalRequired, agentID, s.logger)
	if err != nil {
		return nil, nil, nil, err
	}
	return tools, dynamic, safety, nil
}

// collectSessionToolDefinitions 是运行器装配与工具可见性 API 的共同选择入口。
// 两个消费面必须使用同一套 Profile 命名空间、MCP 健康目录和会话宿主权限
// 规则，避免前端显示的“有效工具”与真正交给 Eino ToolsNode 的工具发生漂移。
func (s *Service) collectSessionToolDefinitions(sessionID string,
	prof *profile.Profile) ([]tool.Definition, []mcpregistry.Entry, error) {
	var defs []tool.Definition
	if s.registry != nil {
		defs = s.registry.MatchNamespaces(prof.Tools.Namespaces)
		var err error
		defs, err = s.filterSessionExecutionTools(sessionID, defs)
		if err != nil {
			return nil, nil, err
		}
	}
	var mcpEntries []mcpregistry.Entry
	if s.mcpReg == nil {
		return defs, nil, nil
	}
	mcpEntries = s.mcpReg.MatchNamespaces(prof.Tools.Namespaces)
	seen := make(map[string]string, len(defs)+len(mcpEntries))
	for _, def := range defs {
		seen[def.Name] = "内置工具"
	}
	for _, entry := range mcpEntries {
		def := entry.Definition()
		if src, dup := seen[def.Name]; dup {
			return nil, nil, fmt.Errorf(
				"工具名冲突: MCP server %q 的工具 %q 与%s重名", entry.Server, def.Name, src)
		}
		seen[def.Name] = fmt.Sprintf("MCP server %q", entry.Server)
		defs = append(defs, def)
	}
	return defs, mcpEntries, nil
}

// filterSessionExecutionTools 移除当前会话无权使用的宿主工具。Docker execute
// 不受宿主开关影响，继续按自身审批模式工作。
func (s *Service) filterSessionExecutionTools(sessionID string,
	defs []tool.Definition) ([]tool.Definition, error) {
	hostVisible := false
	if sessionID != "" && s.hostExecutionAllowed() {
		policy, err := s.st.GetSessionExecutionPolicy(sessionID)
		if err != nil {
			return nil, err
		}
		hostVisible = policy.HostExecutionEnabled
	}
	filtered := make([]tool.Definition, 0, len(defs))
	for _, def := range defs {
		if def.Name == tool.ExecuteHostToolName && !hostVisible {
			continue
		}
		filtered = append(filtered, def)
	}
	return filtered, nil
}

// shouldUseToolSearch 根据 schema 预算决定是否启用 Eino 通用检索。取得
// ContextTokens 时使用 10% 预算；无法取得时仅按动态工具数 >10 后备判断。
func shouldUseToolSearch(enabled bool, allDefs, dynamicDefs []tool.Definition,
	contextTokens int) bool {
	if !enabled || len(dynamicDefs) == 0 {
		return false
	}
	if contextTokens <= 0 {
		return len(dynamicDefs) > toolSearchFallbackCount
	}
	return estimateToolSchemaTokens(allDefs) > contextTokens/toolSearchContextRatioDenominator
}

// estimateToolSchemaTokens 对模型实际可见的名称、描述和参数 schema 做保守
// 估算。中英文混合按 2 个 Unicode 字符约 1 token 计算；这里只用于是否启用
// 检索的稳定阈值，不参与计费和上下文精确裁剪。
func estimateToolSchemaTokens(defs []tool.Definition) int {
	totalRunes := 0
	for _, def := range defs {
		totalRunes += len([]rune(strings.Join([]string{
			def.Name, def.Description, def.ParametersJSON,
		}, "\n")))
	}
	return (totalRunes + 1) / 2
}

// adaptToolDefs 按内置/MCP 来源路由到对应执行后端。
func (s *Service) adaptToolDefs(defs []tool.Definition,
	mcpEntries []mcpregistry.Entry) ([]kernel.Tool, error) {
	if len(defs) == 0 {
		return nil, nil
	}
	var mcpByName map[string]mcpregistry.Entry
	if len(mcpEntries) > 0 {
		mcpByName = make(map[string]mcpregistry.Entry, len(mcpEntries))
		for _, entry := range mcpEntries {
			mcpByName[entry.Definition().Name] = entry
		}
	}
	var builtinDefs []tool.Definition
	var mcpDefs []mcpregistry.Entry
	for _, def := range defs {
		if entry, ok := mcpByName[def.Name]; ok {
			mcpDefs = append(mcpDefs, entry)
		} else {
			builtinDefs = append(builtinDefs, def)
		}
	}
	adapted, err := kernel.AdaptTools(builtinDefs, s.executor)
	if err != nil {
		return nil, err
	}
	mcpTools, err := kernel.AdaptMCPTools(mcpDefs, s.mcpReg)
	if err != nil {
		return nil, err
	}
	return append(adapted, mcpTools...), nil
}

// splitToolDefs 把命中清单稳定切分为 pinned 与可检索动态集。
func splitToolDefs(defs []tool.Definition, pinned []string,
	logger *log.Logger) (pinnedDefs, dynamic []tool.Definition) {
	if len(pinned) == 0 {
		return nil, defs
	}
	want := make(map[string]struct{}, len(pinned))
	for _, name := range pinned {
		want[name] = struct{}{}
	}
	for _, def := range defs {
		if _, ok := want[def.Name]; ok {
			pinnedDefs = append(pinnedDefs, def)
			delete(want, def.Name)
		} else {
			dynamic = append(dynamic, def)
		}
	}
	if len(want) > 0 {
		missed := make([]string, 0, len(want))
		for name := range want {
			missed = append(missed, name)
		}
		sort.Strings(missed)
		logger.Warn("pinned 工具不在命名空间命中集，已忽略", "tools", missed)
	}
	return pinnedDefs, dynamic
}
