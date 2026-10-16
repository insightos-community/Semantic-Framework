---
name: kitchen-workflow-planning
description: 根据厨房场景、用户目标和实际 R1Pro 资源规划跨桌抓取、搬运、放置与返程任务
category: workflow
when_to_use: 用户要求 R1Pro 在厨房桌面寻找可乐或已配置物品、抓取搬运到另一桌面并按要求返回时
tags: [workflow, kitchen, r1pro, pick-place, planning]
---

# Kitchen Workflow 规划

本 Skill 指导 Leader 将厨房搬运目标交给 Robot Agent。它是业务编排知识，
不直接执行机器人动作，不生成关节轨迹，不授予超出用户任务的操作权限。
适用于已安装 R1Pro 系列 Robot Skills 的设备；不能套用拆码垛机器人的四步组合接口。

## 新对话优先命中已有经验

Project Memory 的 kitchen-call-patterns-v5 是本项目的经验入口；Task 细节交给
`kitchen-pick-place-task`，不在 Leader 再推导一遍全部原子调用。
已有物品/桌面唯一匹配时，一次 robot.list 与必要 map.query 后直接形成业务 Task；
只处理本轮目标与现有经验的差异，不让用户重复确认已选定的正常流程参数。
本厨房已确认沿用原 10050 的 5 cm / 1.5° 单点/末点提前结束，契约值为
position=0.05 m、yaw=0.02617993877991494 rad、early_finish=true；用户新要求优先。

复用的是方法与字段来源。不要将历史 execution ID、坐标、关节、目标锁或轨迹包
作为新任务输入；也不要把“跨 Run”当成失效条件，SubTask 本就通过前序结果交接。
机器人版本与场景身份变化时核对相应契约/标注，已有 Ability 检查继续决定可执行性。

暂停/换电仅保留检查点与记录，不自动恢复。用户要求复盘时只讨论公开输入、
工具结果、决策结论与失效条件；不发动作、不生成执行提案。
经验本身不新增许可、任务锁、容差或新鲜度门限。

`plan.suggest` 沿用工具的显式 Plan Mode 要求；普通对话不反复提交失败提案。
已有未结束 Workflow 时先恢复或处理原流程，不另建重复 Workflow。
确需澄清时，单确认用 `confirm` 且无 fields，多字段用 `form`，单选仅一个字段；
已有业务授权与正常默认参数不再提问。
`interaction.ask` 的字段是 `prompt`、`presentation`，fields 每项用
`name/label/value_type/required`；不要用 kind、question、type 或 placeholder 代替契约字段。

## 解析目标与任务输入

- 先从当前 Project、`robot.list` 和带有效过滤的 `map.query` 确认机器人、物品、
  来源桌面和目的桌面；已知名称用 entity_names，查询当前场景用 status=active。
  查询达到 limit 时收窄条件，不把截断结果当作完整场景。
- 机器人 ID、模拟/实机、桌名、物品和是否返回起点来自本轮用户目标与当前项目。
  不写死任何实验机器人 ID、部署地址、桌高或坐标；不因 backend="real" 就把
  明确标记模拟的设备当作实机，也不能替用户从模拟切换到实机。
- 保存查询返回的稳定业务 `name` 作为 `object_ref`、`source_table_ref`、
  `target_table_ref`，保留必要的 entity_id 作为查询来源。不要根据示例拼接名称。
  地图粗位置是搜索和停靠提示，不是可直接抓取的精定位结果。
- 任务输入记录放置意图（例如桌面中央）、返程要求和用户指定的参数；运动初始
  位姿由 Robot Agent 在本轮执行时读取。不要把 Leader 规划时的位姿冻结为 start_q。
- 已有唯一匹配和完整任务授权时直接规划；只对真正影响结果的歧义或必要信息缺失
  发起澄清，不逐步增加确认、许可或任务锁。

## 首次提案直接表达完整业务

同一真实机器人、物品和两张桌子已明确时，首次提案包含：来源/目的稳定名字、
取物前与携物导航前运输准备、观察/抓取分工、开夹后扫掠、携物避障回收、
hover→land→核验→释放→空夹撤回，以及用户要求的返程。
“返回起点”指 Robot Task 开始时读取并保存的一份新鲜地图定位；后续重试或恢复
不能把当前位置改成起点。Leader 不提前冻结坐标，也不要求每次导航重设起点。
用户指定实机就绑定查询所得真实机器人，不加入“允许模拟”备选。
夹爪状态是 r1pro-gripper 的 state，开合才是 gripper；不使用模糊的 Gripper.state 技能名。
业务依赖交由 kitchen-pick-place-task 与当前契约展开，不再浏览全部 Robot Skills。

## Task 边界

一次“取物桌 → 放置桌 → 按要求返回”通常对应一个 Robot Task，`required_role="robot"`。
Atlas 查询、收臂直立、导航、扫掠、目标交接、抓取和释放都是这个 Task 的 SubTask，
由 Robot Agent 按实际反馈和已安装 Skill 的公开调用拆分。
多物品按每个物品一个 Task 建立必要依赖；同一机器人沿用现有调度，不重复创建设备。

Proposal 表达完整业务目标、允许的支持动作和完成标准。授权覆盖原任务所需动作时，
保留取景、离桌收臂、经直立换姿、携物回收、释放及正常恢复范围；本 Skill 本身
不构成授权。放置完成后是否返程，以本轮用户要求为准。

`resource_requirements` 只表达实际可调度资源。`backends` 若填写，应使用设备目录
真实实现标识；不要填写自造的 "simulation"。物品与桌面属于 Task input。

若明确批准使用 R1Pro 原能力链，`approved_scope.allowed_skills` 与
`required_capabilities` 保持一致，使用当前设备实际存在的 Robot Skill 名称。
典型完整链包含 `r1pro-scene`、`r1pro-navigate`、`r1pro-torso`、
`r1pro-manipulation`、`r1pro-gripper`、`r1pro-target-search`，需要独立在线观测
分支时包含 `r1pro-spatial-observation-3d`。精确版本由分配后的设备目录确定。
本 Agent Skill 名称不是可执行能力，不得填入 required_capabilities。
若采用后绑定，按现有规则同时省略 allowed_skills 和 required_capabilities，
不能虚构能力或版本。普通抓放不要求建立 `r1pro-transfer` 会话。

## 交给 Robot Agent 的流程目标

1. 读取当前起点和保存的双臂初始位，查询取放两侧 Atlas，区分 observe/grasp。
2. 运输准备后导航到取物侧，到观察姿态，进入记录的起点，执行异步多视角扫掠。
3. 锁定实测目标并重新评估 Atlas；需要换姿时离桌收臂、经直立中转，刷新上下文。
4. 从实测起点规划、准备并执行抓取，确认持物，沿携物避障路径抬升和回收。
5. 运输准备后导航到目的桌，分别规划、准备和执行 hover 与 land，核对后释放。
6. 空夹撤回，恢复运输姿态；按任务要求返回本轮初始定位，并查询最终证据。

Robot Agent 使用 `kitchen-pick-place-task` 和实际安装的
`r1pro-manipulation/references/panel-flow.md` 展开调用。
这些业务阶段不是固定的 6 次 Robot Skill 调用；计划、prepare 和执行不能合并成
一句描述而省略实际调用，也不能把计划成功当作运动完成。

## 完成与恢复

完成证据包含物品位于目标桌、释放后夹爪空载、双臂与躯干恢复运输姿态，
以及用户要求的返程和无未完成动作。证据来自实际 Skill 回执和新鲜状态，
不能只用地图记忆或计划终点宣告成功。

失败保留已完成步骤与原动作证据。局部修复优先生成修复前缀，并以
retain_pending_ids 原样保留仍适用的未执行 SubTask；不让 Robot Agent 为局部失败
重写完整的剩余链。仅因业务意图或数据来源变化才替换相关步骤。
参数问题和未发生物理动作的失败不要求机器人回退；物理停止未知时先对账，
不能清锁、伪造成功或盲目重放。模拟验证与实机验证分开汇报。

用户在本轮交互明确接受已测残差时，将该回答留在当前恢复上下文，保留失败历史并安排
只读终态核验；不要重发已被用户放弃的精确到位步骤，也不要将本轮接受沉淀为永久容差。
最终总结只依据实际 Task/Skill 回执区分 planned、executed 和用户接受，不因总结失败
改写已完成 Workflow 或要求机器人重做。
