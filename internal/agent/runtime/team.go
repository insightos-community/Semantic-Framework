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

package runtime

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/agent/subagent"
	"insightos.cn/semantic-framework/internal/agent/team"
	"insightos.cn/semantic-framework/internal/skill"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
)

// AssembleTeam 注册 Leader、请求式 SubAgent 和 Task Worker。Worker 只进入
// roster，供 Workflow Runtime 在 Task ready 后按 required_role 完成后绑定；
// 不会进入 SubAgent Registry，也不会被包装成 AgentTool。
func (s *Service) AssembleTeam(_ context.Context, def *team.Def) error {

	// SubAgent 注册表先于成员装配派生（leader 会话装配时消费）；
	// 无启用成员时为空目录（leader 工具集无委派面）。
	reg, err := subagent.NewRegistry(def, s.profiles)
	if err != nil {
		return fmt.Errorf("构建 SubAgent 注册表失败: %w", err)
	}
	s.subAgents = reg

	for _, member := range def.All() {
		prof, err := s.profiles.Load(member.Role)
		if err != nil {
			return fmt.Errorf("加载成员 %q 的角色 %q 失败: %w", member.ID, member.Role, err)
		}
		effectiveModel := prof.Model
		defaultInherited := false
		if effectiveModel == "" {
			effectiveModel = s.llmReg.Default().Name
			defaultInherited = true
		}
		info := AgentInfo{
			ID:                  member.ID,
			Role:                prof.Name,
			Mode:                string(prof.Mode),
			Model:               effectiveModel,
			DefaultInherited:    defaultInherited,
			ReasoningEffort:     prof.ReasoningEffort,
			ReasoningVisibility: prof.ReasoningVisibility,
			Description:         prof.Description,
			ToolNamespaces:      append([]string(nil), prof.Tools.Namespaces...),
			PinnedTools:         append([]string(nil), prof.Tools.Pinned...),
			ToolSearch:          prof.Tools.ToolSearch,
			ApprovalRequired:    append([]string(nil), prof.Interrupt.ApprovalRequired...),
			MaxTurns:            prof.Limits.MaxTurns,
			ContextTokens:       prof.Limits.ContextTokens,
			LongTermMemory:      prof.Memory.LongTerm,
		}
		if s.skills != nil {
			for _, sk := range skill.NewView(s.skills, prof.Skills.Allowlist, nil).List() {
				info.SkillNames = append(info.SkillNames, sk.Name)
			}
		}
		if info.ToolNamespaces == nil {
			info.ToolNamespaces = []string{}
		}
		if info.PinnedTools == nil {
			info.PinnedTools = []string{}
		}
		if info.ApprovalRequired == nil {
			info.ApprovalRequired = []string{}
		}
		if info.SkillNames == nil {
			info.SkillNames = []string{}
		}
		info.AgentSkillNames = append([]string{}, info.SkillNames...)

		switch prof.Mode {
		case profile.ModeCoordinator:
			if _, err := s.llmReg.Get(effectiveModel); err != nil {
				return fmt.Errorf("成员 %q 的 model %q 不在 LLM 注册表: %w", member.ID, effectiveModel, err)
			}
			info.Status = AgentStatusIdle
			info.Activity = "待命"
			s.roster.register(info)
			s.mu.Lock()
			s.leaderID = member.ID
			s.mu.Unlock()

		case profile.ModeService:
			info.Status = AgentStatusIdle
			info.Activity = "待命"
			if sub, ok := reg.Get(member.ID); ok {
				info.Activity = "待命：可经 " + sub.ToolName + " 委派"
			}
			s.roster.register(info)

		case profile.ModeWorker:
			if _, err := s.llmReg.Get(effectiveModel); err != nil {
				return fmt.Errorf("成员 %q 的 model %q 不在 LLM 注册表: %w", member.ID, effectiveModel, err)
			}
			info.Status = AgentStatusIdle
			info.Activity = "待命：由 Workflow 调度"
			s.roster.register(info)

		default:
			return fmt.Errorf("成员 %q 的模式 %q 装配未落地", member.ID, prof.Mode)
		}
		s.logger.Info("Team 成员已组建", "team", def.Name,
			"member", member.ID, "role", prof.Name, "mode", string(prof.Mode))
	}

	s.logger.Info("Team 组建完成", "team", def.Name, "members", len(def.All()))
	return nil
}

// buildSubAgentTools 把 Team 注册表中的 SubAgent 逐个装配为委派工具
// （架构文档 04 §7.3）：AgentConfig 由 profile 驱动（model/namespaces/
// max_turns）+ 共享基础设施。SubAgent 作为父 Run Trace 下的 Agent 子跨度，
// 以成员实例 ID 归因；它不会复用另一轮对话的 Trace。
//
// 返回内核工具与对应的安全门禁契约：契约须并入 leader 的门禁清单（门禁对
// 未登记工具 fail-closed），命名空间按 tool_name 归属（subagent.ToolDefinition）。
// 注册表为空（未组建 Team / 无启用成员）返回空——leader 工具集无委派面。
//
// 为什么 Name 用成员实例 ID（query-1）而非角色名（query）：冒泡事件按
// AgentName 归因到父事件流，roster/下行事件都以实例 ID 为身份基准，
// 三者对齐后转换层无需再做名字翻译。
func (s *Service) buildSubAgentTools(ctx context.Context, sessionID string) ([]kernel.Tool, []tool.Definition, error) {
	if s.subAgents == nil {
		return nil, nil, nil
	}
	var tools []kernel.Tool
	var defs []tool.Definition
	workspaceRoot, err := s.sessionWorkspaceRoot(sessionID)
	if err != nil {
		return nil, nil, err
	}
	for _, def := range s.subAgents.List() {
		prof, err := s.profiles.Load(def.Role)
		if err != nil {
			return nil, nil, fmt.Errorf("加载 SubAgent %q 的角色 %q 失败: %w", def.ID, def.Role, err)
		}
		entry, snapshot, err := s.resolveSessionModelEntry(sessionID, def.ID, prof)
		if err != nil {
			return nil, nil, err
		}
		chatModel, err := s.buildModel(ctx, entry, s.llmReg.APIKey(snapshot.EndpointID))
		if err != nil {
			return nil, nil, err
		}
		// 每个 SubAgent 独立包装自己的会话模型，不复用或回退到 Leader 模型。
		chatModel = kernel.WithSafeModelRetry(chatModel)
		// SubAgent 的独立工具链：query 的只读命名空间 + 其自身的安全门禁
		// （委派边界的审批在 SubAgent 内部工具调用层生效，leader 侧对委派
		// 工具本身不再叠加审批——风险在调用点判定，09 §4）。门禁的归因身份
		// 用成员实例 ID（审批中断冒泡到 leader 的 run 后，审批卡显示发起方）。
		subTools, _, subSafety, err := s.buildSessionToolchain(sessionID, prof, def.ID)
		if err != nil {
			return nil, nil, err
		}
		effectiveSkills, err := s.sessionSkillReader(sessionID, prof)
		if err != nil {
			return nil, nil, err
		}
		agentTool, err := kernel.BuildAgentTool(ctx, kernel.AgentConfig{
			Name:          def.ID,
			Role:          string(prof.Mode),
			Description:   prof.Description,
			Instruction:   prof.Instruction,
			SafetyDoc:     prof.SafetyDoc,
			Model:         chatModel,
			ModelName:     entry.Model,
			Tools:         subTools,
			Safety:        subSafety,
			MaxTurns:      def.MaxTurns,
			ContextTokens: prof.Limits.ContextTokens,
			Store:         s.st,
			SkillStore:    effectiveSkills,
			WorkspaceRoot: workspaceRoot,
			Price:         entry.Price,
			Purpose:       "subagent",
			Logger:        s.logger,
		}, kernel.AgentToolDef{
			ToolName:        def.ToolName,
			ToolDescription: def.ToolDescription,
			Timeout:         subagent.DelegationTimeout,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("装配 SubAgent %q 的委派工具失败: %w", def.ID, err)
		}
		tools = append(tools, agentTool)
		defs = append(defs, subagent.ToolDefinition(def))
	}
	if len(tools) > 0 {
		s.logger.Info("SubAgent 委派工具已装配", "count", len(tools))
	}
	return tools, defs, nil
}

// sessionWorkspaceRoot 读取会话所属 Project 的工作区根目录。Runner 在会话
// 首次使用时构建并固定该边界，不从模型输入或工具参数接受宿主路径。
func (s *Service) sessionWorkspaceRoot(sessionID string) (string, error) {
	session, err := s.st.GetChatSession(sessionID)
	if err != nil {
		return "", fmt.Errorf("读取会话 Project 失败: %w", err)
	}
	project, err := s.st.GetProject(session.ProjectID)
	if err != nil {
		return "", fmt.Errorf("读取会话 Project 工作区失败: %w", err)
	}
	return project.WorkspaceRoot, nil
}

// sessionSkillReader 根据会话 Project 与 Agent Profile 计算有效 Skill 集。
// 会话或绑定读取失败属于装配错误，不能静默退化为全局技能集，否则配置错误
// 会反而扩大 Agent 能力。
func (s *Service) sessionSkillReader(sessionID string, prof *profile.Profile) (kernel.SkillStore, error) {
	session, err := s.st.GetChatSession(sessionID)
	if err != nil {
		return nil, fmt.Errorf("读取会话 Skill 边界失败: %w", err)
	}
	return s.projectSkillReader(session.ProjectID, prof)
}

// projectSkillReader 返回 Agent allowlist 与 Project Skill 绑定的只读交集。
// Project 空绑定沿用 Store 的基础约定，表示不增加 Project 级限制；Agent
// allowlist 始终是不可扩大的硬边界。
func (s *Service) projectSkillReader(projectID string, prof *profile.Profile) (kernel.SkillStore, error) {
	if s.skills == nil || prof == nil || len(prof.Skills.Allowlist) == 0 {
		return nil, nil
	}
	bindings, err := s.st.GetProjectBindings(projectID)
	if err != nil {
		return nil, fmt.Errorf("读取 Project %q Skill 绑定失败: %w", projectID, err)
	}
	view := skill.NewView(s.skills, prof.Skills.Allowlist, bindings.SkillNames)
	if len(view.List()) == 0 {
		return nil, nil
	}
	return view, nil
}

// skillRoot 返回当前 Skill Store 根目录，供 Docker execute 挂载标准 Skill
// scripts；无技能形态返回空字符串。
func (s *Service) skillRoot() string {
	if s.skills == nil {
		return ""
	}
	return s.skills.Dir()
}

// Roster 返回 Team 成员与当前已注册 Robot 的统一 Agent 目录。
//
// Robot Agent 是“常驻逻辑身份”，不是常驻的大模型进程：Pilot 一旦上报 Robot，
// Server 就能稳定派生 robot:<robot_id> 及其 Profile、能力和上下文命名空间；
// 真正的模型仅在 Task 需要规划、恢复或决策时创建一次 Agent Run。这样既让
// Leader、设备页和调度器看到同一个 Robot Agent，又不会为每台空闲 Robot
// 长期占用模型连接和上下文。
func (s *Service) Roster() []AgentInfo {
	result := s.roster.list()
	if s.st == nil {
		return result
	}
	pilots, err := s.st.ListRobotPilots()
	if err != nil {
		return result
	}

	// 同一 Robot 重新连接时可能短暂保留旧 Pilot 记录。目录只展示最近上报的
	// 实例；旧实例仍保留在设备历史中，但不能形成第二个可分配 Agent。
	latest := make(map[string]store.RobotPilot)
	for _, pilot := range pilots {
		if pilot.RobotID == "" {
			continue
		}
		current, ok := latest[pilot.RobotID]
		if !ok || pilot.LastSeenAt.After(current.LastSeenAt) {
			latest[pilot.RobotID] = pilot
		}
	}
	existing := make(map[string]struct{}, len(result))
	for _, info := range result {
		existing[info.ID] = struct{}{}
	}
	prof, profileErr := s.profiles.Load("robot")
	for robotID, pilot := range latest {
		id := "robot:" + robotID
		if _, duplicated := existing[id]; duplicated {
			continue
		}
		info := AgentInfo{
			ID: id, Role: "robot", Source: "pilot", Mode: string(profile.ModeWorker),
			Status: AgentStatusOffline, Activity: "Pilot 离线",
			RobotID: robotID, PilotInstanceID: pilot.PilotInstanceID,
			RobotModel: pilot.RobotModel, Backend: pilot.Backend,
			Description:    "负责已绑定 Robot 的任务规划、Robot Skill 选择与异常恢复",
			ToolNamespaces: []string{}, PinnedTools: []string{},
			ApprovalRequired: []string{}, SkillNames: []string{}, AgentSkillNames: []string{},
		}
		if profileErr == nil {
			if s.skills != nil {
				for _, sk := range skill.NewView(s.skills, prof.Skills.Allowlist, nil).List() {
					info.AgentSkillNames = append(info.AgentSkillNames, sk.Name)
				}
			}
			info.Model = prof.Model
			info.ReasoningEffort = prof.ReasoningEffort
			info.ReasoningVisibility = prof.ReasoningVisibility
			info.Description = prof.Description
			info.ToolNamespaces = append([]string(nil), prof.Tools.Namespaces...)
			info.PinnedTools = append([]string(nil), prof.Tools.Pinned...)
			info.ToolSearch = prof.Tools.ToolSearch
			info.ApprovalRequired = append([]string(nil), prof.Interrupt.ApprovalRequired...)
			info.MaxTurns = prof.Limits.MaxTurns
			info.ContextTokens = prof.Limits.ContextTokens
			info.LongTermMemory = prof.Memory.LongTerm
			if info.Model == "" {
				info.Model = s.llmReg.Default().Name
				info.DefaultInherited = true
			}
		}
		if strings.EqualFold(pilot.Status, "online") {
			switch {
			case pilot.CurrentExecutionID != "" || strings.EqualFold(pilot.RobotStatus, "busy"):
				info.Status, info.Activity = AgentStatusRunning, "正在执行 Robot Task"
			case strings.EqualFold(pilot.AbilityFrameworkStatus, "ready"):
				info.Status, info.Activity = AgentStatusIdle, "待命：由 Workflow 后绑定任务"
			default:
				info.Status, info.Activity = AgentStatusStarting, "Pilot 在线，等待 Ability 就绪"
			}
		}
		if skills, listErr := s.st.ListRobotPilotSkills(pilot.PilotInstanceID); listErr == nil {
			for _, installed := range skills {
				if installed.Enabled && strings.EqualFold(installed.Status, "installed") {
					info.SkillNames = append(info.SkillNames, installed.Name)
					info.RobotSkillNames = append(info.RobotSkillNames, installed.Name)
				}
			}
			sort.Strings(info.SkillNames)
		}
		result = append(result, info)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// Shutdown 取消仍在执行的 Run，并在 Store 关闭前等待其收尾。
// Shutdown 停止并等待仍在执行的请求级 Agent Run。
func (s *Service) Shutdown() {
	s.mu.Lock()
	runs := make([]activeRun, 0, len(s.activeRuns))
	for _, run := range s.activeRuns {
		runs = append(runs, run)
	}
	s.mu.Unlock()
	for _, run := range runs {
		s.cancelActiveRun(run)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for _, run := range runs {
		select {
		case <-run.done:
		case <-deadline.C:
			s.logger.Warn("等待 Agent Run 停止超时", "run_id", run.runID)
			return
		}
	}
}
