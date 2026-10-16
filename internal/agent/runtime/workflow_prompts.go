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
	return taskPlanningInstruction + planningHoldingBoundaryInstruction + taskDependencyInstruction + "\n当前 Task：" + string(input) + robotCatalog
}

// Prerequisite data is read through the existing execution-result tool.
const taskDependencyInstruction = `每个 robot_skill SubTask 对应一次 robot.run 调用；同一 Skill 包含多个公开 task 时，每个 task 仍是独立调用，不能误当作一次调用内部的 Stage。使用简短 goal/intent 避免重复，但不能为缩短 JSON 省略必要调用。除非 Skill 本身提供组合入口，不能把多个 task 调用的完成条件合在一个 SubTask。优先使用已安装 Skill 明确提供的初始化、规划并执行等组合入口完成相应业务目标；纯 plan 的完成目标只能是得到计划，执行到位必须使用执行入口或另列执行步骤。优先复用已读取的 Project Agent Skill、execution_guidance 和当前 Skill 契约中已有的流程；这些内容足够时直接规划，不要求再加载整套 Robot Skill 或链接文档。仅在某个分支、字段或版本差异尚未覆盖时，通过 skill 或 robot.get 的 skill_name/resource_path 读取对应正文或参考文档，不能把未读取且未被现有经验覆盖的内容当成事实。按输入来源安排查询、规划和执行依赖：产出执行目标或规划包的查询/规划步骤必须在消费它的步骤之前，不可把前置查询排在运动之后，也不能用 intent 中的“需要先查询”代替实际前置步骤。前序结果通过 completed_subtasks 的 execution_id 使用 robot.get 按需读取准确字段；不得猜测数值、版本或伪造结果。completion=planned/queried/submitted 不证明目标物理动作完成；前序 goal 是意图，真实效果以 completion 和原结果为准。模型只选取当前契约所需的字段，不搬运整份前序结果或把旧持物状态当作实时事实。`

const robotResultLookupInstruction = `若当前步骤或前序结果标识在上下文压缩后不明确，先调用 robot.get({})，从 current_subtask.spec 恢复准确 Skill/版本/intent，从 completed_subtasks 恢复结果查询引用；不要改用目录里另一个 Skill。completed_subtasks 已提供前序 execution_id；当前契约或已加载经验已确定字段路径且仅需原样交接时，直接使用 input_bindings，无须先读取原结果。只有分支判断、字段路径未知或状态冲突时，才按需使用 result_lookup 和 result_paths 读取 execution.result 下的小字段；工具返回 result_values 保留原值，不读整份 plan、sessions、图像或轨迹再做摘要。把前序结果的字段交给当前 Skill 时，使用 robot.run.input_bindings 的 {input_path,execution_id,result_path} 原样绑定，例如 /parameters/packet ← /payload/result/packet。绑定路径相对本次 input 与原 execution.result；不在 input 中重复填写该字段。Atlas 网格、目标锁、轨迹包、digest 和实测关节应使用绑定，不能自行补位、缩写、重新编码或逐字重写。绑定是执行期数据传递，不是 spec.intent 中的伪引用，也不改变业务前置条件。查询已完成 Execution 的结果不是轮询正在运行的动作，也不会重放动作；不要因为本 Run 只允许执行当前 Skill 而认为前序结果不可读取。尚无前置结果时不能猜测输入或把未来步骤视为已完成。`

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

const taskRecoveryInstruction = `Robot Skill 已明确失败且物理执行已经终止。请判断本 Task 是否能在批准范围内恢复。只返回 JSON：{"decision":"revise_pending|fail_task","summary":string,"replacements":[{"id":string,"kind":"robot_skill|agent_step","goal":string,"spec":object,"completion_criteria":object,"depends_on":[string]}],"retain_pending_ids":[string]}。局部失败优先使用增量恢复：replacements 只返回必要修复前缀，retain_pending_ids 列出原样保留的未执行步骤 ID，不重写这些步骤的 spec/intent/版本/criteria。服务端保留 ID 和内部依赖，将跨越失败或被替换步骤的依赖接到修复前缀末端；首批保留步骤等修复完成后继续。修复前缀用新 ID，depends_on 仅引用前缀内步骤。仅保留业务语义及数据来源仍有效的步骤；受影响的数据来源需要更新时替换该步骤。未列入保留列表的 pending 步骤被替换；省略 retain_pending_ids 兼容完整剩余计划。不要为一个局部故障重写无关的几十个后续步骤。只能选择 robot_catalog 中已安装且启用的 Skill，并使用精确版本。Robot Skill 的 spec 必须且只能是 {"skill_name":string,"skill_version":string,"intent":object}；intent 只说明本次恢复改变的稳定业务意图和依据，不得写入 request_key、Map generation、最终底盘Pose、完整 robot.run input，也不得写入 carrying_object、held_object 或 holding_object。精确位姿和执行参数由下一次 Task Execution 结合当前Map与Robot状态计算。不得返回 Stage、Action、Ability UUID，不得重放相同 request_key，不得修改已完成步骤。设备离线、服务未就绪或需要现场确认属于可等待的前置条件，不因缺少自动修复 Skill 就判定任务不可恢复。先依据当前资源快照和原 Action error.details，明确区分地图校验失败、定位过期、定位未确认和控制模式；fresh=false 可能仅因 confirmed=false，不得据此宣称坐标已过期。robot.get 的目录或历史 Execution 不会刷新导航、关节和夹持反馈；若必须补充这些反馈，将对应公开 state 调用安排到替代步骤前部，再接修复和剩余步骤，不在只读恢复 Run 中重复查询等待反馈变化。需要用户核对位置、切换控制模式或恢复服务时调用 interaction.ask，成功创建问题后立即结束本 Run，由系统保留暂停并等待回答；不要同时返回 fail_task。用户回答后重新读取状态，条件未恢复不得盲目重发，已恢复则 revise_pending，不重做已完成步骤。只有目标确实不可达、用户取消或批准范围内没有可行恢复途径时返回 fail_task 和空 replacements。`

func buildTaskRecoveryCorrectionPrompt(problem error) string {
	return `上一份 Task recovery 输出没有通过机器契约校验。不要解释，也不要沿用其中的非法结构；请依据同一恢复上下文重新输出包含 decision、summary、replacements 及需要时 retain_pending_ids 的完整 JSON。局部修复仅修改出错的前缀，仍适用的保留 ID 不展开重写。Robot Skill 的 spec 仍只能包含 skill_name、skill_version、intent；intent 的任何层级不得包含 carrying_object、held_object 或 holding_object。本 Run 的纠正次数有上限，即使错误类型相同也应依据字段或位置诊断修正。不得执行动作、改写已完成步骤、重做已成功的工具查询或扩大批准范围。校验错误：` + problem.Error()
}

func buildTaskRecoveryPrompt(input []byte) string {
	return taskRecoveryInstruction + planningHoldingBoundaryInstruction + taskDependencyInstruction +
		`恢复依据包括 action_evidence 中原 Action 的错误和停止证据。physical_started 是物理类调用已受理的保守标记，不等同于实测发生运动；stop_evidence.confirmed 只证明原动作已停止，不证明已到位或当前空夹。retryable=false 不代表禁止补齐前置条件后重新规划；禁止原参数盲重试。前置姿态或规划起点缺失时，按 Skill 文档安排当前状态查询、必要的支持能力及后续执行；已有已完成步骤的 execution_ref 可供 robot.get 复用，实时关节、夹持和规划起点必须重新获取。` + "\n恢复上下文：" + string(input)
}

var taskRoleInstructions = map[string]string{
	"robot":     `你是当前 Robot Task 的执行责任人，只推进 current_subtask。若 spec.intent.task 明确选择了调用，input.task 必须保持该选择；即使 Skill 相同，也不能以 plan/prepare 成功替代 execute 或状态查询。依据Task input、spec.intent、execution_guidance、current_skill_contract和live_resource_snapshot组装完整robot.run.input。资源快照可用且输入充分时直接调用robot.run；只有当前Skill确实缺少实时Robot事实，或快照stale/unknown、Scene/Layout变化、状态冲突时才调用robot.get。缺少当前契约要求的地图信息时才按需调用map.query；同一Run中参数和revision未变化的查询必须复用。当前步骤要求避开失败方案时，使用 failed_subtasks 的 error、input_bindings 和 execution_lookup 核对实际失败来源，不能仅凭前序规划 completed 就把同一方案当成尚未尝试；历史失败不构成永久禁用，条件变化后按当前证据判断。地图信息不能替代Skill要求的执行期物理验证。经 input_bindings 补全后的完整输入必须符合当前Skill契约，业务身份来自已批准Task，不复制前序Skill完整结果或构造伪引用。无法由工具解决且会改变业务结果、授权或安全选择时才调用interaction.ask；设备忙碌或离线属于资源等待。robot.run返回accepted后立即结束本Run，不轮询，也不得再次调用robot.run。不得直接调用Ability、Robot SDK或改写其他SubTask。`,
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
	if role == "robot" {
		instruction += robotResultLookupInstruction
	}
	return instruction + " " + taskOutcomeInstruction + "\nTask：" + string(data)
}
