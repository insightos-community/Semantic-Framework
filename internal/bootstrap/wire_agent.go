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
	"context"
	"fmt"

	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/agent/runtime"
	"insightos.cn/semantic-framework/internal/agent/team"
	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/interaction"
	"insightos.cn/semantic-framework/internal/mcpregistry"
	"insightos.cn/semantic-framework/internal/skill"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/llm"
	"insightos.cn/semantic-framework/pkg/log"
)

// defaultTeamName 是随启动组建的常驻 Team 名（v1 固定组建 default；
// 多 Team 路由落地前不引入 default_team 配置项）。
const defaultTeamName = "default"

// newAgentRuntime 装配 Agent 运行时层（架构文档 02 §4 启动序列第 4 步）：
// 角色 profile 加载器 + 工具体系（注册表/执行器）+ 交互服务 + AgentRuntime 服务，
// 随后按 teams_dir 登记 Leader 与请求式 SubAgent。
// leader profile 在此预加载——配置错误（目录缺失、yaml 非法、model 不在
// 注册表）在启动时暴露，而不是等到第一条用户消息才失败。
func newAgentRuntime(cfg *config.Config, logger *log.Logger,
	st *store.Store, llmReg *llm.Registry, bus *event.Bus,
	toolRegistry *tool.Registry, toolExecutor *tool.Executor,
	mcpReg *mcpregistry.Registry,
	interactionSvc *interaction.Service, skillStore *skill.Store) (*runtime.Service, error) {

	loader := profile.NewLoader(cfg.Agents.ProfilesDir)
	prof, err := loader.Load("leader")
	if err != nil {
		return nil, fmt.Errorf("加载 leader 角色 profile 失败: %w", err)
	}
	effectiveModel := prof.Model
	if effectiveModel == "" {
		effectiveModel = llmReg.Default().Name
	}
	if _, err := llmReg.Get(effectiveModel); err != nil {
		return nil, fmt.Errorf("leader 角色的 model %q 不在 LLM 注册表: %w", effectiveModel, err)
	}
	logger.Info("角色 profile 已加载",
		"role", prof.Name, "mode", string(prof.Mode), "model", effectiveModel,
		"default_inherited", prof.Model == "",
		"max_turns", prof.Limits.MaxTurns, "context_tokens", prof.Limits.ContextTokens,
		"tool_namespaces", prof.Tools.Namespaces, "approval_required", prof.Interrupt.ApprovalRequired)

	svc := runtime.NewService(runtime.Deps{
		Profiles:           loader,
		LLM:                llmReg,
		Store:              st,
		Bus:                bus,
		Logger:             logger,
		Registry:           toolRegistry,
		Executor:           toolExecutor,
		MCPRegistry:        mcpReg,
		Interaction:        interactionSvc,
		SkillStore:         skillStore,
		AllowHostExecution: cfg.Execution.AllowHost,
	})

	// Team 组建（启动序列内）：teams_dir 缺失或为空时按单 Leader 模式运行。
	teams, err := team.LoadTeams(cfg.Agents.TeamsDir)
	if err != nil {
		return nil, fmt.Errorf("加载 Team 定义失败: %w", err)
	}
	if len(teams) == 0 {
		logger.Info("未配置 Team，按单 leader 模式运行", "teams_dir", cfg.Agents.TeamsDir)
		return svc, nil
	}
	def, ok := teams[defaultTeamName]
	if !ok {
		return nil, fmt.Errorf("Team 定义 %q 不存在（已加载: %v）", defaultTeamName, team.Names(teams))
	}
	if err := svc.AssembleTeam(context.Background(), def); err != nil {
		return nil, fmt.Errorf("组建 Team %q 失败: %w", def.Name, err)
	}
	return svc, nil
}

// newSkillStore 装配技能存储（架构文档 06，启动序列第 ⑤ 步之后）：
// 加载 skills.dir 快照并启动热更监听（目录内文件变更 500ms 去抖自动 Reload）。
// 目录不可用返回 nil（WARN 降级为无技能形态——技能是上下文增强项，不阻断
// 启动，与 teams_dir 缺失同策略）；监听启动失败保留静态快照（WARN 降级为
// 不热更）。
func newSkillStore(cfg *config.Config, logger *log.Logger) *skill.Store {
	st, err := skill.NewStore(cfg.Skills.Dir, logger)
	if err != nil {
		logger.WithError(err).Warn("技能目录不可用，按无技能形态运行", "skills_dir", cfg.Skills.Dir)
		return nil
	}
	if err := st.Start(); err != nil {
		logger.WithError(err).Warn("技能目录热更监听启动失败，技能热更不生效", "skills_dir", cfg.Skills.Dir)
	}
	logger.Info("技能存储已加载", "skills_dir", cfg.Skills.Dir, "skills", len(st.List()))
	return st
}

// stopSkillStore 停止技能目录监听（装配失败的清理路径用）：
// nil 安全——无技能形态时没有可停的资源。
func stopSkillStore(st *skill.Store) {
	if st != nil {
		st.Stop()
	}
}
