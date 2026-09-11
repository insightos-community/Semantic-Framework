<div align="center">
  <img src="docs/assets/logo.png" alt="InsightOS Semantic" width="100%">

  <h3>面向具身场景的语义智能体系统</h3>

  <p>
    <a href="https://github.com/insightos-community/Semantic-Framework/stargazers"><img src="https://img.shields.io/github/stars/insightos-community/Semantic-Framework?style=social" alt="GitHub Stars"></a>
    <a href="LICENSE"><img src="https://img.shields.io/badge/License-Apache--2.0-blue" alt="Apache-2.0 许可证"></a>
    <img src="https://img.shields.io/badge/Version-0.5.0--dev-14796b" alt="0.5.0 开发版">
    <a href="https://semantic.insightos.cn/"><img src="https://img.shields.io/badge/Website-Semantic-3268d8" alt="Semantic 官网"></a>
  </p>

  <p>
    <a href="README.md">English</a> · <b>简体中文</b><br>
    <a href="#快速开始">快速开始</a> · <a href="#系统架构">系统架构</a> · <a href="#framework-开发">框架开发</a> · <a href="#组件生态">组件生态</a> · <a href="https://github.com/insightos-community/semantic-docs/tree/main/docs">项目文档</a> · <a href="https://github.com/insightos-community/Semantic-Framework/issues">问题反馈</a>
  </p>
</div>

---
InsightOS Semantic 是具识智能面向具身场景打造的语义智能体系统。它以语义连接机器人本体、环境、任务、Skill 和运行经验，将自然语言需求转化为机器人可以执行的任务，并贯通**理解—规划—执行—进化**的完整闭环。

InsightOS Semantic 支持售货仓取货配送、拆码垛搬运、多机协同流水线、定时巡检等场景，可连接人形机器人、四足机器人、机械臂和移动机器人等多种本体。

本仓库是 **InsightOS Semantic 的产品主仓与 Framework 实现**：`semantic-server` 管理项目、任务、注册表和 Runtime 调度，`semantic-pilot` 连接机器人执行环境，`semantic` CLI 提供配置和 Runtime 管理。完整系统还包含独立仓库中的 Studio、机器人技能与仿真组件。组织与生态入口见 [InsightOS Community](https://github.com/insightos-community)。

## 为什么选择 Semantic？

机器人从“能演示”走向“能干活”，通常面临三类问题：

- **不能理解**：任务依赖固定代码，难以理解人的意图和场景约束。
- **不懂变通**：环境、物体或执行状态发生变化时，固定流程难以继续。
- **不会进化**：执行经验难以沉淀，换本体、换场景后往往需要重新开发。

InsightOS Semantic 使用统一语义连接任务、环境、本体和能力，让机器人可以理解目标、组织 Skill、执行任务，并将运行经验沉淀为可复用能力。

## 系统架构

系统由应用交互、任务编排、机器人执行和环境设备四个部分组成，通过任务指令与执行反馈协同工作。

<p align="center">
  <a href="docs/assets/architecture-zh-CN.svg"><img src="docs/assets/architecture-zh-CN.svg" alt="InsightOS Semantic 系统架构" width="720"></a>
</p>

| 层次 | 主要模块 | 职责 |
|:--|:--|:--|
| **应用交互** | Semantic Studio、模型服务 | 用户在 Studio 中描述目标、审阅计划并查看结果；模型服务为智能体提供理解与推理能力。 |
| **任务编排** | Semantic Server | Leader 理解目标与环境，Workflow 组织任务和依赖，Robot Agent 选择技能并安排子任务。 |
| **机器人执行** | Pilot、Robot Skill、AbilityFramework、Robot SDK | Robot Skill 组织操作阶段，Pilot 分发动作，AbilityFramework 运行能力实例，Robot SDK 连接设备并返回观察与结果。 |
| **环境与设备** | MuJoCo Runtime、真实机器人 | MuJoCo 提供场景与物理仿真；真实机器人通过适配器与硬件接口接入，在现场执行动作并提供反馈。 |

## 核心 Features

- **自然语言任务交互**：通过一句话描述目标，由系统组织任务和 Skill。
- **多机器人协同**：面向异构机器人分配任务并协调工作流程。
- **语义世界模型**：同时表达物体是什么、位于哪里以及彼此关系。
- **动态环境适应**：根据环境和执行状态变化调整任务过程。
- **仿真驱动开发**：连接场景、机器人、Skill 和任务，在仿真中快速验证。
- **经验与能力进化**：将执行数据和经验沉淀为可复用、可持续优化的 Skill。
- **具身应用生态**：复用场景应用、Skill、模型、插件、适配器和仿真资产。

## 快速开始

### 1. 二进制安装

在 **Linux x86_64** 机器上准备 Bash、curl 和 Python 3.10+，执行：

```bash
curl -fsSL https://semantic.insightos.cn/install.sh | bash -s -- --install-system-deps
```

安装器会准备 Server、Studio、原生 MuJoCo、R1 Pro Robot Bundle、场景资产，以及导航、抓取和放置技能，默认安装到 `$HOME/.local/share/semantic` 并启动服务。`--install-system-deps` 表示通过宿主机的包管理器安装所需系统库。

### 2. 打开 Studio

打开 **http://localhost:3000**，使用 **`admin`** 和安装器显示的随机密码登录。以下命令可以重新查看登录信息与服务状态：

```bash
export SEMANTIC_HOME="$HOME/.local/share/semantic"
export PATH="$SEMANTIC_HOME/bin:$PATH"
semanticctl welcome
semanticctl status
```

从同一网络中的另一台设备访问时，使用 `http://<服务器 IP>:3000`。Web 网关默认监听 `0.0.0.0:3000`。日常启停服务使用 `semanticctl stop` 和 `semanticctl start`。

在 Studio 的**系统设置**中配置支持工具调用的模型，再创建项目并启动场景，即可按[第一个 Project](https://github.com/insightos-community/semantic-docs/blob/main/docs/user/getting-started/first-project.md)运行机器人任务。

### 详细安装指南

完整安装文档与源码构建流程见 **[Quick Start 中文指南](https://github.com/insightos-community/quick-start/blob/main/README.zh-CN.md)**。自定义目录和端口、服务管理及其他二进制安装选项见 **[二进制安装说明](https://github.com/insightos-community/quick-start/blob/main/artifacts/README.md)**，各组件的配套版本见[版本清单](https://github.com/insightos-community/quick-start/blob/main/repo-versions.json)。

## Framework 开发

### 工程结构

- `cmd/`：Server、Pilot 与 CLI 入口。
- `internal/` · `pkg/`：调度、服务、契约及公共库。
- `configs/`：配置模板；`scripts/`：构建与 Bundle 工作流。
- `tests/` · `ci/`：单元测试、集成门禁与构建自动化。

### 编译与启动

需要 Go **1.23+**、Make 和 Linux 开发环境。

```bash
make build
make init
go test ./...
make run
```

编译生成 `.output/bin/semantic-server`、`semantic-pilot`、`semantic`；初始化将配置写入 `.output/configs/`。初始化前通过本地环境变量或未跟踪的 `.env` 设置 `SEMANTIC_ADMIN_PASSWORD`，不要公开密码。

实例配置应修改 `.output/configs/semantic-server.yaml`，不要直接改共享模板。默认开发端点为 HTTP `127.0.0.1:8080` 和 WebSocket `127.0.0.1:8081`；Studio 是独立项目。

### 产物使用

仅编译 Server 不会配置好 Robot。完整部署应遵循 quick-start 的目录布局与版本清单：登记原生 MuJoCo、构建 AbilityFramework / SDK Wheel，再组装并激活 Robot Bundle。

准备好输入产物后，在本仓库查看构建入口：

```bash
python3 scripts/refresh_v050_mujoco.py --help
```

使用 Robot 的 Python **3.13** 解释器执行 `build` 工作流；Server 启动后再执行独立的 `publish` 工作流发布所需 Skill。替换正在使用的 Bundle 前，先停止相关场景与 Robot。

### 常见问题

- Robot 离线：检查 Pilot 连接、激活目录、Runtime 登记和 Skill 发布版本。
- 配置不生效：确认进程实际使用的生成配置路径。
- 配置热重载钩子失败时，已应用的配置段会恢复为旧值。如果回滚失败，系统会明确报错，设置快照保留该段最后成功应用的值。请修正无效配置后重试；配置文件本身不会被回滚。
- 单元测试不等于整栈验证：仿真门禁需要资产与关联仓库，模型服务门禁可能产生调用费用。
- 开发端口只向可信网络开放，令牌不要提交。

[历史技术参考](README.reference.md) · [配置模板](configs/) · [构建工作流](scripts/)

## 组件生态

Semantic 的组件围绕共同的任务模型和执行链路协作。你可以从本仓库了解整个产品，再进入对应仓库开发某一层能力。

| 层次 | 仓库 | 职责 |
|:--|:--|:--|
| 语义框架 | **[semantic-framework](https://github.com/insightos-community/Semantic-Framework)** · 本仓库 | Server、Pilot、CLI、智能体、任务编排与共享契约。 |
| 用户界面 | [semantic-web](https://github.com/insightos-community/semantic-web) | Semantic Studio，包含项目、机器人、工作流与仿真视图。 |
| 项目文档 | [semantic-docs](https://github.com/insightos-community/semantic-docs) | 系统架构、用户指南与组件开发文档。 |
| 机器人技能 | [robot-skill](https://github.com/insightos-community/robot-skill) | Robot Skill SDK，以及导航、抓取和放置技能。 |
| 机器人能力 | [r1pro-ability](https://github.com/insightos-community/r1pro-ability) | R1 Pro 导航、机械臂运动、传感器与感知等能力。 |
| 机器人接口 | [robot-sdk](https://github.com/insightos-community/robot-sdk) | 通用机器人接口、型号适配器和 Backend。 |
| 能力引擎 | [AbilityFramework](https://github.com/insightos-community/AbilityFramework) | Ability 实例运行与生命周期管理的原生运行时。 |
| 能力开发 | [Ability-SDK-Python](https://github.com/insightos-community/Ability-SDK-Python) | 用于实现和调用 Ability 的 Python SDK。 |
| 能力工具 | [ability-scaffold](https://github.com/insightos-community/ability-scaffold) | Ability 开发与打包工具。 |
| 运行依赖 | [ability-runtime](https://github.com/insightos-community/ability-runtime) | 机器人执行环境所需的依赖包和构建输入。 |
| 仿真运行时 | [mujoco-runtime](https://github.com/insightos-community/mujoco-runtime) | 原生 MuJoCo 运行时与场景生命周期管理。 |
| 场景资产 | [mujoco-asset](https://github.com/insightos-community/mujoco-asset) | 机器人模型、物体、布局与仿真资产。 |
| 机器人部署 | [semantic-deployment](https://github.com/insightos-community/semantic-deployment) | Robot Bundle 定义与部署配置。 |
| 安装入口 | [quick-start](https://github.com/insightos-community/quick-start) | 完整系统安装、制品打包与组件版本清单。 |

## Feature Roadmap

| 阶段 | Feature 主题 |
|---|---|
| **2026 Q3：系统基础与 Public Preview** | 语义系统基础、仿真资源模型、Ability/Skill 体系、语义世界模型、本地试用与基础示例 |
| **2026 Q4：开发能力** | 流程开发、Skill 开发、仿真场景开发，以及面向开发者的 SDK 和工具链 |
| **2027 Q1：高级智能** | 经验进化、Skill 持续优化、异常感知与处理、任务恢复和动态环境适应 |
| **2027 Q2：开放生态** | 场景应用、Skill、模型、插件和仿真资源生态，软件包分发与第三方开发者共建 |

## 文档与社区

- **[Quick Start](https://github.com/insightos-community/quick-start/blob/main/README.zh-CN.md)**：安装、运行管理与组件配套版本。
- **[系统架构](https://github.com/insightos-community/semantic-docs/tree/main/docs/architecture)**：项目、智能体、任务规划、执行链路与部署关系。
- **[用户手册](https://github.com/insightos-community/semantic-docs/tree/main/docs/user)**：Studio、环境配置与机器人任务操作。
- **[开发文档](https://github.com/insightos-community/semantic-docs/tree/main/docs/developer)**：核心模块、扩展接口与组件开发。
- **[Issues](https://github.com/insightos-community/Semantic-Framework/issues)**：反馈问题，交流使用场景与功能建议。

欢迎为核心服务、Studio、机器人技能、设备适配、场景和文档贡献代码与想法。你可以先通过 Issue 描述希望支持的任务，也可以直接向对应组件仓库提交 Pull Request，并附上可复现示例与适合该变更的验证结果。协作流程见[贡献指南](https://github.com/insightos-community/semantic-docs/blob/main/docs/developer/reference/contributing/_index.md)。

[CI 与 Tag 制品发布](docs/ci-release.md)

## 许可证

Copyright 2026 InsightOS。自有代码采用 [Apache-2.0](LICENSE) 许可证。第三方组件与资产的说明见 [NOTICE](NOTICE) 和 [LICENSE_SCOPE.md](LICENSE_SCOPE.md)。
