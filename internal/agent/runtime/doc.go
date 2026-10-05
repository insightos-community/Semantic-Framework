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

// Package runtime 是 Agent 对话与请求式协作的运行时。
//
// 当前承载两块能力：
//
//  1. 对话闭环：用户消息经 WS/REST 进入 → 路由到 leader（协调者）
//     → kernel 执行一轮 ReAct → 文本增量经事件总线下行 → 消息与 run 落库。
//
//  2. Team 组建与 Agent 目录（roster）：Server 启动时登记 coordinator
//     与请求式 service；启用 subagent 的 service 同时注册进
//     SubAgent 注册表，Leader 装配时经 agent-as-tool 注入其工具集，
//     架构文档 04 §7.3）；roster 维护全部成员的状态/模型/当前活动，
//     供 Agent 目录 API 消费。
//
// 关键设计：
//
//  1. 会话恢复语义：内核的 Run 是请求级模型，agent 无跨 Run 记忆。
//     会话状态的唯一事实源是 store（chat_messages）；每条消息到来时
//     全量重放历史（S7）再追加新消息（S8）执行。进程重启后首条消息
//     走同一条路径（进程内 runner 缓存 miss → 重建 → 重放历史），
//     因此恢复不是特例而是常态路径——没有检查点加载，也没有状态漂移。
//
//  2. 事件与 store 的一致性顺序：先落库，再发事件。用户消息先写入
//     chat_messages 才启动 run；助手消息与 run 终态先落库才发布
//     message.done。事件是落库结果的通知而非先行，消费者看到
//     message.done 时 REST 已能查到完整消息，断连期间也不丢消息
//     （重连后经 GET messages 拉取）。
//
//  3. 同会话串行：同一会话的 HandleMessage 经会话级互斥串行化，
//     保证历史重放与消息落库的顺序一致（并发重放会读到对方尚未
//     落库的消息边界）。不同会话之间完全并行。
//
//  4. run 与连接解耦：run 在独立于 WS 连接生命周期的 context 中执行，
//     客户端断连不中断运行；运行结果照常落库。
package runtime
