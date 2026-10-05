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

// Package tool 是工具体系骨架（架构文档 05）：统一工具契约
// （名称/描述/参数 jsonschema/命名空间/annotations）、进程内注册表与
// 并发安全的执行器。安全管线（internal/security）以 middleware 形态
// 包住每次调用，本包不感知审批逻辑——契约（annotations.risk）是两者的衔接点。
//
// 当前职责：
//   - tool.go：Definition/Tool 契约、Registry 注册表、结构化结果与错误；
//   - executor.go：并发执行器——同轮多调用并发（命名空间级串行选项）、
//     超时硬上限、结构化错误返回；
//   - builtin/：5 个内置工具（system.time/echo/calc、artifact.put/get）。
package tool
