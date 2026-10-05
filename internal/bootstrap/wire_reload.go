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
	"sort"
	"strings"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/skill"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/llm"
	"insightos.cn/semantic-framework/pkg/log"
)

// EnableConfigReload 开启配置热重载：监听 configPath 与 ./.env（500ms 去抖），
// 白名单段热应用（白名单表见 pkg/config doc.go），watcher 随 App.Run 启动、
// 随优雅关闭停止。dotEnvKeys 为启动时 ./.env 写入进程 env 的键，
// 热同步只更新这些"自有键"，不触碰外部设置的环境变量。
func (a *App) EnableConfigReload(configPath string, dotEnvKeys []string) error {
	reloader, err := config.NewReloader(configPath, a.cfg, dotEnvKeys, config.Hooks{
		OnLLMChanged:         a.applyLLMConfig,
		OnLogLevelChanged:    func(level string) { a.logger.SetLevel(log.ParseLevel(level)) },
		OnProfilesDirChanged: a.applyProfilesDir,
		OnSkillsDirChanged:   a.applySkillsDir,
		OnMCPServersChanged:  a.applyMCPServers,
		OnExecutionChanged: func(execution config.ExecutionConfig) {
			a.runtime.SetHostExecutionAllowed(execution.AllowHost)
		},
	}, a.logger)
	if err != nil {
		return err
	}
	a.reloader = reloader
	// 装配 settings PATCH 的写回目标（启动期单线程赋值，先于 Run 与请求处理）。
	a.settingsCtl.configPath = configPath
	return nil
}

// applyLLMConfig 热应用 llm.* 段：先做 component 白名单校验（未知驱动
// fail-closed），再原子替换注册表快照（密钥缓存随之清空重读），
// 最后按启动同一策略巡检缺 key 端点（WARN 降级，不阻塞）。
func (a *App) applyLLMConfig(cfg config.LLMConfig) error {
	if err := checkLLMComponents(cfg); err != nil {
		return err
	}
	if err := a.llmReg.Reload(cfg); err != nil {
		return err
	}
	// 已构建的会话 Runner 持有旧 ChatModel 客户端；仅更新注册表不足以让
	// 已存在会话生效。空闲 Runner 立即淘汰，运行中 Runner 在本轮结束后淘汰。
	a.runtime.InvalidateModelRuntimes()
	for _, name := range a.llmReg.Names() {
		p, err := a.llmReg.Get(name)
		if err != nil {
			continue // Names 返回的名字必然存在，防御性跳过
		}
		if kernel.RequiresAPIKey(p.Component) && a.llmReg.APIKey(name) == "" {
			a.logger.Warn("模型端点缺少 API key，该模型暂不可用",
				"provider", name,
				"component", p.Component,
				"env", llm.APIKeyEnv(name),
			)
		}
	}
	a.logger.Info("LLM 注册表已热更新",
		"default", a.llmReg.Default().Name,
		"providers", a.llmReg.Names(),
	)
	return nil
}

// applyProfilesDir 热应用 agents.profiles_dir：重建加载器并预加载 leader
// 做校验（目录缺失/yaml 非法/model 不在注册表在此暴露），通过才原子替换进
// 运行时。本轮只到 loader 重建：运行中的会话仍持旧 profile 快照，
// 新会话经新加载器构建（不做运行中 agent 热替换）。
func (a *App) applyProfilesDir(dir string) error {
	loader := profile.NewLoader(dir)
	prof, err := loader.Load("leader")
	if err != nil {
		return fmt.Errorf("加载 leader 角色 profile 失败: %w", err)
	}
	effectiveModel := prof.Model
	if effectiveModel == "" {
		effectiveModel = a.llmReg.Default().Name
	}
	if _, err := a.llmReg.Get(effectiveModel); err != nil {
		return fmt.Errorf("leader 角色的 model %q 不在 LLM 注册表: %w", effectiveModel, err)
	}
	a.runtime.SetProfiles(loader)
	a.logger.Info("角色 profile 加载器已重建",
		"profiles_dir", dir, "role", prof.Name, "model", effectiveModel,
		"default_inherited", prof.Model == "")
	return nil
}

// applySkillsDir 热应用 skills.dir：走 store.Reload 路径（原子替换快照 +
// 监听目录集同步切换）。启动时技能目录不可用（无技能形态）时此处补建
// 存储并注入运行时——与启动路径同一降级语义：监听启动失败保留静态快照。
// 已缓存会话的中间件栈不受影响（新会话生效，与 applyProfilesDir 同语义）。
func (a *App) applySkillsDir(dir string) error {
	if a.skills == nil {
		st, err := skill.NewStore(dir, a.logger)
		if err != nil {
			return err
		}
		if err := st.Start(); err != nil {
			a.logger.WithError(err).Warn("技能目录热更监听启动失败，技能热更不生效", "skills_dir", dir)
		}
		a.skills = st
		a.runtime.SetSkillStore(st)
		a.logger.Info("技能存储已补建", "skills_dir", dir, "skills", len(st.List()))
		return nil
	}
	if err := a.skills.Reload(dir); err != nil {
		return err
	}
	a.logger.Info("技能快照已热更新", "skills_dir", dir, "skills", len(a.skills.List()))
	return nil
}

// checkLLMComponents 校验全部端点的 component 是内核已支持的驱动名，
// 聚合所有未知项一次报出（Wire 启动装配与热重载共用，fail-closed）。
func checkLLMComponents(cfg config.LLMConfig) error {
	var problems []string
	for name, p := range cfg.Providers {
		if !kernel.KnownComponent(p.Component) {
			problems = append(problems,
				fmt.Sprintf("llm.providers.%s.component: unknown component %q", name, p.Component))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems) // map 遍历无序，排序保证输出稳定
		return fmt.Errorf("配置校验失败:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}
