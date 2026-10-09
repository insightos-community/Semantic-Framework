# 原生 MuJoCo Robot Skill 本地调试

[English](README.md) | [简体中文](README.zh-CN.md)

这个入口只用于调试一条真实 Robot Skill 物理链，不启动 Semantic Server、Web、
Workflow 或 Agent。`semantic-pilot` 仍负责 Worker、Action 路由、Ability 调用和
安全停止，不能直接执行 Skill Python 代替 Pilot。

## 前置组件

运行命令前必须已经具备：

1. 一个 running 的 MuJoCo scene instance；
2. 与该 scene instance 对应的 `robot-deployment.yaml`；
3. 一个启动了七类 Ability 的 AbilityFramework；
4. 安装了 Robot Skill SDK 与具体 Skill 依赖的 Python；
5. 一个包含 `grasp_object`、`semantic_navigation`、`place_object` 目录的 Skill Catalog。

同一 Robot不能同时由常驻 `semantic-pilot` 和本地调试命令控制。运行前应先停止
对应的常驻 Robot instance，再用 `debug-stack` 重新启动 AF和Ability；不要仅凭
“当前看起来 idle”绕过跨进程互斥边界。

`RobotDeployment` 提供 Robot ID、Runtime endpoint、scene instance ID、
AbilityFramework endpoint、frame、tool、IK 和安全配置。Skill输入只保存业务目标，
不重复这些部署参数。

## 构建

```bash
cd /home/wwy/agent_refractor/.worktrees/semantic-framework-robot
go build -o .output/bin/semantic-pilot ./cmd/semantic-pilot

cd /home/wwy/agent_refractor/semantic-robot-deployment
go build -o bin/semantic-robot-instance ./cmd/semantic-robot-instance
```

## 启动本地组件栈

先停止同一 rendered instance 的完整 supervisor。该操作会让常驻 Pilot安全停止
当前工作，然后依次停止 Ability和AF；不会停止 MuJoCo scene Runtime：

```bash
DEPLOY_ROOT=/home/wwy/agent_refractor/semantic-robot-deployment
FRAMEWORK_ROOT=/home/wwy/agent_refractor/.worktrees/semantic-framework-robot
INSTANCE_ROOT="$FRAMEWORK_ROOT/.output/v050-mujoco-product/three-skills-instance9"

"$DEPLOY_ROOT/bin/semantic-robot-instance" stop \
  --instance "$INSTANCE_ROOT" \
  --timeout 30s

"$DEPLOY_ROOT/bin/semantic-robot-instance" debug-stack \
  --instance "$INSTANCE_ROOT"
```

`debug-stack` 是前台进程。看到 `status: ready` 后保持此终端运行。它与完整实例
共用 `instance.lock`，但不启动 Pilot、不连接Server，也不使用 access token。

如果 `stop` 报告实例本来已经 stopped，可以直接运行 `debug-stack`。如果报告
`interrupted`，不要继续启动本地调试，应先确认 Robot物理状态。

需要观察 Runtime真实相机时，在另一个终端打开随仓样例页：

```bash
xdg-open "$FRAMEWORK_ROOT/examples/mujoco-skill-debug/live-camera.html?endpoint=http://127.0.0.1:18090&robot=r1_pro_tote_gripper-1"
```

该页面只轮询 Runtime的 `camera.rgb`，不模拟 Robot状态，也不替代后续
Semantic Web Physics Viewer。

## 执行抓取

```bash
FRAMEWORK_ROOT=/home/wwy/agent_refractor/.worktrees/semantic-framework-robot
SKILLS_ROOT=/home/wwy/agent_refractor/semantic-robot-skills
INSTANCE_ROOT="$FRAMEWORK_ROOT/.output/v050-mujoco-product/three-skills-instance9"
BUNDLE_ROOT="$FRAMEWORK_ROOT/.output/v050-mujoco-product/bundle-stable-load"

"$FRAMEWORK_ROOT/.output/bin/semantic-pilot" skill run \
  --profile "$INSTANCE_ROOT/robot-deployment.yaml" \
  --skill grasp-object@0.3.0 \
  --input "$FRAMEWORK_ROOT/examples/mujoco-skill-debug/grasp-object.json" \
  --skill-catalog "$SKILLS_ROOT/semantic_robot_skills/skills" \
  --python "$BUNDLE_ROOT/python/venv/bin/python" \
  --events "$FRAMEWORK_ROOT/.output/mujoco-skill-debug/grasp-events.jsonl" \
  --result "$FRAMEWORK_ROOT/.output/mujoco-skill-debug/grasp-result.json" \
  --timeout 10m
```

上面的 `INSTANCE_ROOT` 和 `BUNDLE_ROOT` 是当前开发产物示例，不是稳定安装路径。
重新创建 scene instance或重建类型包后，应替换为新实例生成的路径。禁止只修改
`scene_instance_id` 而继续使用另一场景的 Robot ID或 Ability实例。

命令输出中：

- `events.jsonl` 保存 Stage、Action、精确 Ability instance和反馈事件；
- `result.json` 保存 Skill终态、业务结果、错误和安全停止结果；
- Ctrl+C或超时会请求 `Skill stop → Ability stop → Robot hold`。

如果执行进入 `waiting_agent`，本地命令会明确失败；本地调试不会用固定回复模拟
Robot Agent。需要改变策略时，修改输入后从明确安全状态重新执行，或回到正式
Semantic Framework链路。

退出顺序必须是：先等待 `skill run` 完成，或在该终端按 Ctrl+C并看到停止结果；
再对 `debug-stack` 按 Ctrl+C。不要先关闭 AF和Ability。

## 参数来源

| 参数 | 是否必填 | 来源 |
|---|---:|---|
| `--profile` | 是 | 当前 scene/Robot生成的 RobotDeployment |
| `--skill` | 是 | Skill Catalog中的精确 `name@version` |
| `--input` | 是 | 业务输入 JSON；也可用 `-` 从 stdin读取 |
| `--skill-catalog` | 开发态是 | Robot Skill源码目录；安装态默认读 Profile中的 active目录 |
| `--python` | 开发态建议显式 | 类型包 Python，必须已安装 Robot Skill SDK和依赖 |
| `--ability-framework` | 否 | 仅诊断时覆盖 Profile endpoint |
| `--python-path` | 否 | 未安装源码依赖时的临时 import路径，可重复 |
| `--execution-id` | 否 | 复现实验时指定；默认自动生成 |
| `--events` | 否 | JSONL事件文件；默认 stderr |
| `--result` | 否 | 最终 JSON文件；默认 stdout |
| `--timeout` | 否 | 最长执行时间；默认 15 分钟 |

本地模式没有 `--project`、`--workflow`、`--task` 或 `--robot`：Robot来自
RobotDeployment，其余对象在这条调试链中不存在。
