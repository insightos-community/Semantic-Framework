---
name: depalletizing-robot-task
description: 将一个搬箱 Robot Task 拆成可串行执行的四个 Robot Skill SubTask
category: robot
when_to_use: Robot Agent获得一个将周转箱搬到目标堆叠列的 Task时
tags: [robot, depalletizing, subtask, planning]
---

# 拆码垛 Robot Task 规划

本 Skill 只指导 Robot Agent 生成和推进 SubTask，不直接运行 Python、Ability 或
Robot SDK。只能选择当前 Robot 已安装并启用的精确 Robot Skill 版本。

## 单箱步骤

按当前 Robot Pose 和对象位置生成以下串行步骤：

1. `semantic-navigation`：以来源对象的精确 `object_ref` 为业务目标；实际可操作基座 Pose
   由 Navigation Ability 从实时场景和 Robot Profile 解析，不依赖 approach Region。
2. `grasp-object`：传目标对象身份、可选几何提示、工具和策略。Skill会实时观测，
   并在成功前把双臂整理到携物转运姿态。
3. `semantic-navigation`：一次携物导航到目标堆叠列的放置工位，只传目标位姿和
   `carried_object_ref`，不得复制上一项的 HeldObjectState。
4. `place-object`：只传 `object_ref` 与目标列。Skill会实时读取持物、计算下一层
   放置 Pose、验证稳定，并恢复 `travel` 姿态。

## 状态边界

- SubTask 依赖表示执行顺序，不表示通过 JSON 搬运物理状态。
- intent 不得写入 `carrying_object`、`held_object` 或 `holding_object`，包括
  布尔值和 null。空载或携物属于执行期状态，写入 `completion_criteria`；Skill
  通过 RobotState Ability 实时读取。`carried_object_ref` 只表示要搬运的对象
  身份，可以保留。
- Task input 中对象或目标列的 `name/object_ref/target_ref` 是 Robot Skill
  重新观测时使用的业务身份；Map `entity_id/region_id` 只说明本次规划选择了哪条
  地图记录。生成 intent 和可执行输入时必须保留业务身份，不能把 Map ID 改写成
  `object_ref`、`carried_object_ref` 或放置 `target_ref`。
- 来源导航 intent 中的 `target.target_ref` 必须使用当前要抓取的精确 `object_ref`；
  `source_pallet` 只描述支撑关系，不能替代目标箱体。Task Execution把该对象的中心Pose
  作为提示交给 `semantic-navigation`，Navigation Ability再从实时SceneSnapshot解析支撑面、
  箱体extent、当前Robot位置和Profile，选择托盘外且箱体仍在双臂范围内的基座工位。
  Robot Agent不手算工作距离、栅格膨胀或四元数，也不使用Layout专用approach点。
- 携物导航 intent 中的 `target.target_ref` 必须使用精确目标堆叠列。Robot Agent只提供
  目标列中心Pose提示和 `carried_object_ref`；Navigation Ability结合实时支撑托盘与携物
  包络解析一次可达工位，SDK继续对实际路线做碰撞检查。不得先到托盘前再二次导航。
- Semantic Map只用于选择对象、列和基座目标；每个 Skill在 Stage入口通过Ability
  重新观测当前物理事实。
- 同一 Agent Run 中，参数和 revision 均未变化的 Map 查询结果必须复用；取得当前
  Robot、目标、支撑面和邻障碍等计算所需事实后直接形成工位，不重复查询同一实体。
- `robot.run` 返回 accepted 后停止模型轮询，等待 Robot Execution事件。
- 滑移、过载、传感异常或状态未知时先停止并hold；不得重放已经开始的物理Action。
- Skill进入 `waiting_agent` 时只在声明的局部决策范围内选择候选、刷新目标或终止；
  需要扩大业务范围时交还Leader或询问用户。
