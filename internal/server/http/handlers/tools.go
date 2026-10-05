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

package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"insightos.cn/semantic-framework/internal/mcpregistry"
	"insightos.cn/semantic-framework/internal/tool"
)

// 工具目录视图的来源分组（sourceGroup.Kind）取值。
const (
	// toolSourceBuiltin 内置 Go 工具（internal/tool 注册表）。
	toolSourceBuiltin = "builtin"

	// toolSourceMCP MCP server 发现的工具（mcpregistry 目录）。
	toolSourceMCP = mcpregistry.SourceKindMCP
)

// ToolsHandler 是工具目录的 REST 处理器（架构 16 §4 目录只读视图）：
// 按来源分组返回内置工具与 MCP 工具（含命名空间/风险/健康状态），
// 为前端工具目录页（R17）与调试观测供数据。
type ToolsHandler struct {
	// registry 内置工具注册表。
	registry *tool.Registry

	// mcpReg MCP 工具目录；nil 时响应只含 builtin 组。
	mcpReg *mcpregistry.Registry
}

// NewToolsHandler 创建工具目录处理器。
func NewToolsHandler(registry *tool.Registry, mcpReg *mcpregistry.Registry) *ToolsHandler {
	return &ToolsHandler{registry: registry, mcpReg: mcpReg}
}

// toolView 是单个工具的目录视图（内置与 MCP 同一形状，
// 前端不需要按来源特判字段）。
type toolView struct {
	// Name 工具全名（<命名空间>.<动作>）。
	Name string `json:"name"`

	// Namespace 命名空间（MCP 工具即 server 名）。
	Namespace string `json:"namespace"`

	// Description 能力描述。
	Description string `json:"description"`

	// Risk 风险等级（low/medium/high/critical）。
	Risk string `json:"risk"`

	// Schema 参数 jsonschema（object 根）；原始 JSON 透传不二次序列化。
	Schema json.RawMessage `json:"schema,omitempty"`

	// Health 健康状态（healthy/unavailable）；
	// 内置工具无健康概念，恒为 healthy（形状统一）。
	Health string `json:"health"`

	// Server 来源 server 名（仅 MCP 工具有）。
	Server string `json:"server,omitempty"`

	// UpdatedAt 条目内容最后变化时间（仅 MCP 工具有）。
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// sourceGroup 是按来源分组的工具清单。
type sourceGroup struct {
	// Kind 来源（builtin | mcp）。
	Kind string `json:"kind"`

	// Tools 该来源的工具（按全名排序）。
	Tools []toolView `json:"tools"`
}

// toolsResponse 是 GET /tools 的响应体。
type toolsResponse struct {
	// Sources 按来源分组的工具目录（builtin 在前，mcp 在后）。
	Sources []sourceGroup `json:"sources"`
}

// HandleListTools 处理 GET /api/v1/tools：返回按来源分组的工具目录。
// MCP 组直接读目录快照（含 unavailable 条目——目录页需要展示失联工具，
// 与 Agent 注入只取 healthy 的视图不同）。
func (h *ToolsHandler) HandleListTools(w http.ResponseWriter, _ *http.Request) {
	sources := make([]sourceGroup, 0, 2)

	builtin := make([]toolView, 0)
	for _, def := range h.registry.List() {
		builtin = append(builtin, toolView{
			Name:        def.Name,
			Namespace:   def.Namespace,
			Description: def.Description,
			Risk:        def.Annotations.Risk,
			Schema:      json.RawMessage(def.ParametersJSON),
			Health:      mcpregistry.HealthHealthy,
		})
	}
	sources = append(sources, sourceGroup{Kind: toolSourceBuiltin, Tools: builtin})

	if h.mcpReg != nil {
		mcpTools := make([]toolView, 0)
		for _, entry := range h.mcpReg.List() {
			updatedAt := entry.UpdatedAt
			mcpTools = append(mcpTools, toolView{
				Name:        entry.FullName,
				Namespace:   entry.Server,
				Description: entry.Description,
				Risk:        entry.Risk,
				Schema:      json.RawMessage(entry.SchemaJSON),
				Health:      entry.Health,
				Server:      entry.Server,
				UpdatedAt:   &updatedAt,
			})
		}
		sources = append(sources, sourceGroup{Kind: toolSourceMCP, Tools: mcpTools})
	}

	writeJSON(w, http.StatusOK, toolsResponse{Sources: sources})
}
