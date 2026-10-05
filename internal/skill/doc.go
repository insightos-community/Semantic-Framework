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

// Package skill 实现统一技能系统的加载与存储（架构文档 06）：
// SKILL.md（YAML frontmatter + markdown 正文）从技能目录加载进 skill store，
// 经内核 skill middleware 渐进披露注入 Prompt（清单摘要常驻、命中加载全文）。
//
// 设计纪律：
//   - 软错误加载：单个 SKILL.md 失败（读取/解析/必填校验）只收集错误继续，
//     不影响其他技能；技能目录本身不可读是硬错误，由调用方决定降级策略。
//   - 快照原子替换：Store.Reload 全量重建后一次性替换快照，读路径
//     （List/Get/Summary）永远看到一致的一份；热更新下一轮 run 生效。
//   - 具身扩展字段（goal/safety_rules/component/steps/on_failure 等）只经
//     Skill.Extensions 透传，本轮不消费（Phase 3 由任务/安全模块消费），
//     不建对应的空结构体；不写多信号评分匹配器、不建多档注入——匹配与
//     注入只有 eino skill middleware 的渐进披露一条路径。
package skill
