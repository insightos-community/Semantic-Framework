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

// Package kernel 是 LLM 与 Agent 执行内核的适配层（架构文档 01 §3、04 §6）。
//
// 本包是全仓唯一允许 import eino / eino-ext 的包：上游（pkg/llm 注册表、
// runtime、bootstrap、领域层）只面向 llm.Provider 与本包暴露的封装类型
// （Runner / Message / Event / Model / Tool 别名），eino 类型不向外泄漏到
// 领域层，保证内核可替换（退守"eino core + 自研薄 loop"时只改本包）。
//
// 当前职责：
//   - factory.go：按注册表条目构建内核模型（openai 驱动 / mock 驱动）；
//   - mock.go：无 key 开发测试用的预设响应模型（与真实模型同接口）；
//   - agent.go：BuildAgent——adk.ChatModelAgent + adk.Runner 的 agent 编排，
//     按 AgentConfig（角色 profile 展开）组装；
//   - middleware.go：middleware 栈统一装配（patchtoolcalls →
//     skill（技能渐进披露） → toolsearch（动态工具检索） →
//     safety（安全门禁） → summarization → reduction →
//     context（角色提示与安全说明））；
//   - skill.go：eino skill middleware 的 Backend 适配（技能清单渲染与
//     正文 inline 加载，internal/skill 的消费方）；
//   - toolsearch.go：eino toolsearch middleware 挂接（DynamicTools 可检索
//     集注入与可见性管理，挂接条件与缓存影响的取舍见该文件）；
//   - reduction.go：eino reduction middleware 的外置后端适配——超限工具
//     结果全文落 store artifacts（路径即产物 ID 引用，artifact.get 可取回）；
//   - tools.go：工具体系（internal/tool）的内核适配——tool.Tool → eino
//     tool.BaseTool（schema 转换）、安全门禁契约（ToolCallGuard）与中断原语；
//   - agent_tool.go：SubAgent 委派工具适配（架构文档 04 §7.3 agent-as-tool）——
//     BuildAgentTool 把按 AgentConfig 构建的 SubAgent 包装为 {task} 契约的
//     委派工具（上下文隔离/超时强制/失败归一/单层嵌套防御），父 agent
//     含委派工具时自动开启 EmitInternalEvents，子 agent 事件冒泡进父事件流；
//   - checkpoint.go：adk CheckPointStore 实现（断点随 run_sessions 存）；
//   - run.go：Runner 运行器封装——历史重放输入、内核事件流 → kernel
//     Event（文本增量/工具结果/委派冒泡与结果/完成/错误/中断）的转换、
//     Resume 断点恢复；
//   - trace.go：每次 Run 动态挂载的 eino callbacks.Handler，把 Agent 根、
//     模型和工具调用写入 trace_spans，并保存模型计量。
package kernel
