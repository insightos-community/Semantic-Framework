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

// Package aggregate 是 semantic-server 的会话聚合器 v1（架构文档 02 §3.4）。
//
// 聚合器是事件从域模块到客户端之间的唯一正规通道：
//
//	域模块（runtime/interaction/...）→ event bus（agent.events）
//	  → 聚合器（归一化 → 分级打标 → 落库 → 转发）→ WS hub → 客户端
//
// 职责：
//   - 归一化：补全 envelope 字段——id 由聚合器单点重新发放（保证字典序 =
//     发放序，断连续传的排序基础）、ts 兜底、agent 信息兜底；
//   - 分级：按规则表（rules.go，纯函数）打标 importance，发布方的取值
//     仅作占位（前端只按 channel/importance 渲染，打标是后端职责）；
//   - 落库：先落库（store.events）再转发 hub——凡是客户端见过的事件
//     必然可经 sync 补发，反向顺序在崩溃窗口会破坏该契约；
//   - 补发：实现 ws.EventReplayer，为断连续传（sync 上行）提供按游标
//     回放缺失事件的能力。
//
// 本包依赖 ws（Envelope 协议结构）与 store（事件持久化），不引用
// runtime/interaction 等域模块——域模块只面向 event bus 发布。
package aggregate
