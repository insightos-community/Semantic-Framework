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

package kernel

import einofs "github.com/cloudwego/eino/adk/middlewares/filesystem"

const (
	// 文件工具描述既用于 Eino Middleware 装配，也用于会话有效工具 API。
	// 统一维护可避免模型实际看到的说明与前端检查器发生漂移。
	filesystemLsDescription   = "列出当前 Project 目录；path 必须是 /workspace 下的绝对虚拟路径。"
	filesystemReadDescription = "分页读取当前 Project 的文本文件；file_path 必须是 /workspace 下的绝对虚拟路径。"
	filesystemGlobDescription = "在当前 Project 内按 glob 模式查找文件；path 必须位于 /workspace。"
	filesystemGrepDescription = "在当前 Project 内使用正则搜索文本；path 必须位于 /workspace。"
)

// MiddlewareToolInfo 描述由 Eino Middleware 注入、不会出现在 Semantic
// 全局 Registry 中的工具。它只承载只读目录信息，不参与实际工具执行。
type MiddlewareToolInfo struct {
	Name        string
	Description string
	SchemaJSON  string
}

// FilesystemToolCatalog 返回当前 Project 文件中间件固定注入的四个只读工具。
// 名称直接引用 Eino 常量，依赖升级后若官方改名可在编译期发现。
func FilesystemToolCatalog() []MiddlewareToolInfo {
	return []MiddlewareToolInfo{
		{Name: einofs.ToolNameLs, Description: filesystemLsDescription,
			SchemaJSON: `{"type":"object","properties":{"path":{"type":"string"}}}`},
		{Name: einofs.ToolNameReadFile, Description: filesystemReadDescription,
			SchemaJSON: `{"type":"object","properties":{"file_path":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"required":["file_path"]}`},
		{Name: einofs.ToolNameGlob, Description: filesystemGlobDescription,
			SchemaJSON: `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"}},"required":["pattern"]}`},
		{Name: einofs.ToolNameGrep, Description: filesystemGrepDescription,
			SchemaJSON: `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"}},"required":["pattern"]}`},
	}
}

// SkillToolCatalog 返回 Skill Middleware 的渐进加载工具目录项。
func SkillToolCatalog(summary string) MiddlewareToolInfo {
	return MiddlewareToolInfo{Name: skillToolName, Description: skillToolDescription(summary),
		SchemaJSON: `{"type":"object","properties":{"skill":{"type":"string"}},"required":["skill"]}`}
}

// ToolSearchCatalog 返回 Eino 通用工具检索中间件注入的元工具目录项。
func ToolSearchCatalog() MiddlewareToolInfo {
	return MiddlewareToolInfo{Name: toolSearchToolName,
		Description: "按名称和描述检索当前 Agent 尚未直接注入的动态工具。",
		SchemaJSON:  `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`}
}
