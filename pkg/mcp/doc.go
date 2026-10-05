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

// Package mcp 提供 MCP（Model Context Protocol）客户端薄封装。
//
// 按 docs/architecture/16 §3 的设计取舍：semantic-server 只做 MCP client
// （系统集成总线的消费方），自身不对外暴露 MCP Server；传输只支持
// streamable HTTP（远程/卫星服务）与 stdio（本地 sidecar）两种，
// 本版不做 ws/OAuth/resources/prompts。
//
// 协议底座是 MCP 官方 Go SDK github.com/modelcontextprotocol/go-sdk，
// 锁定 v1.3.1——这是 go 指令仍兼容 go 1.23 的最高版本（v1.4.0 起要求
// go 1.24，v1.5.0 起要求 go 1.25）。本包只做三件薄封装：
//
//   - Client（client.go）：单 server 会话——Connect/ListTools/CallTool/Close。
//     ListTools 把工具描述截断到 2048 字符（OpenAPI 衍生的 MCP server 会把
//     15-60KB 端点文档塞进 description，不截断会挤爆模型上下文，
//     与 Claude Code 的 MAX_MCP_DESCRIPTION_LENGTH 同值）。
//   - Pool（pool.go）：连接 memoize——以配置规范化 hash 为键复用连接；
//     建连并发上限本地 3/远程 20（信号量）；连接失效剔除重建一次，
//     二次失败短路 15 分钟（返回 ErrServerUnavailable，到点半开试连）。
//   - 通知桥接（notify.go）：SDK v1.3.1 原生支持 notifications/tools/list_changed
//     （ClientOptions.ToolListChangedHandler），OnToolsChanged 直接桥接，
//     无需轮询对账入口。
//
// 消费方是 R16 的 internal/mcpregistry（工具目录发现/调用/热更新），
// 本包不反向依赖 internal。
package mcp
