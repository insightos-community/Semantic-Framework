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

// Package config 提供框架统一的配置加载能力。
//
// 配置来源优先级（高覆盖低）：进程环境变量（SEMANTIC_ 前缀，嵌套字段以 _
// 分隔，如 SEMANTIC_SERVER_HTTP_ADDR） > ./.env > ~/.semantic/.env >
// 配置文件（yaml） > 代码内默认值。.env 文件在启动早期由 LoadDotEnv 读入
// 进程 env（不覆盖已有键），随后经 env 覆盖链生效；真实 .env 不入库。
//
// 加载即校验（fail-closed）：yaml 中的未知键与类型错误聚合报出
// （ValidationError，含完整 yaml 路径），bootstrap 启动失败并输出全部问题。
//
// 热重载（Reloader，监听配置文件与 ./.env，500ms 去抖）白名单热应用表：
//
//	配置段               热应用方式
//	llm.*                pkg/llm.Registry.Reload 原子替换端点快照，API key 缓存清空
//	log.level            log.Logger.SetLevel 即时生效
//	agents.profiles_dir  重建 profile 加载器并原子替换（运行中的会话不受影响）
//	skills.dir           internal/skill.Store.Reload 原子替换技能快照（监听目录
//	                     随之切换；目录内文件变更由 store 监听自动热更）
//	mcp_servers          bootstrap 对账 mcpregistry 同步项（新增/删除/变更；
//	                     目录变化下轮 PrepareAgent 生效，无需额外广播）
//	其余（server.*/store.*）不热应用：WARN 日志"配置项 X 已变更，需重启生效"
//
// 热应用成功打 INFO 审计日志（变更段/结果）；校验失败保留旧配置，钩子失败
// 则回滚本轮已应用段。回滚失败时记 ERROR，快照保留该段实际生效值以便重试。
// 配置文件不回滚。Watcher 随 App.Run 启动、随优雅关闭停止。
//
// 配置树视图（tree.go）：Tree/TreeHash/MergeTree/DecodeTree/PatchPaths
// 把配置快照导出为 yaml 键名的通用键值树，是 settings REST 的快照序列化、
// base_hash 乐观锁与 merge patch 写回的共同基础；Reloader.ApplyExternal
// 让 settings PATCH 与文件热重载走同一白名单热应用路径。
package config
