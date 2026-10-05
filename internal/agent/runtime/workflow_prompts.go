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
	"encoding/json"
	"strings"
)

// Task Planning 只生成当前 Task 的可执行步骤。这里把稳定指令与运行数据分开，
// 既避免不同调用点复制协议，也让模型缓存能够复用前半段稳定内容。
const taskPlanningInstruction = `请为这个 Worker Task 生成可独立推进的类型化 SubTask。只返回 JSON：{"summary":string,"subtasks":[{"id":string,"kind":"robot_skill|agent_step","goal":string,"spec":object,"completion_criteria":object,"depends_on":[string]}]}，不要 Markdown、解释或代码围栏。summary 用一两句话说明拆解结果，供主 Conversation 展示，不得包含模型思考过程。所有 object 都必须是由具名 key/value 组成的合法 JSON，不能在 object 中放置匿名说明文字。depends_on 用于表达真实执行顺序；本版本 Task 内按依赖顺序串行执行 SubTask。Robot Task 必须按业务步骤拆分，且只能生成 robot_skill SubTask；每个 spec 的精确形状只能是 {"skill_name":string,"skill_version":string,"intent":object}，不得增加 input、constraints、validation、Stage、Action 或 Ability UUID。intent 只表达稳定业务意图，不要求满足 Robot Skill 的 Pydantic 输入模型；不得把Task或Map中的pose_hint复制进intent，规划位姿保留在Task input即可；精确位姿、当前持物和实时状态由后续 Task Execution Run 在执行当前步骤时确定。intent 不得出现 carrying_object、held_object 或 holding_object，包括布尔值和 null；空载或携物要求写入 completion_criteria，由 Skill 执行时读取实时状态。carried_object_ref 是业务身份，可以保留。业务完成要求写入 SubTask 外层的 completion_criteria。不得把前序 Robot Skill 的完整结果设计成下一项 Skill 的输入，也不得生成 $ref、subtask:... 等伪引用。只能选择统一执行目录中实际安装并启用的 Robot Skill；常驻目录是摘要，确实需要理解某个 Skill 时用 skill 工具加载其正文。非 Robot Task 只能生成 agent_step SubTask。`

func buildTaskPlanningPrompt(input []byte, robotCatalog string) string {
	return taskPlanningInstruction + planningHoldingBoundaryInstruction + "\n当前 Task：" + string(input) + robotCatalog
}

// 规划意图与实时持物输入明确分离；不影响外层业务完成目标的表达。
const planningHoldingBoundaryInstruction = ` spec.intent 的任何层级不得出现 carrying_object、held_object 或 holding_object，即使值为 false、null 或布尔预测也不允许。搬运阶段只用 carried_object_ref 表达稳定对象身份；不携物或持物成功的目标要求可写在外层 completion_criteria，真实状态由执行期观测。`

func buildTaskPlanningCorrectionPrompt(problem error) string {
	return `上一份 Task Planning 输出没有通过机器契约校验。不要解释，也不要沿用其中的非法结构；请依据同一 Task 上下文重新输出包含 summary 与 subtasks 的完整 JSON。Robot SubTask 的 spec 仍只能包含 skill_name、skill_version、intent 三个字段；intent 只描述稳定业务意图，不得包含 carrying_object、held_object 或 holding_object。本 Run 的纠正次数有上限；即使错误类型相同，也应依据下面的位置或字段诊断修正完整输出。不要重做已成功的工具查询，不得执行或重放机器人动作。校验错误：` + problem.Error()
}

func buildRobotExecutionCorrectionPrompt() string {
	return `当前SubTask仍没有持久Robot Execution。上一条回复可能没有调用robot.run，也可能已经调用但被执行前输入预检拒绝；先读取紧邻的结构化工具错误。若是Pydantic字段错误，必须依据current_skill_contract修正完整input，不得原参数重放；spec.intent是业务规划提示，字段名和值都不能直接冒充Skill输入枚举。Task或Map中的position:{x,y,z}也只是规划提示，不能原样当作Skill PoseRef，抓取或放置的可选pose_hint不符合契约时应省略，由Skill通过Ability实时观测。不得编造execution_id或accepted状态。修正后只调用一次robot.run；确实缺少会改变业务结果、授权或安全选择的用户输入时才调用interaction.ask。`
}

// Native tool calls are a transport contract, not a JSON reply schema. Never
// parse or execute call-shaped assistant text; ask the model to use its bound
// tool channel and keep the existing approval/validation/idempotency gates.
const robotToolTransportInstruction = `必须通过本次请求提供的原生工具调用通道发起 robot.run（模型侧名称以绑定工具为准），不能只在普通回复中写调用说明、Markdown、{"kind":"call","calls":[...]}或工具调用JSON文本。参数正确但没有真实工具调用仍属未完成；不要伪造accepted或execution_id。`

func buildRobotExecutionFeedback(currentFailure *purposeToolFailure) string {
	prompt := buildRobotExecutionCorrectionPrompt() + "\n" + robotToolTransportInstruction
	if currentFailure != nil {
		failure, _ := json.Marshal(currentFailure)
		return prompt + "\n本次未建立 Execution 的工具错误：" + string(failure)
	}
	return prompt + "\n本轮校验：ROBOT_TOOL_CALL_REQUIRED。本轮未建立真实Execution，且没有本轮工具拒绝记录。不要把历史工具错误当作本轮仍未修正的字段错误；根据当前契约检查参数，并实际发起一次原生工具调用，而不是再次返回调用描述。"
}

const robotAgentRequestInstruction = `Robot Skill 已在安全 checkpoint 请求一次局部决策。先识别请求中的 skill_name、reason、context 和 response_schema，再严格按 response_schema 返回一个 JSON 对象。response_schema 由 Skill 的 Pydantic 回复模型生成，不是让你设计新协议；字段可能是 decision、choice 或 action，只能使用当前 schema 实际声明的字段和值。若 schema 包含 expected_plan_revision，必须原样复制 context.plan_revision 的当前整数，绝不能自行递增、预测或改写。若 context.allowed_decisions 存在，只能从其中选择；若 context.allowed_actions 存在，action 只能从其中选择，即使 response_schema 的枚举允许更多值，也不得超出当前状态允许的集合。不要返回Markdown、解释、Python、轨迹、内部Stage跳转或schema未声明的字段。不要把抓取、放置或姿态恢复问题假设成导航问题；response_schema不包含替换目标位姿时不得查询Map或重新计算底盘工位。当前请求事实足以作出类型化选择时立即返回，不要为了形式完整调用工具。`

const robotNavigationDecisionInstruction = `只有当前请求和response_schema明确允许替换目标时，才依据当前Skill契约补充缺失的目标信息。一次Run中revision未变化的相同查询结果必须复用；取得必要事实后立即返回类型化决策，不扩大请求范围。`

func buildRobotAgentRequestPrompt(input []byte, navigation bool) string {
	instruction := robotAgentRequestInstruction
	if navigation {
		instruction += "\n" + robotNavigationDecisionInstruction
	}
	return instruction + "\n请求：" + string(input)
}

func buildRobotAgentDecisionCorrectionPrompt(problem error) string {
	return `上一份 Robot Decision 回复没有通过类型化契约校验。不要解释，不要使用 Markdown 或代码围栏；请依据同一请求中的 response_schema 重新返回一个完整 JSON 对象。不要重做已经成功的工具查询；本 Run 的纠正次数有上限，即使错误类型相同也应修正下面指出的问题，不得执行或重放机器人动作。校验错误：` + problem.Error()
}

const taskRecoveryInstruction = `Robot Skill 已明确失败且物理执行已经终止。请判断本 Task 是否能在批准范围内恢复。只返回 JSON：{"decision":"revise_pending|fail_task","summary":string,"replacements":[{"id":string,"kind":"robot_skill|agent_step","goal":string,"spec":object,"completion_criteria":object,"depends_on":[string]}]}。revise_pending 必须返回一份完整的剩余步骤，替代所有尚未开始的步骤；只能选择 robot_catalog 中已安装且启用的 Skill，并使用精确版本。Robot Skill 的 spec 必须且只能是 {"skill_name":string,"skill_version":string,"intent":object}；intent 只说明本次恢复改变的稳定业务意图和依据，不得写入 request_key、Map generation、最终底盘Pose、完整 robot.run input，也不得写入 carrying_object、held_object 或 holding_object。精确位姿和执行参数由下一次 Task Execution 结合当前Map与Robot状态计算。不得返回 Stage、Action、Ability UUID，不得重放相同 request_key，不得修改已完成步骤。无法在批准范围内安全恢复时返回 fail_task 和空 replacements。`

func buildTaskRecoveryCorrectionPrompt(problem error) string {
	return `上一份 Task recovery 输出没有通过机器契约校验。不要解释，也不要沿用其中的非法结构；请依据同一恢复上下文重新输出包含 decision、summary 与 replacements 的完整 JSON。Robot Skill 的 spec 仍只能包含 skill_name、skill_version、intent；intent 的任何层级不得包含 carrying_object、held_object 或 holding_object。本 Run 的纠正次数有上限，即使错误类型相同也应依据字段或位置诊断修正。不得执行动作、改写已完成步骤、重做已成功的工具查询或扩大批准范围。校验错误：` + problem.Error()
}

func buildTaskRecoveryPrompt(input []byte) string {
	return taskRecoveryInstruction + planningHoldingBoundaryInstruction + "\n恢复上下文：" + string(input)
}

var taskRoleInstructions = map[string]string{
	"robot":     `你是当前 Robot Task 的执行责任人，只推进 current_subtask。依据Task input、spec.intent、execution_guidance、current_skill_contract和live_resource_snapshot组装完整robot.run.input。资源快照可用且输入充分时直接调用robot.run；只有当前Skill确实缺少实时Robot事实，或快照stale/unknown、Scene/Layout变化、状态冲突时才调用robot.get。缺少当前契约要求的地图信息时才按需调用map.query；同一Run中参数和revision未变化的查询必须复用。地图信息不能替代Skill要求的执行期物理验证。完整输入必须符合当前Skill契约，业务身份来自已批准Task，不复制前序Skill完整结果或构造伪引用。无法由工具解决且会改变业务结果、授权或安全选择时才调用interaction.ask；设备忙碌或离线属于资源等待。robot.run返回accepted后立即结束本Run，不轮询，也不得再次调用robot.run。不得直接调用Ability、Robot SDK或改写其他SubTask。`,
	"map":       `你是 Map Task 的结果责任人。只推进 current_subtask，保存明确的地图实体、generation/revision 与证据，不修改其他 Task。`,
	"monitor":   `你是 Monitor Task 的独立验证责任人。只根据当前 Observation/Artifact 验证 completion_criteria，不替代被验证 Agent 执行动作。`,
	"developer": `你是 Developer Task 的结果责任人。只推进 current_subtask，并把代码、测试或文档的真实结果与证据写入结果。`,
}

// taskExecutionGuidance 读取当前角色在 Project 中已获准的应用知识。
// 具体对象、几何和 Skill 用法由 Agent Skill 与实际安装契约提供。
func (s *Service) taskExecutionGuidance(projectID, role string) (string, error) {
	if s.skills == nil {
		return "", nil
	}
	prof, err := s.profiles.Load(role)
	if err != nil {
		return "", err
	}
	reader, err := s.projectSkillReader(projectID, prof)
	if err != nil || reader == nil {
		return "", err
	}
	var parts []string
	for _, item := range reader.List() {
		parts = append(parts, "# Agent Skill: "+item.Name+"\n"+item.Body)
	}
	return strings.Join(parts, "\n\n"), nil
}

const taskOutcomeInstruction = `可以使用当前 Agent Profile 授权的工具，但只能在当前 Project 与批准范围内操作。完成本次 Agent 决策时只返回 {"kind":"result","summary":string,"evidence":object|array}，不要 Markdown 或代码围栏。确实缺少会影响结果的用户输入时必须调用 interaction.ask；工具成功创建问题后停止本轮，不要再执行工具，也不要返回另一套 interaction JSON。用户回答后系统会在同一 Task Context 中启动新 Run。不要猜测缺失输入。`

func buildTaskExecutionPrompt(role string, data []byte) string {
	instruction := taskRoleInstructions[role]
	if instruction == "" {
		instruction = `你是当前 Task 的结果责任人，只推进 current_subtask。`
	}
	return instruction + " " + taskOutcomeInstruction + "\nTask：" + string(data)
}
