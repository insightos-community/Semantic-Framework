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

// Package profile 是角色 profile 的加载层（架构文档 04 §3.1）。
//
// 角色 = 一个 profile 目录（configs/agents/<role>/），新增角色 = 新增目录，
// 不写 Go 代码。目录内：
//   - role.yaml：角色配置（name/mode/model/limits/tools/interrupt/memory 等）；
//   - AGENT.md：角色提示词正文（渲染进系统提示 S1，见 10 §3）；
//   - SAFETY.md：可选的全局安全总则（存在则附加到系统提示 S3 段）。
//
// 本包只做"加载 + 校验 + 默认值"：mode/model/name 必填（mode 决定装配方式，
// 未知取值报错），缺失的可选字段用默认值。reasoning_effort 只控制当前模型、
// tools.pinned/tools.tool_search 由 runtime.buildToolchain 消费（ToolSearch
// 切分）；memory 尚未消费，解析透传，由后续里程碑（长期记忆）接管。
package profile
