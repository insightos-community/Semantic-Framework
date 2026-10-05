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

package kernel

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/adk"
	einoskill "github.com/cloudwego/eino/adk/middlewares/skill"

	"insightos.cn/semantic-framework/internal/skill"
	"insightos.cn/semantic-framework/pkg/log"
)

// skillToolName 是技能加载工具在模型侧的名字：与 eino skill middleware 的
// 默认工具名一致，这里显式固定——safety 门禁的豁免集与本名同源（见
// middleware.go 的 safety 挂接），防两处漂移；注册表工具均带命名空间
// （system.*/artifact.*），与本名不冲突。
const skillToolName = "skill"

// SkillStore 是技能存储的读取端口（middleware 栈 skill 位的技能源）：
// kernel 面向接口组装，实现方为 internal/skill.Store（面向接口以便测试桩驱动）。
type SkillStore interface {
	// List 返回当前快照的全部技能（空快照 = 无技能，不挂接 middleware）。
	List() []skill.Skill

	// Get 按名取技能（渐进披露的正文加载）；未命中返回 false。
	Get(name string) (skill.Skill, bool)

	// Summary 返回技能清单摘要文本（按 category 分组，工具描述渲染用）。
	Summary() string
}

// skillBackend 把 SkillStore 适配为 eino skill middleware 的 Backend：
// 清单 = List() 的标准字段（name/description），正文加载 = Get(name).Body。
// Eino FrontMatter 只传 name/description；context 一律为空 = inline 模式
// （技能正文作为工具结果注入当前上下文，不 fork 子 Agent）。scripts、
// references 与 assets 由正文给出现有工具的使用方式，不建立第二套 Skill
// runtime 或 permissions 协议。
type skillBackend struct {
	// store 技能存储读取端口。
	store SkillStore
}

// List 返回全部技能的 frontmatter。Backend 契约要求如实返回；
// 本装配经 CustomToolDescription 改用 Summary() 渲染清单（含 category 分组），
// eino 的默认清单模板不参与渲染。
func (b skillBackend) List(_ context.Context) ([]einoskill.FrontMatter, error) {
	skills := b.store.List()
	matters := make([]einoskill.FrontMatter, 0, len(skills))
	for _, s := range skills {
		matters = append(matters, einoskill.FrontMatter{Name: s.Name, Description: s.Description})
	}
	return matters, nil
}

// Get 按名加载技能：Content 取 SKILL.md 正文（命中后 inline 注入的内容），
// BaseDirectory 取技能目录（正文引用相对资源时的绝对路径基准）。
func (b skillBackend) Get(_ context.Context, name string) (einoskill.Skill, error) {
	s, ok := b.store.Get(name)
	if !ok {
		return einoskill.Skill{}, fmt.Errorf("技能 %q 不存在", name)
	}
	return einoskill.Skill{
		FrontMatter:   einoskill.FrontMatter{Name: s.Name, Description: s.Description},
		Content:       s.Body,
		BaseDirectory: s.Dir,
	}, nil
}

// buildSkillMiddleware 构建 eino skill middleware（渐进披露注入，架构文档 06 §5）：
//   - 常驻段（清单）：skill 工具描述 = 使用要点 + store.Summary() 清单摘要——
//     eino 每次 run 生成工具信息时重新调用，技能热更下一轮 run 即生效，
//     无需重建 agent/会话（DEBUG 日志逐 run 输出清单，热更实测的可观测点）；
//   - 命中段（全文）：模型以技能名为参数调用 skill 工具 → Get(name).Body
//     作为工具结果 inline 注入当前上下文。
//
// store 为 nil 或快照为空时不挂接（返回 nil, nil）——无技能形态与存量行为
// 一致。挂接判定在 agent 构建期：构建后新增的技能对已缓存会话不生效
// （新会话生效，与 profile 热替换语义一致，见 runtime.SetProfiles）。
func buildSkillMiddleware(ctx context.Context, store SkillStore, logger *log.Logger) (adk.ChatModelAgentMiddleware, error) {
	if store == nil || len(store.List()) == 0 {
		return nil, nil
	}
	toolName := skillToolName
	mw, err := einoskill.NewMiddleware(ctx, &einoskill.Config{
		Backend:       skillBackend{store: store},
		SkillToolName: &toolName,
		// 系统指引与工具描述使用领域化中文模板；脚本必须由正文显式指导
		// 调用 execute/execute_host，Skill middleware 本身不隐式执行文件。
		CustomSystemPrompt: func(_ context.Context, name string) string {
			return skillSystemPrompt(name)
		},
		CustomToolDescription: func(_ context.Context, _ []einoskill.FrontMatter) string {
			summary := store.Summary()
			logger.Debug("技能清单已注入", "skills", len(store.List()), "summary", summary)
			return skillToolDescription(summary)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("构建 skill middleware 失败: %w", err)
	}
	return mw, nil
}

// skillSystemPrompt 渲染技能系统的使用指引（经 adk Instruction 通道注入系统
// 提示；context middleware 的幂等判定按内容识别，本指引不影响 S1 位置，
// 见 middleware.go alreadyInjected）。
func skillSystemPrompt(toolName string) string {
	return "# Skill 系统\n\n" +
		"你可以使用一组技能（Skill）：每个技能是\"如何完成一类任务\"的指导文档，遵循渐进披露——\n" +
		"当前可见的是技能的名称与描述（见 `" + toolName + "` 工具描述中的清单），只在需要时加载全文：\n\n" +
		"1. 识别适用场景：用户任务与某个技能的描述匹配时，先调用 `" + toolName + "` 工具（参数为技能名）；\n" +
		"2. 阅读完整指导：工具结果即该技能的完整 SKILL.md 指导（工作流程、规范与示例）；\n" +
		"3. 遵循指导执行后续步骤；Skill middleware 不会自动执行 scripts，必须按正文明确调用现有工具。\n\n" +
		"仅可使用清单中列出的技能；没有匹配的技能时按自身能力自由规划。"
}

// skillToolDescription 渲染 skill 工具描述：使用要点 + 技能清单摘要
// （清单常驻模型上下文，是渐进披露的"披露面"）。
func skillToolDescription(summary string) string {
	return "加载指定技能的完整指导文档并遵循执行。\n\n" +
		"<skills_instructions>\n" +
		"当用户任务与下方某个技能的描述匹配时，先调用本工具加载该技能（参数 skill 为技能名，无其他参数），" +
		"再按返回的完整指导执行。仅可使用 <available_skills> 中列出的技能；不要重复调用已经加载过的技能。\n" +
		"</skills_instructions>\n\n" +
		"<available_skills>\n" + summary + "\n</available_skills>"
}
