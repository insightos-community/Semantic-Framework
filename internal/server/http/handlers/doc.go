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

// Package handlers 是 HTTP 网关的业务处理器集合（接入层）。
//
// 当前承载四个域的 REST 面：
//   - 对话域（/api/v1/chat/*，架构文档 02 §3.2）：会话的创建/列表/删除
//     与消息查询。消息发送走 WS（/ws/chat），REST 只做会话与历史的资源管理。
//   - 设置域（/api/v1/settings/*）：生效配置快照查询（敏感值掩码）、
//     配置 PATCH（base_hash 乐观锁 + merge patch + 白名单热应用）与
//     托管密钥管理（值只写库，响应/日志/审计一律掩码或不记值）。
//   - Agent 目录（/api/v1/agents）：Team 成员清单与实时状态（roster 只读视图）。
//   - 技能库（/api/v1/skills/*，架构文档 06 §2）：技能清单摘要与详情
//     （skill store 只读视图，详情含正文与扩展字段透传）。
//   - 观测域（/api/v1/traces|metering|interactions，架构文档 13 §8，只读）：
//     链路追踪聚合与 span 明细、模型调用计量聚合与任务明细、交互记录列表
//     （status=pending 供前端审批卡刷新后恢复）。
//
// 本包与 internal/server/http 分离的原因是依赖方向：http 包的路由装配
// 需要引用本包，若本包再引用 http 的响应工具会形成包循环——因此本包
// 自带最小响应工具（格式与全网关统一）。
package handlers
