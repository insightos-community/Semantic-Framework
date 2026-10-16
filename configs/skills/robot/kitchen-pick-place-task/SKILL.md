---
name: kitchen-pick-place-task
description: R1Pro 厨房抓放 v5：完整阶段链、结果绑定与少量失败修正示例，优先一次正确调用
category: robot
when_to_use: Robot Agent 编排 R1Pro 厨房跨桌抓放、放置或对应局部恢复任务时
tags: [robot, kitchen, r1pro, atlas, pick-place, recovery]
---

# Kitchen 抓放调用约定 v5

只编排当前设备已启用的 Robot Skill，沿用原能力。精确版本、当前契约和本轮回执
优先；不直连 HAL/ROS，不建立额外 Transfer 会话、任务锁、许可或运动门限。

调用名必须精确：下表中的 Scene/Manipulation/Gripper/Navigate/Torso/TargetSearch 是
阅读简称，对应 skill_name 为 r1pro-scene / r1pro-manipulation / r1pro-gripper /
r1pro-navigate / r1pro-torso / r1pro-target-search。不是 `Manipulation.plan` 或 `manipulation`。
每个 robot.run 的外层为 `{skill_name,skill_version,input,input_bindings}`；input 是
`{task,parameters}`，input_bindings 与 input 同级且属于该次调用；skill_version 取启用版本。
夹爪开合统一 `r1pro-gripper`、task=`gripper`，parameters.mode=`position|grasp`，没有 move 任务。
运动参数中的 confirmed=true 使用本轮已获得的任务授权，不添加逐步审批。

## 每步决策

1. 从 current_subtask 确定一次调用；执行授权和业务参数取本轮 Task。
2. current_skill_contract、execution_guidance 已齐时直接组装输入。仅缺字段、
   版本变化或未覆盖分支才查对应文档；不重读本文件或整套技能。
3. 需要分支才 `robot.get(result_paths)` 读小字段；转交 plan/sessions/packet 用
   input_bindings，不回读整条轨迹。accepted 后结束 Run，等待真实终态。
4. 规划成功只表示有计划。当前输入来源未变、前提未修复时，不重复同一失败调用；
   失败先按下文定位应改的字段或步骤，不从头重写业务链。

## 编排覆盖表

按表展开独立调用与串行依赖。一个 plan 不能同时代表 prepare 或执行。
对象、桌面、目标和返程要求取本轮；地图用 status=active，XYZ 来自同一实体同一版本。

| 阶段 | 必须覆盖的调用及交接 |
|---|---|
| 初始化 | Scene.query、Manipulation.state、Gripper.state、Navigate.state；保存本轮新鲜底盘起点和已保存双臂初始位，不能用全零替代初始位 |
| 两侧选位 | Scene.atlas_status → atlas_evaluate → atlas_plan；分别保存取物侧 observe/grasp 角色和目的操作角色 |
| 取物导航 | Manipulation.prepare_transport → Navigate.navigate；readiness 区分地图、定位、人工确认及 mode=5 |
| 观察入口 | Torso.move(observe) → Scene.context(atlas_role=observe)；entry_context 非空且 replay 不可用时 move_joint(resume_observation=entry_context) → 刷新 context |
| 扫掠 | 按已有开口配置张开 → TargetSearch.sweep；使用 replay/search_config/table_reference/object_geometry，不另串全部原子搜索操作；两个独立视角一致即可沿原判据结束 |
| 锁定选姿 | plan(target-handoff,operation=lock) → 按锁定 position_map_m 再 atlas_evaluate → plan(target-handoff,operation=select) |
| 同站换姿 | 需要时 stow_observation(观察 atlas_selection) → prepare_transport → Torso.move(choice.torso_q) → 新 context(grasp) → target-handoff.validate_context；核验允许则复用锁，不强制重拍 |
| 抓取 | grasp_plan（内部新 state；旧契约用新 state → plan(grasp)）→ plan(prepare) → 按 plan.opening.gap_m 开爪 → execute_group(approach) → 按 plan 闭爪 → Gripper.state 确认持物 → execute_group(lift_return) |
| 携物导航 | prepare_transport → 新鲜持物及运输姿态 → navigate(目的停靠) → Torso.move(目的操作位) → 新 Scene.context(grasp,target_mm=本轮落点) |
| 悬停 | placement_plan(hover，内部取新 state) → plan(prepare) → execute_group(hover) |
| 下放 | placement_plan(land,hover_plan=本次悬停计划，内部取新 state) → plan(prepare) → execute_group(place) |
| 释放 | verify_placement（内部取新 state 和 at_release）；verified/release_allowed 后张开 → Gripper.state 确认空夹 → execute_group(retreat_return) |
| 返程 | prepare_transport → navigate(本轮保存起点，如任务要求) → 最终 state/导航结果；分别报告放置、空夹、收臂直立和返程证据 |

双臂普通初始化用 move_joint，运输准备用 prepare_transport，实际躯干运动用 Torso.move。
两个非直立躯干姿态之间复用“回直立 → 到目标”的两个独立 SubTask；回零成功不能
替代目标姿态步骤。携物回收用带物体避障的 lift_return，普通 joint 不因附加 source 就具备携物避障。

目的 atlas_evaluate.search 的 candidate.posture 是观察位；decision.action=repose 时，
decision.posture 才是操作位。atlas_plan 传 posture/grasp_posture=decision.posture、
observe_posture=candidate.posture，再从该 plan 取 torso_q。context 携带同一完整选择
和 target_mm；仅改 atlas_role 不会切换姿态记录，仅传桌名不会生成物品落点。
抓取同站换姿则用 select.choice.posture 覆盖观察 atlas_selection.grasp_posture。

## 结果字段和绑定

指针相对 execution.result；input_path 相对 robot.run.input。符号引用均取本轮已完成
SubTask 的完整 execution_id。不要截短 ID，不把 error/input/stop_evidence 当结果内指针。

| 生产者 | result_path | 用途 |
|---|---|---|
| Scene.query | /payload/document/{tables,objects,locations} | 当前场景 |
| Scene.query | /payload/interface/arm_presets | 初始位 presets.initial.q、joint_names、model_fingerprint |
| Navigate.state | /payload/{pose,fresh,readiness} | 起点和导航前提；queried 不等于可导航 |
| Manipulation.state | /payload/{arm_q,torso_q,torso_health} | 新起点与原厂健康诊断；关节新鲜不等于无故障 |
| Gripper.state | /payload/{fresh,holding,feedback} | holding=null 不是空夹 |
| Scene.atlas_status | /payload/identity | Atlas 身份，不能代替启动身份 |
| Scene.atlas_evaluate | /payload/{direct,grasp,search} | 候选，无 candidates/result 层 |
| Scene.atlas_plan | /payload/{goal,stop,posture,torso_q,posture_roles} | 当前选择，无 inputs 层 |
| Scene.context | /payload/{atlas_selection,table_reference,target_torso,search_config,target_selection,boot_identity} | 当前姿态下交接；boot_identity 取此处 |
| Scene.context | /payload/atlas_replay/{entry_context,replay} | 进入起点及扫掠 |
| TargetSearch.sweep | /payload/{multi_view,session_id} | 锁定观测；IDs 在 multi_view.observation_ids |
| target-handoff lock | /payload/result | 作为 source；select.config 取 context.target_selection |
| target-handoff select | /payload/result/{switch_required,choice} | choice.torso_q / choice.posture |
| grasp / placement plan | /payload/result/{plan,torso,planner_identity} | 几何计划与身份 |
| prepare plan | /payload/result/sessions | 阶段组；单段内含 packet/digest/options |
| grasp plan | /payload/result/plan/opening/gap_m | 开爪净开口，m |
| grasp plan | /payload/result/plan/{closing_gap_m,estimated_size_m} | 闭爪 opening_m、expected_contact_m=estimated_size_m[1] |

绑定项为 {execution_id,result_path,input_path}；被绑定字段在 input 中省略，不补 0/null。
包与 digest 必须同段，原样传递。Scene/state 顶层为 /payload，Manipulation.plan 多一层 /result。

## 正常链优先使用已验证入口

版本以 current_skill_contract 为准，不用历史版本号。新契约含 grasp_plan 时，绑定本轮
锁定 source 即可；可绑定初始化 Scene.query 的 return_q 固定本轮收回目标，省略则入口
读取当前保存初始位。入口自己取得同一次新鲜 arm_q/torso_q、补 phase=final，只调用
原 state/query/plan，不运动。不要再额外拆一个 state，也不搜索 artifact 找 return_q。

```json
{"task":"grasp_plan","parameters":{"velocity":0.2,"workers":1,"budget_s":60}}
```

LOCK `/payload/result` → `/parameters/source`；可选 INITIAL
`/payload/interface/arm_presets/presets/initial/q` → `/parameters/return_q`。
输出与原 plan(grasp) 完全同层：`/payload/result/plan`、`torso`、`planner_identity`。
旧契约没有 grasp_plan 时才使用下方四绑定例子。缺参恢复先消费 error.details 的
missing_fields/field_sources，一次补齐，不用 20 轮检索同一字段。

计划中明确保留“观察入口之后张开 → sweep”的依赖；张开不是 sweep 内部阶段。
导航前 prepare_transport、抓取 approach/lift_return、放置 hover/place/retreat_return
均沿用覆盖表，不靠一句笼统“完成抓放”替代公开调用。

## Few-shot：只修正发生错误的交接

**锁定目标最终抓取。** 错：只绑定 source/start_q/start_torso，遗漏 phase 或 return_q。
对：input={task:"plan",parameters:{kind:"grasp",parameters:{phase:"final",view_ids:[],
config:{target_mode:"locked"},velocity:本轮速度,workers:本轮配置,budget_s:本轮配置}}}。
通过 input_bindings 同时绑定以下四项：

| 来源（本轮已完成 Execution） | result_path | input_path |
|---|---|---|
| target-handoff lock | /payload/result | /parameters/parameters/source |
| 同一次新鲜 Manipulation.state | /payload/arm_q | /parameters/parameters/start_q |
| 同一次新鲜 Manipulation.state | /payload/torso_q | /parameters/parameters/start_torso |
| 初始化 Scene.query | /payload/interface/arm_presets/presets/initial/q | /parameters/parameters/return_q |

return_q 是保存的 14 轴双臂收回目标，不能替换为当前起点、全零或 Atlas 的 7 轴右臂解。
“须从当前姿态生成最终规划”也可能只是遗漏 phase="final"；“return_q 需要 14 个数值”
先检查遗漏绑定，不代表设备或运动故障。恢复时一次补齐这组字段，以新 request_key
重新规划并让下游 prepare 引用新结果。已有 Scene.query 回执即能绑定保存初始位，
不需要搜索 artifact 或重新浏览技能。恢复决策需要刷新状态时将 state 放进修复前缀，
随后重新规划；只读恢复 Run 不循环查询 robot.get 等待新反馈。

**准备执行包。** 错：把 grasp 的整个结果塞入 prepare.source。
对：input={task:"plan",parameters:{kind:"prepare",parameters:{velocity:本轮速度}}}；
PLAN `/payload/result/plan` → `/parameters/parameters/plan`，
PLAN `/payload/result/torso` → `/parameters/parameters/torso`。

**完整接近组。** 错：只执行 pregrasp 后闭爪。
对：input={task:"execute_group",parameters:{group:"approach",confirmed:true}}；
PREPARE `/payload/result/sessions` → `/parameters/sessions`。同样用 lift_return、hover、
place、retreat_return；Skill 按 options.grasp_end 覆盖独立或合并包。组不包含开合夹爪。
若 pregrasp 已完成且只漏 grasp，恢复计划开口后仅 execute_prepared(grasp)，不重放整组。
单段绑定目的为 `/parameters/packet`、`/parameters/digest`，没有第二层 parameters。

**开夹软件到位误报。** “夹爪定位修正仍未到位”来自自有位置跟踪器，不是原厂硬件
告警。部署已撤回后来收紧的 0.05 mm 单向到位门限，恢复既有 1 mm 定位容差；例如
目标行程 96.0 mm / 反馈 95.8 mm 不再单独判为失败。用户已要求忽略这种软件偏差，
且部署确认服务恢复后，读取新鲜 Gripper.state，再以新 request_key 重试原计划开口。
不要继续追问同一软件误报是否为机械限位；也不要伪造硬件故障已解除。真实硬件故障、
无反馈或机械阻挡仍以能力返回处理。重启运动服务后先确认链路 ready；后续已封装
轨迹仍按能力校验结果执行，过期或失效时重新 prepare，而不是直接绕过能力调用。

**已张开后的空夹撤回。** 执行器已取消自行添加的 gripper_guard 开口区间判定，
以本轮张开动作成功及 Gripper.state 的空夹结果继续撤回。历史错误如反馈 95.306 mm /
下限 95.413 mm 不再要求反复开夹或现场排障，也不修改反馈假装满足区间。
服务更新后从新 state 核对轨迹起点；原包已消费或失效则通过既有 prepare 重新封装
尚未完成的撤回组，保留已完成的放置和释放，不重复下放。

**导航停滞后的同点重试。** 用户要求取消卡住的导航并重发同一点位时，只取消当前
导航 Action，不等于取消整个搬运任务。原厂已返回取消终态且能力确认停止后，以
新 Navigate.state 和 Gripper.state 复核导航前提及携物状态，再用新 request_key
重试原目标与原容差；保留尚未执行的放置、释放和返程步骤。不重做已完成抓取，
不把“取消”标记成到达，也不把下一次目标改成当前点。若同点重试仍无进展，应
诊断原厂规划/障碍反馈，而非无条件循环取消重发。
navigation_cancelled 或原厂 status=5 本身不是硬件故障证据；manual_cancel=true
且本轮已有取消重发指令时，应保留操作来源，不能据此推断过热或要求“维修完成”。
原厂健康状态、实际障碍和控制模式仍按当前反馈判断，不从历史故障套用原因。

**已知桌面放置。** 错：遗漏实时起点、return_q，或把 hover 的整个结果包装当计划。
优先 `r1pro-manipulation` 的 `placement_plan`（当前契约存在时）：

```json
{"task":"placement_plan","parameters":{"phase":"hover","velocity":0.2,"bottom_gap_range_m":[0,0.01],"radius_m":0.03,"tilt_tolerance_deg":2,"yaw_candidates_deg":[0,30,-30,60,-60,90,-90,180]}}
```

绑定 GRASP `/payload/result` → `/parameters/source`，DEST `/payload/table_reference` →
`/parameters/known_table`，DEST `/payload/target_torso` → `/parameters/target_torso`。
land 改 phase="land"，HOVER `/payload/result/plan` → `/parameters/hover_plan`。
入口读取当前 state，return_q 默认来自同一 source 的 return.goal_q；用户本轮参数优先。
结果仍是 payload.result.plan/torso，下一次 prepare 的绑定不变。
当前契约没有此入口时沿原 plan(kind=placement)：内部 phase="final"，view_ids=[]，
config.placement_phase=hover|land，其他几何参数在 config，start_q/start_torso 取新 state，
return_q 取 GRASP return.goal_q。result=null 不是可用计划，不能 prepare。

**稳定接触释放。** 错：负模型间隙自行推断接触，或收到现场确认后仍重复下放。
当前契约提供 `verify_placement` 时直接使用：

```json
{"task":"verify_placement","parameters":{"bottom_gap_range_m":[0,0.01]}}
```

绑定 LAND `/payload/result/plan` → `/parameters/plan`，GRASP `/payload/result` →
`/parameters/source`，DEST `/payload/table_reference` → `/parameters/known_table`。
入口读取新 state，再调用既有 at_release 核验；不释放夹爪。
本轮已明确确认接触且稳定时在 parameters 中加 support_confirmed=true、support_evidence=本轮原话。
未确认则省略这两个字段，不能自行声称已支撑；也不新增所有放置都必须人工确认的要求。
核验返回 verified/release_allowed 后直接释放。不因非正估计重复下放。
旧版本沿用 plan(kind=verify-placement)：plan/source/start_q/start_torso 在内部 parameters，
known_table/at_release/support_confirmed/support_evidence 在其 config。

**已消费包恢复。** 错：sent=0 就重传旧包，或手改 ID/digest。
对：持物和起点仍匹配抓取端点时，用同一 GRASP plan/torso 重新 prepare，
parameters.remaining_only=true，再执行新 lift_return。prepare 只是封装，不会修复已改变的几何起点。

**导航单位。** 已确认默认提前结束为 position=0.05 m、yaw=0.02617993877991494 rad、
input={task:"navigate",parameters:{confirmed:true,early_finish:true,
tolerance:{position:0.05,yaw:0.02617993877991494}}}。ATLAS `/payload/goal/{frame,x,y,yaw}` 分别绑定到
`/parameters/goal/frame`、`/parameters/goal/pose/position/{x,y}`、`/parameters/goal/pose/yaw`。
5 cm÷100，1.5°×π÷180；用户新要求优先，不为重试扩大容差。

## 局部恢复

- 参数失败且 physical_started=false：只修输入或缺失步骤，不要求机器人回退。
  调整 timeout 不修复 budget、字段层级或缺参；纯 plan 成功不代表设备运动。
- deadline_exceeded 先看报错层级：Ability 限时不受 Skill timeout_seconds 控制。
  同一导航目标未收敛时，把外层 600 改成 1200 秒不会修复；先查实际导航结果与停止证据。
- 先看原 Action 的 error.details / stop_evidence，再决定哪些步骤受影响。仅修复前缀，
  retain_pending_ids 保留来源有效的后续 ID/版本/依赖；数据来源变化才替换相关步骤。
- 状态缺失：调用新 state；反复 get 旧 execution 不刷新现场。过热等 hardware_fault
  等原厂故障解除，不重规划、重启或清码假装恢复。断线不是故障已消失。
- 同一几何错误、新 state 仍相同：说明前提未变化，等待对应规划/服务修复，不继续同参重试。
  原厂导航障碍与低速中间点收敛分别处理；曾经到达或一条 SOFT/HARD 日志不能替代本轮终态。
- 原厂 too near / global_path.empty 且剩余误差未变：保留失败，向用户报告实测残差和
  本轮容差；不得连续同条件重发，也不自行扩大阈值。用户明确接受本轮残差后，修复前缀
  只做最终定位、空夹、运输位核验并保留后续步骤；接受依据保留在 interaction_answer，
  不写入永久技能或 Project Memory。原失败不得改成到位成功。
- readiness 缺现场位置确认时使用现有 interaction.ask，收到回答后读新 state；不因没有
  自动确认 Skill 直接 fail_task。单确认用 confirm 且无 fields，多字段用 form，single_select 仅一个字段。
- 保留真实成功与失败回执，不能以其它 state 冒充原动作 stop_evidence。
  用户暂停/充电只保存记录，等恢复指令。记忆只存方法，不存可重放坐标、关节或包。

## 故障来源与记忆适用性

先用本轮 error.details 的 layer/cancel_origin/native_detail、原 Action 和版本匹配故障；
人工取消不是过热证据。原厂 detail 为空时明确“未提供”，不能把旧日志作为本次原因。
仅文档写“已修复”不足以证明当前版本生效：对照当前契约和新回执。开合容差与轨迹
开口 guard 是两层；前者修好不代表后者修好。路线来源哈希失配属于发布一致性问题，
等待匹配部署，不重新计算 Atlas、不让 Agent 自行改 hash 或绕过校验。
