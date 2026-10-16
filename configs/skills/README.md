# 场景 Agent Skill 接入

这里是 Semantic Server 的 Agent Skill 模板，不是 Pilot 上可执行的 Robot Skill 包。
运行时实际根目录由 `skills.dir` 指定；`semantic init` 使用安装副本的绝对路径。
服务只扫描该目录下的 `SKILL.md`，不会扫描任意工程的 mission.md。

Kitchen 场景使用两份指导：

- [kitchen-workflow-planning](workflow/kitchen-workflow-planning/SKILL.md)：Leader 规划业务 Task。
- [kitchen-pick-place-task](robot/kitchen-pick-place-task/SKILL.md)：Robot Agent 拆分、执行与恢复 SubTask。

机器人、物品、桌面、返程和授权属于本轮 Task input；当前物理状态与执行参数
通过已安装 Robot Skill 读取。场景指导不引入新的执行引擎或动作许可。

## 安装到已有服务

1. 将以上两个目录复制到运行配置 `skills.dir` 对应的 workflow/ 与 robot/ 下。
2. 在实际 `agents.profiles_dir/leader/role.yaml` 的 skills.allowlist 追加
   `kitchen-workflow-planning`，在 robot/role.yaml 追加 `kitchen-pick-place-task`，
   保留已有角色配置。仓库模板已包含这两项，旧安装副本不会自动同步。
3. 通过 `GET /api/v1/projects/{id}/bindings` 读取现有绑定，再通过相同路径的 PUT
   更新 kitchen 项目。保留 agent_ids 和适用的通用 Skill，加入上述两个名称。
   专用 kitchen 项目不绑定 depalletizing 两份指导；拆码垛项目绑定其原有指导。
   不改其他项目、不清空绑定规避冲突；有运行任务时沿用原 API 的更新规则。
4. Skill 文件修改由目录监听热加载；角色 allowlist 使用已缓存 Profile，更新已有
   服务时在任务空闲后正常重启 Semantic Server，或使用其已有 profiles_dir 重载机制。
   只改 SKILL.md 不需要重启。不要认为网页刷新就会清除服务端角色缓存。

例如，专用 kitchen 项目可使用以下绑定；应用前与实际现有配置合并：

```json
{
  "agent_ids": [],
  "skill_names": [
    "artifact-usage", "echo-guide", "data-profile", "semantic-diagnostics",
    "kitchen-workflow-planning", "kitchen-pick-place-task"
  ]
}
```

## 验证与使用

- `GET /api/v1/skills` 可见两份 Skill，详情接口能返回完整正文。
- `/api/v1/agents` 可见对应角色允许的 Agent Skill；会话的有效范围还需与项目绑定取交集。
- 规划阶段通过已有 `skill` 工具按需读取；Task 执行通过 execution_guidance 接收
  当前角色在项目中的指导正文。参考文档仍通过 robot.get 读取当前设备安装的精确包。
- 源码验证：`go test ./internal/agent/runtime -run TestKitchenSkillsProjectGuidance -count=1`。
  测试覆盖真实模板加载、规划可见性、执行正文注入、角色边界与两个场景的项目隔离。

用户可使用短任务：“将取物桌上的可乐搬到放置桌中央，完成后收臂直立并返回本轮起点。”
桌名必须对应当前项目，模拟/实机和目标机器人由用户任务或明确项目上下文确定。
该提示只描述目标，实际步骤由 Agent Skill 与已安装 Robot Skill 共同提供。
