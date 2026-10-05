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

// Package subagent 是 SubAgent 委派机制（架构文档 04 §7.3 的 agent-as-tool
// 机制）的注册表层：从 Team 定义与角色 profile（subagent 段）派生可委派
// 成员目录（Def），供 runtime 在 leader 装配时逐个包装为委派工具
// 注入其工具集。
//
// 设计要点（为什么这样定）：
//   - 单层委派（depth ≤1）：SubAgent 自身的工具集不允许再含委派工具
//     （kernel.BuildAgentTool 构建期防御）——嵌套委派让故障定位、预算归因
//     与中断恢复的组合复杂度爆炸，v1 禁止；
//   - 上下文隔离默认：SubAgent 每次委派是一次独立 run，输入只有任务文本
//     （不继承 leader 历史），结果摘要（其最终回复）作为工具结果回传——
//     leader 上下文不被委派过程污染，SubAgent 也不感知会话全貌；
//   - 仅 service 模式角色可启用（profile 加载期校验）：委派是请求-响应
//     短调用语义，与 coordinator/worker 的装配形态不兼容；
//   - 事件冒泡：委派执行中的子 agent 事件经 eino EmitInternalEvents 转发
//     进 leader 事件流，runtime 转换为 subagent.* 下行事件（channel=
//     dialogue），前端单视图呈现委派过程（04 §7.4）。
package subagent
