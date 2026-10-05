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

// Package mcpregistry 是 MCP 工具目录（docs/architecture/16 §4）：
// 连接 mcp_servers 声明的各 MCP server，发现其工具并维护全系统统一的
// 目录视图（命名空间、健康状态），供 Agent 工具链注入与 REST 目录查询。
//
// 设计要点（每条附"为什么"）：
//
//  1. 强类型契约 v1（entry.go）：目录条目是 Exported struct（Entry），
//     字段类型与取值集合（SourceKind/Health/Risk）全部显式声明。
//     教训来自 agent_ori：旧框架目录条目用 map[string]any 传递，
//     键名拼写/类型在消费方靠猜，重构时编译器完全沉默，线上靠运行时
//     断言兜底。本包反其道：契约即类型，编译期门禁，消费方不猜谜。
//
//  2. 内存目录，本版不落库：目录是各 server tools/list 的投影——
//     事实源在 server 侧，落库只会制造"库与远端不一致"的第二事实源
//     （重启后读到陈旧条目反而误导规划）。进程重启后首轮同步（秒级）
//     即可重建目录，持久化买不到任何东西。provider/插件包声明路径
//     （架构 16 §4.2）落地时再评估是否需要快照持久化。
//
//  3. reconcile 三态（registry.go）：每次 SyncServer 成功后做全量对账，
//     按 DeepEqual（description/schema/risk）分出 added/updated/removed，
//     然后快照替换该 server 的条目集。全量比对而非增量事件，是因为
//     tools/list 本身就是全量视图，对账天然幂等、无乱序问题。
//
//  4. warning 不清仓：连接/ListTools 失败只把该 server 条目标
//     unavailable，绝不删除——网络瞬断/进程重启是常态，若失败即清空，
//     Agent 的工具视图会随抖动反复增删，规划结果不稳定。条目只在两种
//     情况下消失：成功的对账确认 server 侧已下线该工具（reconcile
//     removed），或配置段删除了该 server（RemoveServer）。
//
//  5. 双频对账（syncer.go）：启动期快频 2s×3 轮——server 可能尚未
//     就绪或首轮握手失败，快频让目录在数秒内收敛；随后进入稳态 30s
//     周期对账。30s 的依据：list-changed 通知（架构 16 §4.3）已覆盖
//     即时变化，周期对账只是 server 未声明 listChanged 能力时的兜底，
//     低频足够，且远低于连接池 15min 短路窗口，不会放大故障。
//     tools/list_changed 到达时立即对该 server 单独对账（不等周期）。
//
//  6. 命名空间 = server 名：FullName 为 <server>.<tool>，角色
//     profile 的 tools.namespaces 直接按 server 名过滤（与内置工具
//     同一 MatchNamespaces 语义）。Entry 强类型契约中 Server 字段
//     即命名空间，不再另设别名——双事实源必然发散。
//
// 消费方：internal/agent/runtime（buildToolchain 注入工具链）、
// internal/server/http/handlers（GET /api/v1/tools 目录视图）、
// internal/bootstrap（装配与 mcp_servers 热重载对账）。
package mcpregistry
