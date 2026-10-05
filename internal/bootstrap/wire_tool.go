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

package bootstrap

import (
	"fmt"

	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/interaction"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/internal/tool/builtin"
	"insightos.cn/semantic-framework/pkg/log"
)

// newToolchain 装配工具体系（架构文档 05 §4）：工具注册表 + 内置工具
// 注册 + 并发执行器。启动期完成注册——契约缺漏（重名/字段不全）在此
// 暴露，而不是等到第一次工具调用。
// artifact 工具依赖 store（产物元数据 + 内容本体）；Skill 由 Eino
// Middleware 单独装配，不进入全局内置工具注册表。
func newToolchain(st *store.Store, logger *log.Logger,
	creators ...builtin.AgentToolCreator) (*tool.Registry, *tool.Executor, error) {
	registry := tool.NewRegistry()
	if err := builtin.RegisterAll(registry, st, creators...); err != nil {
		return nil, nil, fmt.Errorf("注册内置工具失败: %w", err)
	}
	logger.Info("内置工具已注册", "tools", len(registry.List()))

	executor := tool.NewExecutor(registry, tool.ExecutorOptions{Logger: logger})
	return registry, executor, nil
}

// newInteraction 装配交互服务（架构文档 12）：confirm 请求-应答闭环，
// 危险工具审批（安全管线 L4）的载体。
func newInteraction(st *store.Store, bus *event.Bus, logger *log.Logger) *interaction.Service {
	return interaction.NewService(st, bus, logger)
}
