# Copyright 2026 InsightOS
# SPDX-License-Identifier: Apache-2.0
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""解析 v0.5 Gate 依赖的旁路仓库和 Ability 制品。

目录名同时兼容当前 $SEMANTIC 工作树和旧的平铺联调布局。
显式环境变量优先；指到不存在的路径时立即失败，不回退到个人机器路径。
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path


class WorkspaceError(RuntimeError):
    """Gate 工作树或制品路径无法解析。"""


# clone 目录名（$SEMANTIC 下）和旧联调目录名（旁边平铺）都认。
WORKSPACE_LAYOUTS = (
    {
        "sdk": Path("semantic-robotsdk/robot-sdk"),
        "ability": Path("semantic-ability/r1pro-ability"),
        "skills": Path("semantic-skill/robot-skill"),
        "deployment": Path("semantic-robot-deployment"),
    },
    {
        "sdk": Path("semantic-robot-sdk"),
        "ability": Path("semantic-ability"),
        "skills": Path("semantic-robot-skills"),
        "deployment": Path("semantic-robot-deployment"),
    },
)
VENDOR_LAYOUTS = (
    Path("semantic-ability/ability-runtime"),
    Path("ability-runtime"),
)
WEB_LAYOUTS = (
    Path("semantic-web"),
    Path(".worktrees/semantic-web-v050-devices"),
)
ASSET_LAYOUTS = (
    Path("semantic-scene/mujoco-asset"),
    Path(".worktrees/mujoco-asset-v060-tote-gripper"),
)
MUJOCO_LAYOUTS = (
    Path("semantic-simulation/mujoco-runtime"),
    Path(".worktrees/mujoco-runtime-v060-tote-gripper"),
)
ABILITY_PY_WHEEL_NAME = "ability_py-0.4.0-py3-none-any.whl"


@dataclass(frozen=True)
class GateLayout:
    """一次 Gate 运行所需的旁路仓库和 Ability 制品。"""

    root: Path
    sdk: Path
    ability: Path
    skills: Path
    deployment: Path
    web: Path
    vendor: Path
    ability_framework: Path
    ability_py_wheel: Path
    ability_scaffold: Path


def _unique_paths(paths: list[Path]) -> list[Path]:
    seen: set[Path] = set()
    ordered: list[Path] = []
    for path in paths:
        resolved = path.expanduser().resolve()
        if resolved in seen:
            continue
        seen.add(resolved)
        ordered.append(resolved)
    return ordered


def env_path(name: str) -> Path | None:
    value = os.environ.get(name, "").strip()
    if not value:
        return None
    return Path(value).expanduser().resolve()


def workspace_roots(framework: Path) -> list[Path]:
    roots: list[Path] = []
    semantic = os.environ.get("SEMANTIC", "").strip()
    if semantic:
        roots.append(Path(semantic))
    roots.append(framework.parent)
    if len(framework.parents) > 1:
        roots.append(framework.parents[1])
    return _unique_paths(roots)


def repository_complete(candidate: dict[str, Path]) -> bool:
    return (
        (candidate["sdk"] / "Makefile").is_file()
        and (candidate["ability"] / "abilities").is_dir()
        and (candidate["skills"] / "semantic_robot_skills/skills").is_dir()
        and (candidate["deployment"] / "type-packages").is_dir()
    )


def _override_or_find(
    env_name: str,
    roots: list[Path],
    relatives: tuple[Path, ...],
    predicate,
    missing: str,
) -> Path:
    override = env_path(env_name)
    if override is not None:
        if not predicate(override):
            raise WorkspaceError(f"{env_name} 指向的路径不可用：{override}")
        return override
    for root in roots:
        for relative in relatives:
            candidate = (root / relative).resolve()
            if predicate(candidate):
                return candidate
    raise WorkspaceError(missing)


def resolve_repositories(framework: Path) -> dict[str, Path]:
    roots = workspace_roots(framework)
    tried: list[str] = []
    detected: dict[str, Path] | None = None
    detected_root = roots[0]
    for root in roots:
        for layout in WORKSPACE_LAYOUTS:
            candidate = {
                name: (root / relative).resolve()
                for name, relative in layout.items()
            }
            if repository_complete(candidate):
                detected = candidate
                detected_root = root
                break
            tried.append(str(root))
        if detected is not None:
            break

    overrides = {
        "sdk": env_path("SEMANTIC_SDK_REPO"),
        "ability": env_path("SEMANTIC_ABILITY_REPO"),
        "skills": env_path("SEMANTIC_SKILLS_REPO"),
        "deployment": env_path("SEMANTIC_DEPLOYMENT_REPO"),
    }
    if detected is None and not any(path is not None for path in overrides.values()):
        raise WorkspaceError(
            "找不到旁边的 SDK / Ability / Skill / Deployment 仓。"
            "把它们放在 $SEMANTIC 下，或分别设置 SEMANTIC_SDK_REPO、"
            "SEMANTIC_ABILITY_REPO、SEMANTIC_SKILLS_REPO、SEMANTIC_DEPLOYMENT_REPO。"
            f" 已尝试: {tried}"
        )

    merged = dict(detected or {})
    for name, path in overrides.items():
        if path is not None:
            merged[name] = path
    if not repository_complete(merged):
        raise WorkspaceError(
            "SDK / Ability / Skill / Deployment 仓不完整："
            + ", ".join(f"{name}={merged.get(name, '<missing>')}" for name in overrides)
        )
    return {"framework": framework, "root": detected_root, **merged}


def resolve_vendor_root(framework: Path) -> Path:
    return _override_or_find(
        "SEMANTIC_ABILITY_VENDOR_ROOT",
        workspace_roots(framework),
        VENDOR_LAYOUTS,
        lambda path: (path / "AbilityFramework").is_file(),
        "未找到 AbilityFramework。把 ability-runtime 放在 "
        "$SEMANTIC/semantic-ability 下，或设置 SEMANTIC_ABILITY_VENDOR_ROOT。",
    )


def resolve_web_repo(framework: Path) -> Path:
    return _override_or_find(
        "SEMANTIC_WEB_REPO",
        workspace_roots(framework),
        WEB_LAYOUTS,
        lambda path: (path / "package.json").is_file(),
        "找不到 semantic-web。设置 SEMANTIC_WEB_REPO，或把它放在 $SEMANTIC 下。",
    )


def resolve_mujoco_asset(framework: Path) -> Path:
    return _override_or_find(
        "SEMANTIC_MUJOCO_ASSET_REPO",
        workspace_roots(framework),
        ASSET_LAYOUTS,
        lambda path: (path / "prototypes" / "r1pro-tote-gripper").exists(),
        "找不到 MuJoCo 资产仓。设置 SEMANTIC_MUJOCO_ASSET_REPO。",
    )


def resolve_mujoco_runtime(framework: Path) -> Path:
    return _override_or_find(
        "SEMANTIC_MUJOCO_RUNTIME_REPO",
        workspace_roots(framework),
        MUJOCO_LAYOUTS,
        lambda path: (path / "pyproject.toml").is_file(),
        "找不到 MuJoCo Runtime 仓。设置 SEMANTIC_MUJOCO_RUNTIME_REPO。",
    )


def find_ability_framework(vendor: Path) -> Path:
    override = env_path("SEMANTIC_ABILITY_FRAMEWORK_BIN")
    if override is not None:
        if not override.is_file():
            raise WorkspaceError(
                f"SEMANTIC_ABILITY_FRAMEWORK_BIN 不是文件：{override}"
            )
        return override
    path = vendor / "AbilityFramework"
    if not path.is_file():
        raise WorkspaceError(f"缺少 AbilityFramework：{path}")
    return path


def find_ability_py_wheel(vendor: Path) -> Path:
    override = env_path("SEMANTIC_ABILITY_PY_WHEEL")
    if override is not None:
        if not override.is_file():
            raise WorkspaceError(f"SEMANTIC_ABILITY_PY_WHEEL 不是文件：{override}")
        return override
    search = (
        vendor / ABILITY_PY_WHEEL_NAME,
        vendor / "requirments" / ABILITY_PY_WHEEL_NAME,
        vendor / "requirements" / ABILITY_PY_WHEEL_NAME,
        vendor / "wheels" / ABILITY_PY_WHEEL_NAME,
        vendor / "base-bundles" / "r1pro-mujoco-0.5.0-dev" / "wheels" / ABILITY_PY_WHEEL_NAME,
    )
    for path in search:
        if path.is_file():
            return path
    raise WorkspaceError(
        "找不到 ability_py-0.4.0 Wheel。"
        "它应在 ability-runtime 根目录或 wheels/ 下，也可设置 SEMANTIC_ABILITY_PY_WHEEL。"
    )


def find_ability_scaffold(vendor: Path) -> Path:
    override = env_path("SEMANTIC_ABILITY_SCAFFOLD")
    if override is not None:
        if not override.is_file():
            raise WorkspaceError(f"SEMANTIC_ABILITY_SCAFFOLD 不是文件：{override}")
        return override
    path = vendor / ".venv" / "bin" / "ability-scaffold"
    if not path.is_file():
        raise WorkspaceError(
            f"缺少 ability-scaffold：{path}。"
            "在 ability-runtime 执行 make setup，或设置 SEMANTIC_ABILITY_SCAFFOLD。"
        )
    return path


def resolve_gate_layout(framework: Path) -> GateLayout:
    repos = resolve_repositories(framework)
    vendor = resolve_vendor_root(framework)
    return GateLayout(
        root=repos["root"],
        sdk=repos["sdk"],
        ability=repos["ability"],
        skills=repos["skills"],
        deployment=repos["deployment"],
        web=resolve_web_repo(framework),
        vendor=vendor,
        ability_framework=find_ability_framework(vendor),
        ability_py_wheel=find_ability_py_wheel(vendor),
        ability_scaffold=find_ability_scaffold(vendor),
    )
