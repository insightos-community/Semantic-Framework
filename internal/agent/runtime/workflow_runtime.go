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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/cloudwego/eino/adk/middlewares/filesystem"
	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/contract"
	"insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/skill"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/internal/workflow"
)

var conversationPlanningFilesystemTools = map[string]struct{}{
	filesystem.ToolNameLs: {}, filesystem.ToolNameReadFile: {},
	filesystem.ToolNameGlob: {}, filesystem.ToolNameGrep: {},
}

const planningSkillTool = "skill"

// planningToolPolicy 是 Planning Run 的执行点硬门禁。Profile 负责缩小模型
// 可见工具，本策略再覆盖内置、MCP 和 Filesystem 工具，未知名称一律拒绝。
type planningToolPolicy struct{}

func (planningToolPolicy) WrapToolCall(ctx context.Context, meta kernel.ToolCallMeta,
	argsJSON string, next kernel.ToolCallEndpoint) (string, error) {
	// Task Planning 不自动注入整张地图，但允许 Agent 在确有需要时查询当前
	// 语义事实或询问用户。文件和 Artifact 探索仍然关闭，避免把历史材料误当成
	// 当前场景；Robot 物理工具则只属于后续 Task Execution Run。
	if meta.Name != planningSkillTool && meta.Name != "map.query" && meta.Name != "interaction.ask" {
		return tool.ErrorResult(tool.CodeToolError,
			fmt.Sprintf("Planning Run 不允许调用工具 %q", meta.Name), false), nil
	}
	return next(ctx, argsJSON)
}

type conversationPlanToolPolicy struct{}

func (conversationPlanToolPolicy) WrapToolCall(ctx context.Context, meta kernel.ToolCallMeta,
	argsJSON string, next kernel.ToolCallEndpoint) (string, error) {
	if _, ok := conversationPlanningFilesystemTools[meta.Name]; !ok &&
		meta.Name != planningSkillTool && !tool.ConversationPlanToolAllowed(meta.Name) {
		return tool.ErrorResult(tool.CodeToolError,
			fmt.Sprintf("Plan Mode 不允许调用工具 %q", meta.Name), false), nil
	}
	return next(ctx, argsJSON)
}

// taskDecisionToolPolicy 只保护 Robot Skill checkpoint 决策与失败恢复 Run。
// 两者仍使用同一个 Robot Agent 和 Task Context，但都不能借一次局部判断再次
// 下发物理动作。允许集合由运行目的给出，未知工具一律在执行点拒绝。
type taskDecisionToolPolicy struct {
	allowed map[string]struct{}
}

func (p taskDecisionToolPolicy) WrapToolCall(ctx context.Context, meta kernel.ToolCallMeta,
	argsJSON string, next kernel.ToolCallEndpoint) (string, error) {
	if _, ok := p.allowed[meta.Name]; !ok {
		return tool.ErrorResult(tool.CodeToolError,
			fmt.Sprintf("当前 Task 决策 Run 不允许调用工具 %q", meta.Name), false), nil
	}
	return next(ctx, argsJSON)
}

type purposeToolFailure struct {
	Tool    string
	Code    string
	Message string
}

func parsePurposeToolFailure(toolName, result string) *purposeToolFailure {
	var envelope struct {
		OK    bool `json:"ok"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(result), &envelope) != nil || envelope.OK || envelope.Error == nil {
		return nil
	}
	return &purposeToolFailure{Tool: toolName, Code: envelope.Error.Code, Message: envelope.Error.Message}
}

func missingRobotExecutionError(last *purposeToolFailure) error {
	if last != nil {
		return fmt.Errorf("Robot Agent未成功建立Robot Execution；最后工具错误 %s/%s: %s",
			last.Tool, last.Code, last.Message)
	}
	return errors.New("ROBOT_TOOL_CALL_REQUIRED: Robot Agent未成功建立Robot Execution；本Run没有产生可关联的robot.run执行记录；本轮没有可归因的工具拒绝，普通文本或JSON中的调用描述不等于真实工具调用")
}

// ResolveTaskAssignment 在 Task 依赖满足后使用实时目录完成后绑定。非 Robot
// Worker 可复用同一角色 Profile 并依靠独立 Task Context 并行；Robot 则必须
// 选择在线、空闲且满足声明能力的真实设备，不能退回静态 robot-1。
func (s *Service) ResolveTaskAssignment(_ context.Context,
	request workflow.TaskAssignmentRequest) (workflow.TaskAssignment, error) {
	role := strings.TrimSpace(request.Task.RequiredRole)
	if role == "robot" {
		return s.resolveRobotTaskAssignment(request.Workflow, request.Task)
	}
	for _, info := range s.roster.list() {
		if info.Mode == string(profile.ModeWorker) && info.Role == role &&
			info.Status != AgentStatusStopped {
			return workflow.TaskAssignment{AgentID: info.ID}, nil
		}
	}
	return workflow.TaskAssignment{}, workflow.ErrTaskWaitingResource
}

func (s *Service) resolveRobotTaskAssignment(wf store.Workflow, task store.Task) (workflow.TaskAssignment, error) {
	required := stringListJSON(task.RequiredCapabilities)
	requirements := robotResourceRequirements(task.ResourceRequirements)
	approved := parseRobotApprovedScope(wf.ApprovedScope)
	pilots, err := s.st.ListRobotPilots()
	if err != nil {
		return workflow.TaskAssignment{}, err
	}
	var fallback workflow.TaskAssignment
	for _, pilot := range pilots {
		if pilot.Status != "online" || pilot.RobotStatus != "idle" ||
			pilot.CurrentExecutionID != "" || pilot.AbilityFrameworkStatus != "ready" {
			continue
		}
		reserved, reserveErr := s.st.IsRobotTaskReserved(pilot.RobotID, task.ID)
		if reserveErr != nil {
			return workflow.TaskAssignment{}, reserveErr
		}
		if reserved {
			continue
		}
		if !matchesOptional(approved.RobotIDs, pilot.RobotID) ||
			!matchesOptional(approved.RobotModels, pilot.RobotModel) ||
			!matchesOptional(requirements.RobotIDs, pilot.RobotID) ||
			!matchesOptional(requirements.RobotModels, pilot.RobotModel) ||
			!matchesOptional(requirements.Backends, pilot.Backend) {
			continue
		}
		available := make(map[string]struct{})
		for _, ability := range pilot.Abilities {
			if healthy, _ := ability["healthy"].(bool); healthy {
				addCapability(available, ability["role"])
				addCapabilities(available, ability["actions"])
			}
		}
		skills, listErr := s.st.ListRobotPilotSkills(pilot.PilotInstanceID)
		if listErr != nil {
			continue
		}
		for _, skill := range skills {
			if !matchesOptional(approved.AllowedSkills, skill.Name) {
				continue
			}
			if skill.Enabled && skill.Status == "installed" {
				available[skill.Name] = struct{}{}
			}
		}
		matched := true
		for _, capability := range required {
			if _, ok := available[capability]; !ok {
				matched = false
				break
			}
		}
		if matched {
			candidate := workflow.TaskAssignment{AgentID: "robot:" + pilot.RobotID,
				RobotID: pilot.RobotID}
			if pilot.RobotID == task.AssignedRobotID {
				return candidate, nil
			}
			if fallback.AgentID == "" {
				fallback = candidate
			}
		}
	}
	if fallback.AgentID != "" {
		return fallback, nil
	}
	return workflow.TaskAssignment{}, workflow.ErrTaskWaitingResource
}

func stringListJSON(raw json.RawMessage) []string {
	var values []string
	_ = json.Unmarshal(raw, &values)
	return values
}

type robotApprovedScope struct {
	RobotIDs      []string `json:"robot_ids"`
	RobotModels   []string `json:"robot_models"`
	AllowedSkills []string `json:"allowed_skills"`
}

func parseRobotApprovedScope(raw json.RawMessage) robotApprovedScope {
	var value robotApprovedScope
	_ = json.Unmarshal(raw, &value)
	return value
}

type robotRequirements struct {
	RobotIDs    []string `json:"robot_ids"`
	RobotModels []string `json:"robot_models"`
	Backends    []string `json:"backends"`
}

func robotResourceRequirements(raw json.RawMessage) robotRequirements {
	var value robotRequirements
	_ = json.Unmarshal(raw, &value)
	return value
}

func matchesOptional(allowed []string, actual string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, value := range allowed {
		if strings.TrimSpace(value) == actual {
			return true
		}
	}
	return false
}

func addCapabilities(result map[string]struct{}, value any) {
	switch items := value.(type) {
	case []any:
		for _, item := range items {
			addCapability(result, item)
		}
	case []string:
		for _, item := range items {
			addCapability(result, item)
		}
	}
}

func addCapability(result map[string]struct{}, value any) {
	text, _ := value.(string)
	if text = strings.TrimSpace(text); text != "" {
		result[text] = struct{}{}
	}
}

func robotSDKOptionFloat(configuration map[string]any, name string) float64 {
	sdk, _ := configuration["sdk"].(map[string]any)
	options, _ := sdk["options"].(map[string]any)
	value, _ := options[name].(float64)
	if value > 0 {
		return value
	}
	return 0
}

func robotBaseFootprintRadius(configuration map[string]any) float64 {
	return robotSDKOptionFloat(configuration, "base_footprint_radius_m")
}

func robotNavigationResolution(configuration map[string]any) float64 {
	return robotSDKOptionFloat(configuration, "navigation_resolution_m")
}

func robotManipulationWorkDistance(configuration map[string]any) float64 {
	return robotSDKOptionFloat(configuration, "manipulation_work_distance_m")
}

type robotTaskPlanningView struct {
	Robot  robotTaskRobotView   `json:"robot"`
	Skills []robotTaskSkillView `json:"skills"`
}

type robotTaskRobotView struct {
	ID                        string  `json:"id"`
	PilotInstanceID           string  `json:"pilot_instance_id"`
	Model                     string  `json:"model"`
	Backend                   string  `json:"backend"`
	Status                    string  `json:"status"`
	AbilityFrameworkStatus    string  `json:"ability_framework_status"`
	CurrentExecutionID        string  `json:"current_execution_id,omitempty"`
	BaseFootprintRadiusM      float64 `json:"base_footprint_radius_m,omitempty"`
	NavigationResolutionM     float64 `json:"navigation_resolution_m,omitempty"`
	ManipulationWorkDistanceM float64 `json:"manipulation_work_distance_m,omitempty"`
}

type robotTaskSkillView struct {
	Name            string           `json:"name"`
	Version         string           `json:"version"`
	Description     string           `json:"description"`
	WhenToUse       string           `json:"when_to_use,omitempty"`
	InputModel      string           `json:"input_model,omitempty"`
	ResultModel     string           `json:"result_model,omitempty"`
	RequiredActions []map[string]any `json:"required_actions,omitempty"`
	StopActions     []map[string]any `json:"stop_actions,omitempty"`
	Documentation   string           `json:"documentation,omitempty"`
}

// taskPlanningSkillStore 复用内核既有 skill 工具，将 Project Agent Skill 与
// 当前 Robot 实际安装的 Robot Skill 合并成一个只读视图。常驻 Prompt 只显示
// 摘要，模型确实需要某份契约时才加载正文，避免每轮重复注入全部 SKILL.md。
type taskPlanningSkillStore struct {
	base  kernel.SkillStore
	robot map[string]skill.Skill
}

func (s *taskPlanningSkillStore) List() []skill.Skill {
	items := make(map[string]skill.Skill)
	if s != nil && s.base != nil {
		for _, item := range s.base.List() {
			items[item.Name] = item
		}
	}
	if s != nil {
		for name, item := range s.robot {
			items[name] = item
		}
	}
	result := make([]skill.Skill, 0, len(items))
	for _, item := range items {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (s *taskPlanningSkillStore) Get(name string) (skill.Skill, bool) {
	if s != nil {
		if item, ok := s.robot[name]; ok {
			return item, true
		}
		if s.base != nil {
			return s.base.Get(name)
		}
	}
	return skill.Skill{}, false
}

func (s *taskPlanningSkillStore) Summary() string {
	items := s.List()
	lines := make([]string, 0, len(items))
	for _, item := range items {
		lines = append(lines, "- "+item.Name+": "+item.Description)
	}
	return strings.Join(lines, "\n")
}

// robotTaskPlanningCatalog 把“实际分配的 Robot”和该 Pilot 已安装且启用的
// Robot Skill 合并成一个只读目录。Robot Agent 不需要分别理解设备页、Skill
// Registry 和 Pilot 安装状态；它看到的就是当前 Task 真正可执行的语义能力。
// 目录只暴露语义模型和 SKILL.md，不暴露 Endpoint、文件路径或 Ability UUID。
func (s *Service) robotTaskPlanningCatalog(wf store.Workflow, task store.Task) (robotTaskPlanningView, error) {
	if strings.TrimSpace(task.AssignedRobotID) == "" {
		return robotTaskPlanningView{}, fmt.Errorf("Robot Task 尚未分配 Robot: %w",
			workflow.ErrTaskWaitingResource)
	}
	pilot, err := s.st.GetActiveRobotPilot(task.AssignedRobotID)
	if err != nil {
		return robotTaskPlanningView{}, err
	}
	installed, err := s.st.ListRobotPilotSkills(pilot.PilotInstanceID)
	if err != nil {
		return robotTaskPlanningView{}, err
	}
	approved := parseRobotApprovedScope(wf.ApprovedScope)
	view := robotTaskPlanningView{Robot: robotTaskRobotView{
		ID: pilot.RobotID, PilotInstanceID: pilot.PilotInstanceID,
		Model: pilot.RobotModel, Backend: pilot.Backend, Status: pilot.RobotStatus,
		AbilityFrameworkStatus:    pilot.AbilityFrameworkStatus,
		CurrentExecutionID:        pilot.CurrentExecutionID,
		BaseFootprintRadiusM:      robotBaseFootprintRadius(pilot.Configuration),
		NavigationResolutionM:     robotNavigationResolution(pilot.Configuration),
		ManipulationWorkDistanceM: robotManipulationWorkDistance(pilot.Configuration),
	}}
	for _, actual := range installed {
		if !matchesOptional(approved.AllowedSkills, actual.Name) {
			continue
		}
		if !actual.Enabled || !strings.EqualFold(actual.Status, "installed") {
			continue
		}
		pkg, getErr := s.st.GetRobotSkillPackage(actual.Name, actual.Version)
		if getErr != nil {
			return robotTaskPlanningView{}, fmt.Errorf(
				"Robot %s 的 Skill %s@%s 缺少 Server 发布记录: %w",
				pilot.RobotID, actual.Name, actual.Version, getErr)
		}
		detail, loadErr := robot.LoadSkillPackageDetail(pkg)
		if loadErr != nil {
			return robotTaskPlanningView{}, loadErr
		}
		var inputModel, resultModel string
		if runtimeSpec, ok := detail.Extensions["runtime"].(map[string]any); ok {
			inputModel, _ = runtimeSpec["input_model"].(string)
			resultModel, _ = runtimeSpec["result_model"].(string)
		}
		view.Skills = append(view.Skills, robotTaskSkillView{
			Name: detail.Name, Version: detail.Version, Description: detail.Description,
			WhenToUse: detail.WhenToUse, InputModel: inputModel, ResultModel: resultModel,
			RequiredActions: detail.RequiredActions, StopActions: detail.StopActions,
		})
	}
	sort.Slice(view.Skills, func(i, j int) bool {
		if view.Skills[i].Name == view.Skills[j].Name {
			return view.Skills[i].Version < view.Skills[j].Version
		}
		return view.Skills[i].Name < view.Skills[j].Name
	})
	if len(view.Skills) == 0 {
		return robotTaskPlanningView{}, fmt.Errorf("Robot %s 没有已启用的 Robot Skill: %w",
			pilot.RobotID, workflow.ErrTaskWaitingResource)
	}
	return view, nil
}

func (s *Service) taskPlanningSkills(projectID, agentID string,
	catalog robotTaskPlanningView) (kernel.SkillStore, error) {
	prof, err := s.profileForAgent(agentID)
	if err != nil {
		return nil, err
	}
	base, err := s.projectSkillReader(projectID, prof)
	if err != nil {
		return nil, err
	}
	combined := &taskPlanningSkillStore{base: base, robot: make(map[string]skill.Skill)}
	for _, item := range catalog.Skills {
		pkg, getErr := s.st.GetRobotSkillPackage(item.Name, item.Version)
		if getErr != nil {
			return nil, getErr
		}
		detail, loadErr := robot.LoadSkillPackageDetail(pkg)
		if loadErr != nil {
			return nil, loadErr
		}
		combined.robot[item.Name] = skill.Skill{Name: item.Name,
			Description: item.Description, Category: "robot", WhenToUse: item.WhenToUse,
			Body: detail.Body}
	}
	if len(combined.List()) == 0 {
		return nil, nil
	}
	return combined, nil
}

// PlanTask 由 Task 指定的 Worker 运行，并把输入/输出写入该 Task Context。
func (s *Service) PlanTask(ctx context.Context, request workflow.TaskPlanRequest) ([]store.SubTaskDraft, error) {
	result, err := s.PlanTaskWithResult(ctx, request)
	return result.SubTasks, err
}

// PlanTaskWithResult 在不改变现有 Planner 调度接口的前提下保留模型生成的
// summary。Scheduler 接入群聊里程碑后可使用增量接口读取摘要；机器执行始终
// 只消费经过校验的 SubTasks。
func (s *Service) PlanTaskWithResult(ctx context.Context,
	request workflow.TaskPlanRequest) (workflow.TaskPlanResult, error) {
	task, err := s.st.GetTask(request.Task.ID)
	if err != nil {
		return workflow.TaskPlanResult{}, fmt.Errorf("Task Planning 缺少持久 Task: %w", err)
	}
	// Task input 是 Leader 已确认的业务信息；Map 不会被隐式塞入 Prompt，但
	// Robot Agent可以按需调用 map.query 补充规划依据。查询结果仍只是规划参考，
	// 后续 Robot Skill 必须通过 Ability 重新观察实时物理状态。
	input, err := json.Marshal(struct {
		Task   store.TaskDraft    `json:"task"`
		Answer *store.Interaction `json:"interaction_answer,omitempty"`
	}{Task: request.Task, Answer: request.Answer})
	if err != nil {
		return workflow.TaskPlanResult{}, err
	}
	robotCatalog := ""
	var catalog robotTaskPlanningView
	if task.RequiredRole == "robot" {
		catalogValue, catalogErr := s.robotTaskPlanningCatalog(request.Workflow, task)
		if catalogErr != nil {
			return workflow.TaskPlanResult{}, catalogErr
		}
		catalog = catalogValue
		// Task planning selects semantic skills, not their low-level action routes.
		// Keep the full catalog for validation and execution, but omit action schemas
		// from this model input. The skill tool still exposes documentation on demand.
		planningCatalog := catalogValue
		planningCatalog.Skills = append([]robotTaskSkillView(nil), catalogValue.Skills...)
		for i := range planningCatalog.Skills {
			planningCatalog.Skills[i].RequiredActions = nil
			planningCatalog.Skills[i].StopActions = nil
		}
		encoded, marshalErr := json.Marshal(planningCatalog)
		if marshalErr != nil {
			return workflow.TaskPlanResult{}, marshalErr
		}
		robotCatalog = "\n已分配 Robot 的统一执行目录：" + string(encoded)
	}
	planningSkills, err := s.taskPlanningSkills(request.Project.ID, task.AssignedAgentID, catalog)
	if err != nil {
		return workflow.TaskPlanResult{}, err
	}
	prompt := buildTaskPlanningPrompt(input, robotCatalog)
	var result workflow.TaskPlanResult
	_, err = s.runTaskAgent(ctx, request.UserID, request.Project, request.Conversation,
		request.Workflow, "", &task, task.AssignedAgentID, prompt, request.Answer,
		planningSkills, store.RunKindTaskPlanning, "",
		func(text string) error {
			value, validationErr := decodeAndValidateTaskPlanResult(text, task, catalog)
			if validationErr == nil {
				result = value
			}
			return validationErr
		})
	if err != nil {
		return workflow.TaskPlanResult{}, err
	}
	return result, nil
}

func decodeAndValidateTaskPlan(text string, task store.Task,
	catalog robotTaskPlanningView) ([]store.SubTaskDraft, error) {
	result, err := decodeAndValidateTaskPlanResult(text, task, catalog)
	return result.SubTasks, err
}

func decodeAndValidateTaskPlanResult(text string, task store.Task,
	catalog robotTaskPlanningView) (workflow.TaskPlanResult, error) {
	var result workflow.TaskPlanResult
	if err := decodeStrictJSON(text, &result); err != nil {
		return workflow.TaskPlanResult{}, fmt.Errorf("Task Planning Run 返回非法 JSON: %w", err)
	}
	if len(result.SubTasks) == 0 {
		return workflow.TaskPlanResult{}, fmt.Errorf("Task Planning Run 未返回 SubTask: %w", store.ErrInvalidState)
	}
	if err := store.ValidateSubTaskDrafts(result.SubTasks); err != nil {
		return workflow.TaskPlanResult{}, fmt.Errorf("SubTask 图结构不合法：goal 必须非空，kind 必须合法；显式 id 不得重复，depends_on 必须指向本次计划中的其他步骤且不能成环: %w", err)
	}
	if task.RequiredRole != "robot" {
		for _, item := range result.SubTasks {
			if item.Kind != "agent_step" {
				return workflow.TaskPlanResult{}, fmt.Errorf("非 Robot Task 只能生成 agent_step: %w", store.ErrInvalidState)
			}
		}
		return result, nil
	}

	available := make(map[string]struct{}, len(catalog.Skills))
	for _, skill := range catalog.Skills {
		available[skill.Name+"@"+skill.Version] = struct{}{}
	}
	for _, item := range result.SubTasks {
		if item.Kind != "robot_skill" {
			return workflow.TaskPlanResult{}, fmt.Errorf("Robot Task 只能生成 robot_skill: %w", store.ErrInvalidState)
		}
		var spec struct {
			SkillName    string          `json:"skill_name"`
			SkillVersion string          `json:"skill_version"`
			Intent       json.RawMessage `json:"intent"`
		}
		if err := decodeStrictJSON(string(item.Spec), &spec); err != nil {
			return workflow.TaskPlanResult{}, fmt.Errorf("SubTask %q 的 spec 必须且只能包含 skill_name、skill_version、intent: %w",
				item.ID, err)
		}
		key := strings.TrimSpace(spec.SkillName) + "@" + strings.TrimSpace(spec.SkillVersion)
		if _, ok := available[key]; !ok {
			return workflow.TaskPlanResult{}, fmt.Errorf("SubTask %q 引用了当前 Robot 未安装启用的 Skill %q: %w",
				item.ID, key, store.ErrInvalidState)
		}
		var intent map[string]any
		if len(spec.Intent) == 0 || json.Unmarshal(spec.Intent, &intent) != nil || intent == nil {
			return workflow.TaskPlanResult{}, fmt.Errorf("SubTask %q 的 spec.intent 必须是 JSON object: %w",
				item.ID, store.ErrInvalidState)
		}
		if err := rejectReservedPlanningIntent(item.ID, intent); err != nil {
			return workflow.TaskPlanResult{}, err
		}
	}
	return result, nil
}

// reservedPlanningIntentFields 是执行期物理状态，不是稳定业务身份。
// W-F1-01 把 carrying_object=false 写进来源导航 intent 后，16 次 Skill
// 全部成功，Gate 仍判 protocol_failed。规划只校验信封时拦不住，必须在
// 修正循环内按字段名拒绝，避免整轮搬运空跑。
var reservedPlanningIntentFields = []string{"carrying_object", "held_object", "holding_object"}

func rejectReservedPlanningIntent(subtaskID string, intent map[string]any) error {
	if path := runtimeHoldingStatePath(intent, "spec.intent"); path != "" {
		return fmt.Errorf("SubTask %q 的 %s 属于执行期持物状态，不能写入规划 intent；只保留 object_ref/carried_object_ref 等稳定引用，由执行期重新观测；目标条件写在外层 completion_criteria: %w",
			subtaskID, path, store.ErrInvalidState)
	}
	return nil
}

func decodeAndValidateTaskRecovery(text string, task store.Task,
	catalog robotTaskPlanningView) (workflow.TaskRecoveryDecision, error) {
	// Keep the upstream entry point while using the same validator as RecoverTask;
	// tests and production must not drift into separate recovery contracts.
	return decodeTaskRecoveryDecision(text, task, catalog)
}

// runtimeHoldingStatePath 不校验 Skill 的 Pydantic 输入，也不改写模型输出。
// 它只在规划边界拒绝被误当成事实的持物状态（含 false/null），避免执行后
// 才被产品 Gate 发现。递归检查防止通过嵌套对象绕过；稳定排序提供确定的
// 字段路径诊断，纠正次数由统一预算控制，而非按错误文本提前终止。
func runtimeHoldingStatePath(value any, path string) string {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := path + "." + key
			for _, reserved := range reservedPlanningIntentFields {
				if key == reserved {
					return child
				}
			}
			if found := runtimeHoldingStatePath(typed[key], child); found != "" {
				return found
			}
		}
	case []any:
		for i, item := range typed {
			if found := runtimeHoldingStatePath(item, fmt.Sprintf("%s[%d]", path, i)); found != "" {
				return found
			}
		}
	}
	return ""
}

// ExecuteTask 按 Task 后绑定的 Agent 身份装配一次短 Worker Run。Developer、
// Map、Monitor 与 Robot 共用执行骨架，但 Profile、工具和当前 SubTask 均由
// Task.required_role 决定；不会再把所有任务伪装成 Developer Task。
func (s *Service) ExecuteTask(ctx context.Context,
	execution workflow.TaskExecution) (workflow.TaskResult, error) {
	rt, err := s.buildPurposeRuntime(ctx, execution.Conversation.ID, execution.Task.AssignedAgentID,
		store.RunKindTaskExecution)
	if err != nil {
		return workflow.TaskResult{}, err
	}
	run, err := s.preparePurposeRun(execution.Run, rt)
	if err != nil {
		return workflow.TaskResult{}, err
	}
	release, runCtx, err := s.registerPurposeRun(ctx, run, rt)
	if err != nil {
		return workflow.TaskResult{}, err
	}
	defer release()

	robotSubTask := len(execution.SubTasks) == 1 &&
		execution.SubTasks[0].Kind == "robot_skill"
	var messages []*schema.Message
	if robotSubTask {
		messages, err = s.loadRobotTaskExecutionContext(execution)
	} else {
		messages, err = s.loadTaskContext(execution)
	}
	if err != nil {
		return workflow.TaskResult{}, err
	}
	prompt, err := s.taskExecutionPrompt(execution)
	if err != nil {
		return workflow.TaskResult{}, err
	}
	if robotSubTask {
		prompt, err = s.withSkillInputContract(runCtx, execution, prompt)
		if err != nil {
			return workflow.TaskResult{}, err
		}
		prompt += "\n" + robotToolTransportInstruction
	}
	userMessage := schema.UserMessage(prompt)
	messageID, err := s.appendTaskMessage(execution, run, execution.Task.AssignedAgentID, userMessage)
	if err != nil {
		return workflow.TaskResult{}, err
	}
	messages = append(messages, userMessage)
	runCtx, err = s.withTaskSummaryPersistence(runCtx, execution, messageID)
	if err != nil {
		return workflow.TaskResult{}, err
	}
	text := ""
	for exchange := 0; ; exchange++ {
		var exchangeFailure *purposeToolFailure
		text, exchangeFailure, err = s.consumePurposeRun(runCtx, rt, run, messages)
		if err != nil {
			return workflow.TaskResult{}, err
		}
		if _, pendingErr := s.st.GetPendingInteractionByRunID(run.ID); pendingErr == nil {
			return workflow.TaskResult{}, workflow.ErrTaskWaitingInput
		} else if !errors.Is(pendingErr, store.ErrNotFound) {
			return workflow.TaskResult{}, pendingErr
		}
		if !robotSubTask {
			break
		}
		if _, executionErr := s.st.GetRobotExecutionBySubTask(execution.SubTasks[0].ID); executionErr == nil {
			break
		} else if !errors.Is(executionErr, store.ErrNotFound) {
			return workflow.TaskResult{}, executionErr
		}
		if exchange+1 >= rt.profile.Limits.MaxTurns {
			return workflow.TaskResult{}, fmt.Errorf(
				"Robot Agent达到Agent Profile max_turns；%w", missingRobotExecutionError(exchangeFailure))
		}
		// Only retry if robot.run has NOT created a persistent Execution. Never
		// rerun an accepted physical action merely to repair its textual report.
		// Diagnose this exchange only. An earlier rejected enum must not mask a
		// later text-only reply whose parameters may already have been corrected.
		if correctionErr := rt.contractCorrections.request(runCtx, missingRobotExecutionError(exchangeFailure)); correctionErr != nil {
			return workflow.TaskResult{}, correctionErr
		}
		correction := buildRobotExecutionFeedback(exchangeFailure)
		messages = append(messages,
			schema.AssistantMessage(text, nil),
			schema.UserMessage(correction))
	}
	assistant := schema.AssistantMessage(text, nil)
	if _, err := s.appendTaskMessage(execution, run, execution.Task.AssignedAgentID, assistant); err != nil {
		return workflow.TaskResult{}, fmt.Errorf("保存 Task Agent 执行说明: %w", err)
	}
	if robotSubTask {
		// robot.run 已经建立持久 Robot Execution 时，本次 Agent Run 的职责
		// 就在“下发成功”处结束。模型随后生成的自然语言只属于对话说明，不能
		// 再按同步 Worker 结果解析，更不能提前完成 SubTask；后续状态只能由
		// Robot Execution 终态事件驱动。若模型没有真正调用 robot.run，Store
		// 中不会存在该关联，仍继续走严格 JSON 校验并明确失败。
		if _, executionErr := s.st.GetRobotExecutionBySubTask(execution.SubTasks[0].ID); executionErr == nil {
			return workflow.TaskResult{}, nil
		}
	}
	// This Agent may already have written files or called external tools. Reject
	// malformed reports, but never rerun the whole tool-enabled execution merely
	// to fix its final JSON (that could duplicate side effects).
	checkStart := time.Now()
	outcome, validationErr := validateTaskOutcome(text)
	s.recordContractCheck(run, "worker_result", 1, checkStart, validationErr)
	if validationErr != nil {
		return workflow.TaskResult{}, validationErr
	}
	return workflow.TaskResult{Summary: outcome.Summary, Evidence: outcome.Evidence}, nil
}

// ResolveRobotAgentRequest 在原 Task Context 内启动一次短决策 Run。Skill 已经
// 停在安全 checkpoint，所以本轮只允许读取上下文并返回声明模型所需的 JSON；
// 它不装配 robot.run/stop，也不能通过一个“建议”偷偷启动新的物理动作。
func (s *Service) ResolveRobotAgentRequest(ctx context.Context,
	request workflow.RobotAgentDecisionRequest) (map[string]any, error) {
	var decision map[string]any
	reviewer, err := checkpointReviewer(request.Event, func(value map[string]any) { decision = value })
	if err != nil {
		return nil, err
	}
	input, err := json.Marshal(map[string]any{
		"workflow_goal":      request.Workflow.Goal,
		"task_goal":          request.Task.Goal,
		"subtask_goal":       request.SubTask.Goal,
		"task_input":         request.Task.Input,
		"subtask_spec":       request.SubTask.Spec,
		"assigned_robot_id":  request.Task.AssignedRobotID,
		"robot_execution_id": request.Execution.ID,
		"skill_name":         request.Execution.SkillName,
		"stage":              request.Event["stage"],
		"decision_key":       request.Event["decision_key"],
		"decision_revision":  request.Event["decision_revision"],
		"reason":             request.Event["reason"],
		"context":            request.Event["context"],
		"response_model":     request.Event["response_model"],
		"response_schema":    request.Event["response_schema"],
	})
	if err != nil {
		return nil, err
	}
	navigationDecision := request.Execution.SkillName == "semantic-navigation"
	prompt := buildRobotAgentRequestPrompt(input, navigationDecision)
	planningSkills, skillErr := s.taskPlanningSkills(request.Project.ID,
		request.Task.AssignedAgentID, robotTaskPlanningView{})
	if skillErr != nil {
		return nil, skillErr
	}
	decisionPurpose := runtimePurposeRobotDecision
	if navigationDecision {
		decisionPurpose = runtimePurposeRobotNavigationDecision
	}
	_, err = s.runTaskAgent(ctx, request.UserID, request.Project, request.Conversation,
		request.Workflow, "", &request.Task, request.Task.AssignedAgentID, prompt, request.Answer,
		planningSkills, store.RunKindTaskExecution, decisionPurpose,
		reviewer)
	if err != nil {
		return nil, err
	}
	if len(decision) == 0 {
		return nil, fmt.Errorf("Robot Agent 回复为空: %w", workflow.ErrRobotAgentDecisionInvalid)
	}
	for _, field := range []string{"python", "trajectory", "stage", "action_type"} {
		if _, exists := decision[field]; exists {
			return nil, fmt.Errorf("Robot Agent 回复包含禁止字段 %q: %w", field, workflow.ErrRobotAgentDecisionInvalid)
		}
	}
	return decision, nil
}

func (s *Service) withSkillInputContract(ctx context.Context, execution workflow.TaskExecution, prompt string) (string, error) {
	start := time.Now()
	defer func() {
		if s.logger != nil {
			s.logger.Info("agent.contract.prepare", "task_id", execution.Task.ID,
				"purpose", "skill_input", "duration_ns", time.Since(start).Nanoseconds())
		}
	}()
	if s.skillContracts == nil {
		return "", fmt.Errorf("Robot Skill contract provider is unavailable")
	}
	var spec struct {
		SkillName    string `json:"skill_name"`
		SkillVersion string `json:"skill_version"`
	}
	if err := json.Unmarshal(execution.SubTasks[0].Spec, &spec); err != nil {
		return "", err
	}
	declared, err := s.skillContracts.DescribeSkillInput(ctx, execution.Task.AssignedRobotID, spec.SkillName, spec.SkillVersion)
	if err != nil {
		return "", err
	}
	if _, err := contract.CompileObject(declared); err != nil {
		return "", err
	}
	raw, err := json.Marshal(map[string]any{"skill_name": spec.SkillName,
		"skill_version": spec.SkillVersion, "input_schema": declared})
	if err != nil {
		return "", err
	}
	return prompt + "\n当前已安装 Skill 的真实输入契约（只约束 robot.run 的 input，不是整个工具参数）：\n" + string(raw) +
		"\n按 required、枚举与嵌套定义生成完整 input；可选字段缺少可靠事实时省略，不得编造。契约不是授权；仍须遵守当前 SubTask、批准范围与实时状态。输出不合格时依据结构化错误修正本次调用，不重放已创建的 Execution。", nil
}

// RecoverTask 在 Robot Execution 已明确 failed 后启动一次只读决策 Run。
// 它返回“完整的剩余步骤”而不是数据库补丁，Workflow Service 才是唯一写入者；
// 这样模型无法修改已完成步骤，也不会借恢复之名重放 interrupted Action。
func (s *Service) RecoverTask(ctx context.Context,
	request workflow.TaskRecoveryRequest) (workflow.TaskRecoveryDecision, error) {
	catalog, err := s.robotTaskPlanningCatalog(request.Workflow, request.Task)
	if err != nil {
		return workflow.TaskRecoveryDecision{}, err
	}
	pending := make([]store.SubTask, 0)
	for _, item := range mustListSubTasks(s.st, request.Task.ID) {
		if item.Status == store.TaskStatusPending {
			pending = append(pending, item)
		}
	}
	input, err := json.Marshal(map[string]any{
		"workflow_goal":      request.Workflow.Goal,
		"approved_scope":     request.Workflow.ApprovedScope,
		"task_goal":          request.Task.Goal,
		"failed_subtask":     request.SubTask,
		"robot_execution":    request.Execution,
		"pending_subtasks":   pending,
		"robot_catalog":      catalog,
		"interaction_answer": request.Answer,
	})
	if err != nil {
		return workflow.TaskRecoveryDecision{}, err
	}
	prompt := buildTaskRecoveryPrompt(input)
	planningSkills, skillErr := s.taskPlanningSkills(request.Project.ID,
		request.Task.AssignedAgentID, catalog)
	if skillErr != nil {
		return workflow.TaskRecoveryDecision{}, skillErr
	}
	var decision workflow.TaskRecoveryDecision
	_, err = s.runTaskAgent(ctx, request.UserID, request.Project,
		request.Conversation, request.Workflow, "", &request.Task,
		request.Task.AssignedAgentID, prompt, request.Answer, planningSkills,
		store.RunKindTaskExecution, runtimePurposeTaskRecovery, func(text string) error {
			value, validationErr := decodeTaskRecoveryDecision(text, request.Task, catalog)
			if validationErr == nil {
				decision = value
			}
			return validationErr
		})
	if err != nil {
		return workflow.TaskRecoveryDecision{}, err
	}
	if decision.Decision == "" {
		return workflow.TaskRecoveryDecision{}, fmt.Errorf("Task recovery 未返回决策: %w", store.ErrInvalidState)
	}
	return decision, nil
}

func decodeTaskRecoveryDecision(text string, task store.Task, catalog robotTaskPlanningView) (workflow.TaskRecoveryDecision, error) {
	var output struct {
		Decision     string               `json:"decision"`
		Summary      string               `json:"summary"`
		Replacements []store.SubTaskDraft `json:"replacements"`
	}
	if err := decodeStrictJSON(text, &output); err != nil {
		return workflow.TaskRecoveryDecision{}, fmt.Errorf("Task recovery 返回非法结构: %w", err)
	}
	if strings.TrimSpace(output.Summary) == "" {
		return workflow.TaskRecoveryDecision{}, fmt.Errorf("Task recovery 缺少 summary: %w", store.ErrInvalidState)
	}
	switch output.Decision {
	case "revise_pending":
		if len(output.Replacements) == 0 {
			return workflow.TaskRecoveryDecision{}, fmt.Errorf("Task recovery 缺少替代步骤: %w", store.ErrInvalidState)
		}
		// 恢复仍是在生成未执行步骤，必须与首次规划使用同一机器契约；
		// 失败 Execution 的持物快照不能经 replacements 重新变成规划事实。
		encoded, marshalErr := json.Marshal(workflow.TaskPlanResult{Summary: output.Summary, SubTasks: output.Replacements})
		if marshalErr != nil {
			return workflow.TaskRecoveryDecision{}, marshalErr
		}
		// Recovery follows a failed Robot Skill, so replacements must satisfy the
		// Robot planning contract even if a caller omitted the role in its view.
		planTask := task
		planTask.RequiredRole = "robot"
		if _, validationErr := decodeAndValidateTaskPlanResult(string(encoded), planTask, catalog); validationErr != nil {
			return workflow.TaskRecoveryDecision{}, fmt.Errorf("Task recovery 替代步骤违反规划契约: %w", validationErr)
		}
	case "fail_task":
		if len(output.Replacements) != 0 {
			return workflow.TaskRecoveryDecision{}, fmt.Errorf("fail_task 不得携带替代步骤: %w", store.ErrInvalidState)
		}
	default:
		return workflow.TaskRecoveryDecision{}, fmt.Errorf("Task recovery decision 非法: %w", store.ErrInvalidState)
	}
	return workflow.TaskRecoveryDecision{Decision: output.Decision,
		Replacements: output.Replacements, Summary: output.Summary}, nil
}

func mustListSubTasks(st *store.Store, taskID string) []store.SubTask {
	items, _ := st.ListSubTasks(taskID)
	return items
}

type taskOutcome struct {
	Kind     string          `json:"kind"`
	Summary  string          `json:"summary,omitempty"`
	Evidence json.RawMessage `json:"evidence,omitempty"`
}

func interactionID(value *store.Interaction) string {
	if value == nil {
		return ""
	}
	return value.ID
}

func (s *Service) runTaskAgent(ctx context.Context, userID string, project store.Project,
	conversation store.ChatSession, wf store.Workflow, proposalID string, taskValue *store.Task,
	agentID, prompt string, answer *store.Interaction, planningSkills kernel.SkillStore,
	runKind, runtimePurpose string,
	reviewers ...func(string) error) (string, error) {
	if runKind != store.RunKindTaskPlanning && runKind != store.RunKindTaskExecution {
		return "", fmt.Errorf("Task Agent Run kind 非法 %q: %w", runKind, store.ErrInvalidState)
	}
	kind, contextID, taskID := runKind, "task:planning", ""
	if taskValue != nil {
		contextID, taskID = taskValue.ContextID, taskValue.ID
	}
	rt, err := s.buildPurposeRuntimeWithSkills(ctx, conversation.ID, agentID, kind,
		planningSkills, runtimePurpose)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	run := store.RunSession{ID: store.NewRunSessionID(), ProjectID: project.ID,
		ChatSessionID: conversation.ID, WorkflowID: wf.ID, TaskID: taskID,
		SourceInteractionID: interactionID(answer), Kind: kind,
		ContextID: contextID, AgentID: agentID, AgentName: rt.profile.Name,
		Provider: rt.modelResolution.Provider, Endpoint: rt.modelResolution.ResolvedEndpoint,
		Model: rt.modelResolution.ResolvedModel, TraceID: kernel.NewTraceID(),
		Status: store.RunStatusRunning, StartedAt: now, UpdatedAt: now}
	if err := s.st.CreateRunSession(run); err != nil {
		return "", err
	}
	release, runCtx, err := s.registerPurposeRun(ctx, run, rt)
	if err != nil {
		_, _ = s.st.FinishRunSession(run.ID, nil, store.RunStatusFailed, err.Error(), time.Now().UTC())
		return "", err
	}
	defer release()
	var taskExecution workflow.TaskExecution
	var taskMessageID string
	if taskValue != nil {
		execution := workflow.TaskExecution{Project: project, Conversation: conversation,
			Workflow: wf, Task: *taskValue}
		taskExecution = execution
		taskMessageID, err = s.appendTaskMessage(execution, run, agentID, schema.UserMessage(prompt))
		if err != nil {
			return "", err
		}
	}
	var messages []*schema.Message
	compactDecisionContext := isRobotDecisionPurpose(runtimePurpose) ||
		runtimePurpose == runtimePurposeTaskRecovery
	if taskValue != nil && !compactDecisionContext {
		messages, err = s.loadTaskContext(taskExecution)
		if err == nil {
			runCtx, err = s.withTaskSummaryPersistence(runCtx, taskExecution, taskMessageID)
		}
	} else if taskValue != nil {
		// checkpoint决策与失败恢复只消费调用方构造的当前Execution视图。它们
		// 仍写入同一个Task Context，但不重新加载历史robot.run参数和旧错误，
		// 避免一次局部判断误匹配成早先的SubTask执行请求。
		messages = []*schema.Message{schema.UserMessage(prompt)}
	} else {
		var history loadedContext
		history, err = s.loadContextHistory(conversation.ID, rt.supportsVision)
		messages = append(history.Messages, schema.UserMessage(prompt))
	}
	if err != nil {
		return "", err
	}
	text, runErr := "", error(nil)
	// Final-output validation and tool feedback share a bounded correction
	// budget. Identical error descriptions do not prove identical outputs.
	for exchange := 0; ; exchange++ {
		text, _, runErr = s.consumePurposeRun(runCtx, rt, run, messages)
		if taskValue != nil {
			if _, pendingErr := s.st.GetPendingInteractionByRunID(run.ID); pendingErr == nil {
				waiting, transitionErr := s.st.TransitionRunStatus(run.ID,
					[]string{store.RunStatusRunning}, store.RunStatusWaitingInput, time.Now().UTC())
				if transitionErr != nil {
					return "", transitionErr
				}
				s.publishRunState(waiting, rt, EventTypeRunWaitingInput)
				return "", workflow.ErrTaskWaitingInput
			} else if !errors.Is(pendingErr, store.ErrNotFound) {
				return "", pendingErr
			}
		}
		if runErr != nil || len(reviewers) == 0 || reviewers[0] == nil {
			break
		}
		checkStart := time.Now()
		problem := reviewers[0](text)
		purpose := runtimePurpose
		if purpose == "" {
			purpose = runKind
		}
		s.recordContractCheck(run, purpose, exchange+1, checkStart, problem)
		if problem == nil {
			break
		}
		if exchange+1 >= rt.profile.Limits.MaxTurns {
			runErr = fmt.Errorf("Task Planning 达到 Agent Profile max_turns 后仍未通过契约校验: %w", problem)
			break
		}
		if runErr = rt.contractCorrections.request(runCtx, problem); runErr != nil {
			break
		}
		correction := buildTaskPlanningCorrectionPrompt(problem)
		if isRobotDecisionPurpose(runtimePurpose) {
			correction = buildRobotAgentDecisionCorrectionPrompt(problem)
		} else if runtimePurpose == runtimePurposeTaskRecovery {
			correction = buildTaskRecoveryCorrectionPrompt(problem)
		}
		messages = append(messages,
			schema.AssistantMessage(text, nil),
			schema.UserMessage(correction))
	}
	status, errorText := store.RunStatusCompleted, ""
	if runErr != nil {
		status, errorText = store.RunStatusFailed, runErr.Error()
		if errors.Is(runErr, context.Canceled) {
			status = store.RunStatusCancelled
		}
	}
	finished, finishErr := s.st.FinishRunSession(run.ID, nil, status, errorText, time.Now().UTC())
	if finishErr == nil {
		s.publishRunState(finished, rt, mapRunEventType(status))
	}
	if runErr != nil || finishErr != nil {
		return "", errors.Join(runErr, finishErr)
	}
	if taskValue != nil {
		execution := workflow.TaskExecution{Project: project, Conversation: conversation,
			Workflow: wf, Task: *taskValue}
		if _, err := s.appendTaskMessage(execution, run, agentID, schema.AssistantMessage(text, nil)); err != nil {
			return "", err
		}
	}
	_ = userID // ownership was checked by Workflow Service before invoking this adapter.
	return text, nil
}

func isRobotDecisionPurpose(purpose string) bool {
	return purpose == runtimePurposeRobotDecision ||
		purpose == runtimePurposeRobotNavigationDecision
}

func (s *Service) buildPurposeRuntime(ctx context.Context, sessionID, agentID, runKind string,
	interactionModes ...string) (*sessionRuntime, error) {
	return s.buildPurposeRuntimeWithSkills(ctx, sessionID, agentID, runKind, nil,
		interactionModes...)
}

func (s *Service) buildPurposeRuntimeWithSkills(ctx context.Context, sessionID, agentID,
	runKind string, skillOverride kernel.SkillStore,
	interactionModes ...string) (*sessionRuntime, error) {
	prof, err := s.profileForAgent(agentID)
	if err != nil {
		return nil, err
	}
	if agentID != s.leaderPlanningAgentID() && prof.Mode != profile.ModeWorker {
		return nil, fmt.Errorf("Task Agent %q 不是 worker: %w", agentID, store.ErrInvalidState)
	}
	derived := *prof
	planning := runKind == store.RunKindTaskPlanning
	planConversation := len(interactionModes) > 0 && interactionModes[0] == interactionModePlan
	workflowSummary := len(interactionModes) > 0 && interactionModes[0] == runtimePurposeWorkflowSummary
	runtimePurpose := ""
	if len(interactionModes) > 0 {
		runtimePurpose = interactionModes[0]
	}
	robotDecision := isRobotDecisionPurpose(runtimePurpose)
	robotNavigationDecision := runtimePurpose == runtimePurposeRobotNavigationDecision
	taskRecovery := runtimePurpose == runtimePurposeTaskRecovery
	robotExecution := runKind == store.RunKindTaskExecution &&
		strings.HasPrefix(agentID, "robot:") && !robotDecision && !taskRecovery
	restricted := planning || planConversation || workflowSummary || robotDecision || taskRecovery ||
		robotExecution
	if restricted {
		derived.Tools.Namespaces = nil
		if planConversation {
			derived.Tools.Namespaces = []string{"system.*", "artifact.get", "artifact.list", "map.query",
				"plan.suggest", "interaction.ask"}
		} else if planning {
			derived.Tools.Namespaces = []string{"map.query", "interaction.ask"}
		} else if robotDecision {
			// 抓取、放置等物理checkpoint必须只消费Skill已经给出的当前事实，
			// 不能因为通用Prompt误导而重新查询整张Map。仅导航目标替换需要
			// Robot/Map只读工具；所有Decision仍禁止下发新的物理动作。
			derived.Tools.Namespaces = []string{"interaction.ask"}
			if robotNavigationDecision {
				derived.Tools.Namespaces = []string{"robot.get", "map.query", "interaction.ask"}
			}
		} else if taskRecovery {
			derived.Tools.Namespaces = []string{"robot.get", "map.query", "artifact.get",
				"artifact.list", "interaction.ask"}
		} else if robotExecution {
			derived.Tools.Namespaces = []string{"robot.get", "robot.run", "robot.stop",
				"map.query", "interaction.ask"}
		} else if workflowSummary {
			// Workflow 终态事实已经随 Prompt 注入。总结 Run 不应再次查询、规划或
			// 执行任何动作，否则一次展示性回复可能意外扩大为新的业务操作。
			derived.Tools.Namespaces = nil
		}
		derived.Tools.Pinned = nil
		derived.Tools.ToolSearch = false
		derived.Interrupt.ApprovalRequired = nil
		if planning {
			derived.Instruction += "\n\n当前是 Task Planning Run：依据 Task input、实际 Robot/Skill 目录进行拆解；可以按需加载 Project Agent Skill或当前 Robot Skill正文，也可在确实需要环境事实时调用 map.query。不得读取文件或Artifact，不得执行命令、运行Robot Skill、委派Agent、部署或操作Robot。仅当缺少会改变业务结果、授权或安全选择且无法从当前上下文或只读工具获得的用户决定时调用 interaction.ask，并说明具体缺失项；已批准的步骤无需再次询问是否开始，设备忙碌或离线只属于资源等待。最终回复必须是调用方指定的单个JSON对象。"
		} else if planConversation {
			derived.Instruction += `
当前对话由用户显式选择了 Plan Mode。你仍在原 Conversation 中与用户交流：先读取必要上下文、说明理解，只提出真正影响方案的澄清问题；需要结构化回答时调用 interaction.ask，回答会在新的 Leader Run 中自动带回，然后继续收敛。不得修改文件、执行命令、调用 Skill、委派 Agent、部署或操作 Robot。不要在需求仍不明确时调用 plan.suggest；当目标、约束和完成条件已经足够时，调用 plan.suggest，并在 tasks 中提交完整的 Leader Task TODO 与依赖。Task 只描述业务目标、角色、能力、资源范围和完成标准，不要生成 SubTask、Stage、Action 或 Ability。plan.suggest 成功即表示当前 revision 已 ready，可由计划卡完整展示；只需简短请用户在计划卡审阅、批准或继续对话修改，不要在回复中重复整份计划。批准由用户界面携带精确 revision 直接提交给 Workflow Service，Leader 不执行批准状态迁移，也不能声称仅凭一条自然语言消息已经开始 Workflow。用户要求调整时，应重新调用 plan.suggest 生成新 revision。constraints 与 completion_criteria 必须传数组或对象；map_binding 只有在用户要求核对地图且已有精确 map_id、generation 与 selections 时才传，没有有效选择时省略。绝不能用 true/false 充当结构化字段。若工具返回 BAD_ARGUMENTS，应按错误纠正参数，最多再调用一次。不要使用 artifact.list 推断 Workflow 或 Robot Execution 状态，领域进度由 Workflow/Robot 事件和运行视图展示。不要把 Markdown 文本冒充可执行 Workflow。`
			derived.Instruction += `
Plan Mode 的 Leader 本来就不会注入 robot.get、robot.run、robot.stop；计划确认后，Workflow 会把这些工具注入已绑定 Robot 的 Robot Worker。不要向用户报告当前 Leader 看不到 robot.*，也不要把它误报为系统缺少执行能力。用户给出的目标引用、位姿、generation 或 revision 可以作为 Robot Task 输入直接保留；只有用户明确要求核对地图，或计划本身必须选择当前地图实体时才调用 map.query。用户要求直接传值或不要查询地图后必须停止 map.query。`
			derived.Instruction += "\nTask required_role 只能使用 robot、developer、map、monitor；resource_requirements 只允许 robot_ids、robot_models、backends、workspace_write，并且只保存用户明确要求的资源限制。backends必须使用Robot实际目录中的实现标识；simulation只是环境类别，不是backend，不限制实现时直接省略。业务对象、目标与几何提示写入 Task input；查询得到的稳定业务引用应原样保留，entity_id只作可选规划来源。应用对象的选择、映射与完成判据依据当前Project知识、Agent Skill和实时查询结果，不从名称推测物理状态。"
		} else if robotDecision {
			derived.Instruction += "\n\n当前是Robot Skill安全checkpoint的局部决策Run。只根据当前请求中的reason、context、allowed_decisions和response_schema返回类型化JSON；不得调用robot.run/stop、修改Workflow或重新规划整个Task。抓取或放置checkpoint不是导航规划，不得自行改写底盘目标。确需业务或安全选择时可以询问用户。"
		} else if taskRecovery {
			derived.Instruction += "\n\n当前是已明确failed的Robot Execution恢复Run。可以读取当前Robot、地图和相关证据，确需业务或安全选择时可以询问用户；不得直接运行或停止Robot，不得修改已完成步骤或重放原Execution。最终只返回调用方要求的恢复JSON。"
		} else if robotExecution {
			derived.Instruction += "\n\n当前是Robot Task中一个SubTask的执行Run。只能读取当前Robot或必要地图参考、询问用户、运行或安全停止当前Robot Skill；不得读写工作区、委派SubAgent或操作其他Task。已获批准的步骤无需再次询问是否开始；仅当缺少会改变结果、授权或安全选择的用户决定时调用 interaction.ask，并说明具体缺失项。"
		} else {
			derived.Instruction += "\n\n当前 Run 只负责总结一个已经进入终态的 Workflow。必须以注入的结构化 Task/SubTask 结果和证据为准；不得调用工具、重新规划、声称未出现的结果，或启动任何新的执行。"
		}
	}
	entry, snapshot, err := s.resolveSessionModelEntry(sessionID, agentID, &derived)
	if err != nil {
		return nil, err
	}
	if robotExecution && hasCapability(entry.Capabilities, "reasoning_effort") {
		entry = withReasoningEffort(entry, robotExecutionReasoningEffort(snapshot.ReasoningEffort))
	}
	model, err := s.buildModel(ctx, entry, s.llmReg.APIKey(snapshot.EndpointID))
	if err != nil {
		return nil, err
	}
	model = kernel.WithSafeModelRetry(model)
	// Task Agent 可以在真正执行 Task 时向 Team 中的只读 Service Agent 发起一次
	// 短咨询；Planning Run 仍然禁止嵌套委派，避免把计划职责拆成不可恢复的隐式
	// 调用链。咨询工具和普通工具使用同一安全契约，但咨询结果没有 Workflow 写权。
	var subTools []kernel.Tool
	var subDefs []tool.Definition
	if !restricted && derived.Mode == profile.ModeWorker {
		subTools, subDefs, err = s.buildSubAgentTools(ctx, sessionID)
		if err != nil {
			return nil, err
		}
	}
	tools, dynamic, safety, err := s.buildSessionToolchain(sessionID, &derived, agentID, subDefs...)
	if err != nil {
		return nil, err
	}
	tools = append(tools, subTools...)
	if restricted && len(s.tools) > 0 && len(tools) >= len(s.tools) {
		// 测试逃生舱不属于生产 Profile 授权，Planning 运行器不能继承。
		tools = tools[len(s.tools):]
	}
	workspaceRoot := ""
	if !restricted {
		workspaceRoot, err = s.sessionWorkspaceRoot(sessionID)
		if err != nil {
			return nil, err
		}
	}
	var policy kernel.ToolCallGuard
	var effectiveSkills kernel.SkillStore
	if planning || planConversation {
		policy = planningToolPolicy{}
		if planConversation {
			policy = conversationPlanToolPolicy{}
		}
		effectiveSkills, err = s.sessionSkillReader(sessionID, &derived)
		if err != nil {
			return nil, err
		}
	} else if robotDecision {
		allowed := map[string]struct{}{planningSkillTool: {}, "interaction.ask": {}}
		if robotNavigationDecision {
			allowed["robot.get"] = struct{}{}
			allowed["map.query"] = struct{}{}
		}
		policy = taskDecisionToolPolicy{allowed: allowed}
		effectiveSkills, err = s.sessionSkillReader(sessionID, &derived)
		if err != nil {
			return nil, err
		}
	} else if taskRecovery {
		policy = taskDecisionToolPolicy{allowed: map[string]struct{}{
			planningSkillTool: {}, "robot.get": {}, "map.query": {},
			"artifact.get": {}, "artifact.list": {}, "interaction.ask": {},
		}}
		effectiveSkills, err = s.sessionSkillReader(sessionID, &derived)
		if err != nil {
			return nil, err
		}
	} else if robotExecution {
		// Robot Task Planning 已读取 Project Agent Skill，并把拆解结果持久化为
		// 当前 SubTask；执行 Run 的 Prompt 又会直接注入 current_skill_contract
		// 全文和针对该步骤的 execution_guidance。此时继续暴露通用 skill 工具
		// 不再提供新事实，反而会让模型把角色名 robot 误当成 Skill 名，导致
		// 尚未调用 robot.run 就因“技能不存在”终止。执行面只保留 robot/map/
		// interaction 工具；Planning 与局部决策仍按原规则使用 Skill middleware。
		effectiveSkills = nil
	} else if !workflowSummary {
		effectiveSkills, err = s.sessionSkillReader(sessionID, &derived)
		if err != nil {
			return nil, err
		}
	}
	if (planning || robotDecision || taskRecovery) && skillOverride != nil {
		effectiveSkills = skillOverride
	}
	corrections := newContractCorrectionBudget(derived.Limits.MaxTurns)
	if policy != nil {
		policy = &contractToolPolicy{next: policy, budget: corrections}
	}
	runner, err := kernel.BuildAgent(ctx, kernel.AgentConfig{
		Name: derived.Name, Role: string(derived.Mode), Description: derived.Description,
		Instruction: derived.Instruction, SafetyDoc: derived.SafetyDoc,
		Model: model, ModelName: entry.Model, Tools: tools,
		ToolSearch: derived.Tools.ToolSearch, DynamicTools: dynamic,
		Safety: safety, ToolPolicy: policy,
		ReturnDirectly: func() map[string]bool {
			if robotExecution {
				return map[string]bool{"robot.run": true}
			}
			return nil
		}(),
		MaxTurns:      derived.Limits.MaxTurns,
		ContextTokens: derived.Limits.ContextTokens, Store: s.st,
		SkillStore:    effectiveSkills,
		WorkspaceRoot: workspaceRoot, Price: entry.Price, Purpose: runKind, Logger: s.logger,
	})
	if err != nil {
		return nil, err
	}
	return &sessionRuntime{runner: runner, profile: &derived, contractCorrections: corrections,
		supportsVision: hasCapability(entry.Capabilities, "image"), modelResolution: ModelResolution{
			RequestedEndpoint: derived.Model, ResolvedEndpoint: snapshot.EndpointID,
			ResolvedModel: entry.Model, Provider: entry.Service, Source: snapshot.Source,
			DefaultInherited: snapshot.Source == store.ModelSourceSystemDefault,
		}}, nil
}

func (s *Service) leaderPlanningAgentID() string {
	if id := s.leaderMemberID(); id != "" {
		return id
	}
	return agentRoleLeader
}

func (s *Service) preparePurposeRun(run store.RunSession, rt *sessionRuntime) (store.RunSession, error) {
	traceID := run.TraceID
	if traceID == "" {
		traceID = kernel.NewTraceID()
	}
	return s.st.UpdateRunExecutionMetadata(run.ID, rt.modelResolution.Provider,
		rt.modelResolution.ResolvedEndpoint, rt.modelResolution.ResolvedModel, traceID, time.Now().UTC())
}

func (s *Service) registerPurposeRun(parent context.Context, run store.RunSession,
	rt *sessionRuntime) (func(), context.Context, error) {
	if run.ContextID == "" {
		return nil, nil, store.ErrInvalidState
	}
	runCtx, cancel := context.WithCancel(parent)
	project, err := s.st.GetProject(run.ProjectID)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	policy, _ := s.st.GetSessionExecutionPolicy(run.ChatSessionID)
	robotID, subtaskID := "", ""
	publicRobotProgress := false
	if run.TaskID != "" {
		if taskValue, taskErr := s.st.GetTask(run.TaskID); taskErr == nil {
			publicRobotProgress = taskValue.RequiredRole == "robot"
			robotID = taskValue.AssignedRobotID
			// 旧 Workflow 在 v24 之前把 robot_id 放在 input。仅为原地升级
			// 保留该读取；新计划的设备归属只能来自后绑定字段。
			if robotID == "" {
				var input map[string]any
				if json.Unmarshal(taskValue.Input, &input) == nil {
					robotID, _ = input["robot_id"].(string)
				}
			}
		}
		if subTasks, subErr := s.st.ListSubTasks(run.TaskID); subErr == nil {
			for _, subTask := range subTasks {
				if subTask.Status == store.TaskStatusRunning {
					subtaskID = subTask.ID
					break
				}
			}
		}
	}
	runCtx = tool.WithExecutionScope(runCtx, tool.ExecutionScope{RunKind: run.Kind,
		RunID: run.ID, AgentID: run.AgentID,
		WorkflowID: run.WorkflowID, TaskID: run.TaskID, SubtaskID: subtaskID, RobotID: robotID,
		SessionID: run.ChatSessionID, ProjectID: run.ProjectID, OwnerID: project.OwnerID,
		WorkspaceRoot: project.WorkspaceRoot, SkillsRoot: s.skillRoot(),
		ExecutionMode: policy.Mode, HostExecutionEnabled: policy.HostExecutionEnabled,
		HostExecutionAllowed: s.hostExecutionAllowed()})
	done := make(chan struct{})
	s.mu.Lock()
	if _, exists := s.activeRuns[run.ContextID]; exists {
		s.mu.Unlock()
		cancel()
		return nil, nil, fmt.Errorf("Context 已有活动 Run: %w", store.ErrInvalidState)
	}
	s.activeRuns[run.ContextID] = activeRun{userID: project.OwnerID, projectID: project.ID,
		sessionID: run.ChatSessionID, contextID: run.ContextID,
		runID: run.ID, runtime: rt, cancel: cancel, done: done}
	s.mu.Unlock()
	s.publishRunState(run, rt, EventTypeRunStarted)
	var progress *purposeProgress
	if publicRobotProgress {
		progress = &purposeProgress{metadata: RunMetadata{ReasoningVisibility: rt.profile.ReasoningVisibility}}
		runCtx = context.WithValue(runCtx, purposeProgressKey{}, progress)
	}
	return func() {
		s.finishPurposeProgress(rt, run, progress)
		cancel()
		close(done)
		s.mu.Lock()
		if active, ok := s.activeRuns[run.ContextID]; ok && active.runID == run.ID {
			delete(s.activeRuns, run.ContextID)
		}
		s.mu.Unlock()
	}, runCtx, nil
}

func (s *Service) consumePurposeRun(ctx context.Context, rt *sessionRuntime, run store.RunSession,
	messages []*schema.Message) (string, *purposeToolFailure, error) {
	start := time.Now()
	defer func() {
		if s.logger != nil {
			s.logger.Info("agent.contract.exchange", "run_id", run.ID, "task_id", run.TaskID,
				"kind", run.Kind, "duration_ns", time.Since(start).Nanoseconds(),
				"includes_tools", true)
		}
	}()
	stream, err := rt.runner.RunMessages(ctx, messages,
		kernel.RunOptions{CheckpointID: run.ID, TraceID: run.TraceID})
	if err != nil {
		return "", nil, err
	}
	var text strings.Builder
	var lastToolFailure *purposeToolFailure
	for stream != nil {
		event, ok := stream.Next()
		if !ok {
			return "", lastToolFailure, errors.New("Agent 事件流在终态前关闭")
		}
		s.publishPurposeProgress(ctx, rt, run, event)
		switch event.Kind {
		case kernel.EventTextDelta:
			text.WriteString(event.Text)
		case kernel.EventToolResult:
			if failure := parsePurposeToolFailure(event.ToolName, event.Text); failure != nil {
				lastToolFailure = failure
			}
		case kernel.EventInterrupted:
			stream, run, err = s.awaitApproval(ctx, rt, run, event.Interrupts)
			if err != nil {
				return "", lastToolFailure, err
			}
		case kernel.EventError:
			return "", lastToolFailure, event.Err
		case kernel.EventDone:
			return strings.TrimSpace(text.String()), lastToolFailure, nil
		}
	}
	return "", lastToolFailure, errors.New("Agent 事件流未返回终态")
}

func (s *Service) loadTaskContext(execution workflow.TaskExecution) ([]*schema.Message, error) {
	messages, after, err := s.loadTaskContextPrelude(execution)
	if err != nil {
		return nil, err
	}
	records, err := s.st.ListContextMessagesAfter(execution.Task.ContextID, after, 0)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.Message != nil {
			messages = append(messages, record.Message)
		}
	}
	return messages, nil
}

func (s *Service) loadRobotTaskExecutionContext(
	execution workflow.TaskExecution,
) ([]*schema.Message, error) {
	messages, _, err := s.loadTaskContextPrelude(execution)
	if err != nil {
		return nil, err
	}
	// Robot Task的每个SubTask都会临时注入当前一份完整Skill契约。历史Run仍
	// 保存在同一个Task Context供Trace/Inspector查看，但不能再次装入模型；
	// completed_subtasks、实时Robot状态和Interaction回答已由本轮结构化Prompt
	// 提供，重复加载旧robot.run参数只会制造过期事实并按步骤累积Token。
	return messages, nil
}

func (s *Service) loadTaskContextPrelude(
	execution workflow.TaskExecution,
) ([]*schema.Message, string, error) {
	messages := make([]*schema.Message, 0)
	memory, err := s.st.GetProjectMemory(execution.Project.ID)
	if err != nil {
		return nil, "", err
	}
	if content := strings.TrimSpace(memory.Content); content != "" {
		messages = append(messages, schema.SystemMessage("Project Memory（用户维护）：\n\n"+content))
	}
	summary, err := s.st.GetContextSummary(execution.Task.ContextID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, "", err
	}
	after := ""
	if err == nil {
		after = summary.CoveredThroughMessageID
		if content := strings.TrimSpace(summary.Summary); content != "" {
			messages = append(messages, schema.SystemMessage("Task 历史摘要（仅用于解释过去过程；当前状态以本轮结构化状态和实时资源为准）：\n\n"+content))
		}
	}
	return messages, after, nil
}

func (s *Service) appendTaskMessage(execution workflow.TaskExecution, run store.RunSession,
	agentID string, message *schema.Message) (string, error) {
	id := store.NewChatMessageID()
	err := s.st.AppendContextMessage(store.ContextMessage{ID: id, ContextID: execution.Task.ContextID,
		ProjectID: execution.Project.ID, SessionID: execution.Conversation.ID,
		TaskID: execution.Task.ID, AgentID: agentID, RunID: run.ID, TraceID: run.TraceID,
		Message: message, ArtifactRefs: []string{}, CreatedAt: time.Now().UTC()})
	return id, err
}

func (s *Service) withTaskSummaryPersistence(ctx context.Context, execution workflow.TaskExecution,
	coveredThrough string) (context.Context, error) {
	revision := int64(0)
	summary, err := s.st.GetContextSummary(execution.Task.ContextID)
	if err == nil {
		revision = summary.Revision
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return kernel.WithSummaryPersistence(ctx, kernel.SummaryPersistence{
		ContextID: execution.Task.ContextID, ProjectID: execution.Project.ID,
		SessionID: execution.Conversation.ID, TaskID: execution.Task.ID,
		CoveredThroughMessageID: coveredThrough, ExpectedRevision: revision,
	}), nil
}

func (s *Service) taskExecutionPrompt(execution workflow.TaskExecution) (string, error) {
	if len(execution.SubTasks) != 1 {
		return "", fmt.Errorf("Task Agent 每次必须只推进一个 SubTask: %w", store.ErrInvalidState)
	}
	view, err := s.st.GetWorkflowView(execution.Workflow.ID)
	if err != nil {
		return "", err
	}
	dependencyResults := make([]map[string]any, 0)
	for _, dependency := range view.Dependencies {
		if dependency.TaskID != execution.Task.ID {
			continue
		}
		for _, task := range view.Tasks {
			if task.ID == dependency.DependsOnTaskID {
				dependencyResults = append(dependencyResults, map[string]any{
					"task_id": task.ID, "goal": task.Goal, "status": task.Status,
					"result": task.ResultSummary, "evidence": task.Evidence,
				})
				break
			}
		}
	}
	completedSubTasks := make([]map[string]any, 0)
	for _, subTask := range view.SubTasks {
		if subTask.TaskID != execution.Task.ID || subTask.ID == execution.SubTasks[0].ID ||
			subTask.Status != store.TaskStatusCompleted {
			continue
		}
		var spec struct {
			SkillName    string `json:"skill_name"`
			SkillVersion string `json:"skill_version"`
		}
		_ = json.Unmarshal(subTask.Spec, &spec)
		// 完整 Robot Skill 结果仍保存在 Store，供 Inspector、Leader 总结和审计读取；
		// 下一项 Skill 会重新读取当前物理状态，因此 Task Agent 只需要知道前序步骤
		// 已完成及其证据，不能继续承担 HeldObjectState 等大对象的字段搬运。
		completedSubTasks = append(completedSubTasks, map[string]any{
			"subtask_id": subTask.ID, "skill_name": spec.SkillName,
			"skill_version": spec.SkillVersion, "status": subTask.Status,
			"execution_id":   subTask.ExecutionRef,
			"result_summary": subTask.Goal + "已完成", "evidence_refs": subTask.Evidence,
		})
	}
	var currentSkillContract *robotTaskSkillView
	if execution.Task.RequiredRole == "robot" {
		var spec struct {
			SkillName    string         `json:"skill_name"`
			SkillVersion string         `json:"skill_version"`
			Intent       map[string]any `json:"intent"`
		}
		if raw := strings.TrimSpace(string(execution.SubTasks[0].Spec)); raw != "" && raw != "null" {
			if err := json.Unmarshal(execution.SubTasks[0].Spec, &spec); err != nil {
				return "", fmt.Errorf("解析当前 Robot SubTask: %w", err)
			}
		}
		if spec.SkillName != "" {
			catalog, catalogErr := s.robotTaskPlanningCatalog(execution.Workflow, execution.Task)
			if catalogErr != nil {
				return "", catalogErr
			}
			for index := range catalog.Skills {
				item := &catalog.Skills[index]
				if item.Name == spec.SkillName && (spec.SkillVersion == "" || item.Version == spec.SkillVersion) {
					pkg, getErr := s.st.GetRobotSkillPackage(item.Name, item.Version)
					if getErr != nil {
						return "", getErr
					}
					detail, loadErr := robot.LoadSkillPackageDetail(pkg)
					if loadErr != nil {
						return "", loadErr
					}
					item.Documentation = detail.Body
					currentSkillContract = item
					break
				}
			}
			if currentSkillContract == nil {
				return "", fmt.Errorf("当前 Robot 未安装或未启用 Skill %s@%s: %w",
					spec.SkillName, spec.SkillVersion, store.ErrInvalidState)
			}
		}
	}
	resourceSnapshot := map[string]any{}
	if execution.Task.AssignedRobotID != "" {
		if pilot, getErr := s.st.GetActiveRobotPilot(execution.Task.AssignedRobotID); getErr == nil {
			resourceSnapshot = map[string]any{
				"robot_id": pilot.RobotID, "robot_status": pilot.RobotStatus,
				"pilot_status": pilot.Status, "ability_framework_status": pilot.AbilityFrameworkStatus,
				"current_execution_id":         pilot.CurrentExecutionID,
				"base_footprint_radius_m":      robotBaseFootprintRadius(pilot.Configuration),
				"navigation_resolution_m":      robotNavigationResolution(pilot.Configuration),
				"manipulation_work_distance_m": robotManipulationWorkDistance(pilot.Configuration),
			}
		}
	}
	executionGuidance, err := s.taskExecutionGuidance(execution.Project.ID, execution.Task.RequiredRole)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(map[string]any{
		"workflow": map[string]any{
			"id": execution.Workflow.ID, "goal": execution.Workflow.Goal,
			"status": execution.Workflow.Status, "approved_scope": execution.Workflow.ApprovedScope,
		},
		"task": map[string]any{
			"required_role":     execution.Task.RequiredRole,
			"assigned_agent_id": execution.Task.AssignedAgentID,
			"assigned_robot_id": execution.Task.AssignedRobotID,
			"goal":              execution.Task.Goal, "input": execution.Task.Input,
			"completion_criteria": execution.Task.CompletionCriteria,
		},
		"dependency_results":     dependencyResults,
		"completed_subtasks":     completedSubTasks,
		"current_subtask":        execution.SubTasks[0],
		"execution_guidance":     executionGuidance,
		"current_skill_contract": currentSkillContract,
		"live_resource_snapshot": resourceSnapshot,
		"interaction_answer":     execution.Answer,
	})
	if err != nil {
		return "", err
	}
	return buildTaskExecutionPrompt(execution.Task.RequiredRole, data), nil
}

func decodeStrictJSON(text string, target any) error {
	text = strings.TrimSpace(text)
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return describeJSONError(text, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON 后存在额外内容")
		}
		return describeJSONError(text, err)
	}
	return nil
}

// decodeWorkflowDraft 仍拒绝 Markdown、额外文字和未知字段，但兼容模型在
// 根节点额外套一层 {"workflow": ...} 的常见输出。两种都是明确的类型，
// 不做自由文本或 Markdown 猜测。
func decodeWorkflowDraft(text string) (store.WorkflowDraft, error) {
	var direct store.WorkflowDraft
	if err := decodeStrictJSON(text, &direct); err == nil {
		return direct, nil
	}
	var wrapped struct {
		Workflow store.WorkflowDraft `json:"workflow"`
	}
	if err := decodeStrictJSON(text, &wrapped); err != nil {
		return store.WorkflowDraft{}, err
	}
	return wrapped.Workflow, nil
}

func mapRunEventType(status string) string {
	switch status {
	case store.RunStatusCompleted:
		return EventTypeRunCompleted
	case store.RunStatusCancelled:
		return EventTypeRunCancelled
	default:
		return EventTypeRunFailed
	}
}

var _ workflow.Planner = (*Service)(nil)
var _ workflow.TaskExecutor = (*Service)(nil)
