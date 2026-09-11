<div align="center">
  <img src="docs/assets/logo.png" alt="InsightOS Semantic" width="100%">

  <h3>An open framework for embodied AI applications</h3>

  <p>
    <a href="https://github.com/insightos-community/Semantic-Framework/stargazers"><img src="https://img.shields.io/github/stars/insightos-community/Semantic-Framework?style=social" alt="GitHub stars"></a>
    <a href="LICENSE"><img src="https://img.shields.io/badge/License-Apache--2.0-blue" alt="Apache-2.0 license"></a>
    <img src="https://img.shields.io/badge/Version-0.5.0--dev-14796b" alt="Version 0.5.0 development">
    <a href="https://semantic.insightos.cn/"><img src="https://img.shields.io/badge/Website-Semantic-3268d8" alt="Semantic website"></a>
  </p>

  <p>
    <b>English</b> · <a href="README.zh-CN.md">简体中文</a><br>
    <a href="#quick-start">Quick start</a> · <a href="#system-architecture">System architecture</a> · <a href="#framework-development">Framework development</a> · <a href="#ecosystem">Ecosystem</a> · <a href="https://github.com/insightos-community/semantic-docs/tree/main/docs">Documentation</a> · <a href="https://github.com/insightos-community/Semantic-Framework/issues">Issues</a>
  </p>
</div>

---

InsightOS Semantic is an embodied semantic agent system created for real-world robotic applications. It connects robot embodiments, environments, tasks, Skills, and operational experience through semantics, turning natural-language requests into executable robot tasks across the complete **understand–plan–execute–evolve** loop.

InsightOS Semantic supports scenarios including warehouse picking and delivery, depalletizing and palletizing, multi-robot production lines, and scheduled inspection, and connects humanoids, quadrupeds, manipulators, mobile robots, and other robot embodiments.

This is the **product home and Framework implementation for InsightOS Semantic**. `semantic-server` manages projects, tasks, registries, and runtime coordination; `semantic-pilot` connects robot execution environments; the `semantic` CLI provides configuration and runtime management. The complete system also includes Studio, robot skills, and simulation components maintained in separate repositories. Visit [InsightOS Community](https://github.com/insightos-community) for the organization and ecosystem overview.

## Why Semantic?

Moving from demonstrations to real work presents robots with three challenges:

- **Understanding intent:** hard-coded tasks struggle to capture human goals and scene constraints.
- **Adapting to change:** rigid workflows falter as environments, objects, and execution states change.
- **Learning from experience:** lessons from execution rarely transfer across robots and scenarios, leading to repeated development.

InsightOS Semantic uses shared semantics across tasks, environments, embodiments, and abilities so robots can understand goals, compose Skills, execute work, and turn experience into reusable capabilities.

## System Architecture

The system has four parts: application interaction, task orchestration, robot execution, and environments and devices. Task commands and execution feedback connect them.

<p align="center">
  <a href="docs/assets/architecture-en.svg"><img src="docs/assets/architecture-en.svg" alt="InsightOS Semantic system architecture" width="720"></a>
</p>

| Layer | Main modules | Responsibility |
|:--|:--|:--|
| **Application interaction** | Semantic Studio, model providers | Users describe goals, review plans, and inspect results in Studio. Model providers supply understanding and reasoning for agents. |
| **Task orchestration** | Semantic Server | Leader interprets goals and the environment, Workflow organizes tasks and dependencies, and Robot Agent selects skills and arranges subtasks. |
| **Robot execution** | Pilot, Robot Skill, AbilityFramework, Robot SDK | Robot Skill organizes operation stages, Pilot dispatches actions, AbilityFramework runs capability instances, and Robot SDK connects devices and returns observations and results. |
| **Environments and devices** | MuJoCo Runtime, physical robots | MuJoCo provides scenes and physics simulation. Physical robots connect through adapters and hardware interfaces to perform actions and return feedback. |

## Core Features

- **Natural-language task interaction:** describe a goal in one sentence and let the system organize tasks and Skills.
- **Multi-robot collaboration:** assign work across heterogeneous robots and coordinate the workflow.
- **Semantic world model:** represent what objects are, where they are, and how they relate.
- **Dynamic-environment adaptation:** adjust task execution as the environment and execution state change.
- **Simulation-driven development:** connect scenes, robots, Skills, and tasks for rapid simulation validation.
- **Experience and capability evolution:** turn execution data and experience into reusable, continuously improving Skills.
- **Embodied application ecosystem:** reuse scenario applications, Skills, models, plugins, adapters, and simulation assets.

## Quick start

### 1. Install the binary package

On **Linux x86_64**, prepare Bash, curl, and Python 3.10+, then run:

```bash
curl -fsSL https://semantic.insightos.cn/install-en.sh | bash -s -- --install-system-deps
```

The installer prepares Server, Studio, native MuJoCo, the R1 Pro Robot Bundle, scene assets, and the navigation, grasp, and placement skills. It installs to `$HOME/.local/share/semantic` and starts the services. `--install-system-deps` enables installation of the required system libraries through the host's package manager.

### 2. Open Studio

Open **http://localhost:3000** and sign in as **`admin`** using the generated password shown by the installer. To display the login details again and check service status:

```bash
export SEMANTIC_HOME="$HOME/.local/share/semantic"
export PATH="$SEMANTIC_HOME/bin:$PATH"
semanticctl welcome
semanticctl status
```

From another device on the same network, open `http://<server-ip>:3000`. The web gateway listens on `0.0.0.0:3000` by default. Use `semanticctl stop` and `semanticctl start` to stop and start the installation's services.

In Studio's **System Settings**, configure a model with tool-calling support, then create a project and start a scene. Follow [Your first Project](https://github.com/insightos-community/semantic-docs/blob/main/docs/user/getting-started/first-project.md) to run a robot task.

### Detailed installation guides

Continue with **[Quick Start (English)](https://github.com/insightos-community/quick-start/blob/main/README.md)** for the installation documentation and source build workflow. Custom directories and ports, service management, and other binary installation options are covered in the **[binary installer reference (Chinese)](https://github.com/insightos-community/quick-start/blob/main/artifacts/README.md)**. Compatible component revisions are recorded in the [version manifest](https://github.com/insightos-community/quick-start/blob/main/repo-versions.json).

## Framework development

### Repository structure

- `cmd/` — Server, Pilot, and CLI entry points.
- `internal/` · `pkg/` — orchestration, services, contracts, and shared libraries.
- `configs/` — configuration templates; `scripts/` — build and bundle workflows.
- `tests/` · `ci/` — unit/integration gates and build automation.

### Build and run

Requires Go **1.23+**, Make, and a Linux development environment.

```bash
make build
make init
go test ./...
make run
```

`make build` produces `.output/bin/semantic-server`, `semantic-pilot`, and `semantic`. Initialization writes configuration under `.output/configs/`. Set `SEMANTIC_ADMIN_PASSWORD` in a local environment or untracked `.env` before initialization; do not publish passwords.

Edit the generated `.output/configs/semantic-server.yaml` for your instance rather than changing shared templates. Default development endpoints are HTTP `127.0.0.1:8080` and WebSocket `127.0.0.1:8081`; the Studio is a separate project.

### Use the outputs

A server build alone does not provision a Robot. For the complete stack, follow quick-start's workspace layout and pinned manifest: register native MuJoCo, build AbilityFramework/SDK Wheels, then assemble and activate a Robot Bundle.

From this repository, after those inputs exist:

```bash
python3 scripts/refresh_v050_mujoco.py --help
```

Use its `build` workflow with the Robot Python **3.13** interpreter. Publish the required Skill packages through its separate `publish` workflow after Server starts. Stop affected scenes and Robots before replacing active bundles.

### Troubleshooting

- Missing or offline Robot: verify Pilot connectivity, active catalog, Runtime registration, and published Skill versions.
- Configuration changes do not apply: check the generated configuration used by the running process.
- If a hot-reload hook fails, previously applied sections are restored to their old values. A rollback failure is reported explicitly and the settings snapshot retains the last successfully applied value for that section. Correct the invalid configuration and retry; the configuration file itself is not rolled back.
- Unit tests do not cover the entire product: simulation gates require assets and sibling repositories; model-provider gates can make billable calls.
- Bind development ports to trusted interfaces and keep tokens out of commits.

[Historical technical reference](README.reference.md) · [Configuration templates](configs/) · [Build workflows](scripts/)

## Ecosystem

Semantic is developed across repositories that share one task and execution model. Start here for the product overview; use the component repositories when working on a specific layer.

| Layer | Repository | Responsibility |
|:--|:--|:--|
| Semantic framework | **[semantic-framework](https://github.com/insightos-community/Semantic-Framework)** · this repository | Server, Pilot, CLI, agents, orchestration, and shared contracts. |
| Interface | [semantic-web](https://github.com/insightos-community/semantic-web) | Semantic Studio, including project, robot, workflow, and simulation views. |
| Documentation | [semantic-docs](https://github.com/insightos-community/semantic-docs) | Architecture, user guides, and component development documentation. |
| Robot skills | [robot-skill](https://github.com/insightos-community/robot-skill) | Robot Skill SDK and navigation, grasp, and placement implementations. |
| Robot abilities | [r1pro-ability](https://github.com/insightos-community/r1pro-ability) | R1 Pro navigation, manipulation, sensing, and perception capabilities. |
| Robot interface | [robot-sdk](https://github.com/insightos-community/robot-sdk) | Shared robot interfaces, model adapters, and backends. |
| Ability engine | [AbilityFramework](https://github.com/insightos-community/AbilityFramework) | Native runtime for Ability instances and their lifecycle. |
| Ability development | [Ability-SDK-Python](https://github.com/insightos-community/Ability-SDK-Python) | Python SDK for implementing and interacting with Abilities. |
| Ability tooling | [ability-scaffold](https://github.com/insightos-community/ability-scaffold) | Ability development and packaging tools. |
| Runtime dependencies | [ability-runtime](https://github.com/insightos-community/ability-runtime) | Dependency packages and build inputs for the robot execution environment. |
| Simulation | [mujoco-runtime](https://github.com/insightos-community/mujoco-runtime) | Native MuJoCo runtime and scene lifecycle. |
| Scene assets | [mujoco-asset](https://github.com/insightos-community/mujoco-asset) | Robot models, objects, layouts, and simulation assets. |
| Robot deployment | [semantic-deployment](https://github.com/insightos-community/semantic-deployment) | Robot Bundle definitions and deployment configuration. |
| Installation | [quick-start](https://github.com/insightos-community/quick-start) | Complete-system installation, artifact packaging, and the component version manifest. |

## Feature Roadmap

| Phase | Feature theme |
| --- | --- |
| **2026 Q3: System Foundation and Public Preview** | Semantic system foundation, simulation resource model, Ability/Skill system, semantic world model, local trial, and starter examples |
| **2026 Q4: Development Capabilities** | Workflow development, Skill development, simulation-scene development, and developer SDKs and toolchains |
| **2027 Q1: Advanced Intelligence** | Experience evolution, continuous Skill improvement, anomaly awareness and handling, task recovery, and dynamic-environment adaptation |
| **2027 Q2: Open Ecosystem** | Scenario applications, Skills, models, plugins, simulation resources, package distribution, and third-party developer collaboration |

## Documentation and community

- **[Quick Start](https://github.com/insightos-community/quick-start)** — installation, service management, and compatible component versions.
- **[System architecture](https://github.com/insightos-community/semantic-docs/tree/main/docs/architecture)** — projects, agents, planning, execution, and deployment.
- **[User guides](https://github.com/insightos-community/semantic-docs/tree/main/docs/user)** — Studio, environment setup, and robot task operation.
- **[Developer documentation](https://github.com/insightos-community/semantic-docs/tree/main/docs/developer)** — core modules, extension interfaces, and component development.
- **[Issues](https://github.com/insightos-community/Semantic-Framework/issues)** — report a problem or discuss a feature with the project.

Contributions to the core, Studio, skills, robot adapters, scenes, and documentation are welcome. Open an issue with the task you want to support, or submit a pull request to the relevant component repository. Include a reproducible example and validation appropriate to the change. See the [contributor guide](https://github.com/insightos-community/semantic-docs/blob/main/docs/developer/reference/contributing/_index.md) for the broader workflow.

[CI and Tag releases](docs/ci-release.md)

## License

Copyright 2026 InsightOS. First-party code is licensed under [Apache-2.0](LICENSE). See [NOTICE](NOTICE) and [LICENSE_SCOPE.md](LICENSE_SCOPE.md) for third-party components and assets.
