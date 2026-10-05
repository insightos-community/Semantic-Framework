#!/usr/bin/env python3
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

"""构建、激活并发布 v0.5 MuJoCo Robot 开发制品。"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import signal
import stat
import subprocess
import sys
import time
import tomllib
import urllib.error
import urllib.parse
import urllib.request
import zipfile
from pathlib import Path

ABILITY_PROJECTS = (
    "r1pro-navigation",
    "r1pro-manipulator-motion",
    "r1pro-end-effector",
    "r1pro-robot-state",
    "r1pro-sensor-capture",
    "r1pro-object-perception",
    "r1pro-grasp-planning",
)
SKILL_PROJECTS = {
    "grasp-object": "grasp_object",
    "semantic-navigation": "semantic_navigation",
    "place-object": "place_object",
}
AGENT_SKILL_PROJECTS = (
    Path("workflow/depalletizing-workflow-planning"),
    Path("robot/depalletizing-robot-task"),
)
AGENT_PROFILES = ("leader", "robot")
BUNDLE_NAME = "r1pro-mujoco"


class RefreshError(RuntimeError):
    pass


def run(
    command: list[str],
    cwd: Path,
    env: dict[str, str] | None = None,
) -> None:
    print("+", " ".join(command), f"(cwd={cwd})", flush=True)
    subprocess.run(command, cwd=cwd, check=True, env=env)


def require_file(path: Path, label: str) -> Path:
    if not path.is_file():
        raise RefreshError(f"缺少{label}: {path}")
    return path


def single_file(root: Path, pattern: str, label: str) -> Path:
    matches = sorted(path for path in root.glob(pattern) if path.is_file())
    if len(matches) != 1:
        raise RefreshError(f"{label}应当精确匹配一个文件，实际为: {matches}")
    return matches[0]


def bundle_name(manifest: Path) -> str:
    content = manifest.read_text(encoding="utf-8")
    metadata = re.search(r"(?ms)^metadata:\s*\n(?P<body>(?:^[ \t]+.*\n?)+)", content)
    if not metadata:
        return ""
    name = re.search(r"(?m)^\s+name:\s*([^#\s]+)", metadata.group("body"))
    return name.group(1).strip("'\"") if name else ""


def find_product_bundles(catalog: Path) -> list[Path]:
    if not catalog.is_dir():
        return []
    return [
        manifest.parent.resolve()
        for manifest in sorted(catalog.glob("*/bundle.yaml"))
        if bundle_name(manifest) == BUNDLE_NAME
    ]


def make_tree_directories_writable(path: Path) -> None:
    for current, _, _ in os.walk(path):
        current_path = Path(current)
        mode = current_path.stat().st_mode
        os.chmod(current_path, mode | stat.S_IRUSR | stat.S_IWUSR | stat.S_IXUSR)


def make_tree_directories_readonly(path: Path) -> None:
    for current, _, _ in os.walk(path):
        current_path = Path(current)
        mode = current_path.stat().st_mode
        os.chmod(current_path, mode & ~stat.S_IWUSR & ~stat.S_IWGRP & ~stat.S_IWOTH)


def safe_recreate(path: Path, output_root: Path) -> None:
    resolved = path.resolve()
    if output_root.resolve() not in resolved.parents:
        raise RefreshError(f"拒绝清理 .output 之外的目录: {resolved}")
    if resolved.exists():
        # Bundle 构建器会把产物目录设为只读，防止运行实例修改共享制品。
        # 开发刷新只在已经通过 .output 边界检查的暂存目录内恢复目录写权限；
        # 不修改文件内容，也不触碰正在使用的 Catalog Bundle。
        make_tree_directories_writable(resolved)
        shutil.rmtree(resolved)
    resolved.mkdir(parents=True, exist_ok=True)


def zip_skill(source: Path, target: Path) -> str:
    manifest = require_file(source / "SKILL.md", "Robot Skill SKILL.md")
    match = re.search(
        r"(?m)^version:\s*([^#\s]+)", manifest.read_text(encoding="utf-8")
    )
    if not match:
        raise RefreshError(f"Robot Skill 缺少 version: {manifest}")
    version = match.group(1).strip("'\"")
    with zipfile.ZipFile(target, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        for path in sorted(source.rglob("*")):
            if not path.is_file():
                continue
            relative = path.relative_to(source)
            if "__pycache__" in relative.parts or path.suffix in {".pyc", ".orig"}:
                continue
            archive.write(path, relative.as_posix())
    return version


def iter_bundle_users(bundle_roots: list[Path]) -> list[tuple[int, str, str]]:
    """只收集将被替换的 Bundle 进程，不猜测 Server 或系统进程身份。"""
    roots = [str(path.resolve()) + os.sep for path in bundle_roots]
    found: list[tuple[int, str, str]] = []
    if not Path("/proc").is_dir() or not roots:
        return found
    skip = {os.getpid(), os.getppid(), 1}
    for entry in Path("/proc").iterdir():
        if not entry.name.isdigit() or int(entry.name) in skip:
            continue
        try:
            executable = str((entry / "exe").resolve(strict=True))
            command = (
                (entry / "cmdline")
                .read_bytes()
                .replace(b"\0", b" ")
                .decode("utf-8", errors="replace")
            )
        except (FileNotFoundError, PermissionError, ProcessLookupError):
            continue
        if any(executable.startswith(root) or root in command for root in roots):
            found.append((int(entry.name), executable, command.strip()))
    return found


def process_using_bundle(bundle_roots: list[Path]) -> list[str]:
    return [f"pid={pid} exe={exe} cmd={cmd}" for pid, exe, cmd in iter_bundle_users(bundle_roots)]


def _signal_tree(pid: int, sig: int) -> None:
    try:
        pgid = os.getpgid(pid)
        mine = os.getpgid(0)
    except OSError:
        return
    try:
        if pgid not in (0, 1, mine):
            os.killpg(pgid, sig)
        else:
            os.kill(pid, sig)
    except ProcessLookupError:
        return
    except PermissionError:
        try:
            os.kill(pid, sig)
        except (ProcessLookupError, PermissionError):
            return


def stop_processes_using_bundle(bundle_roots: list[Path], timeout: float = 15) -> None:
    users = iter_bundle_users(bundle_roots)
    if not users:
        return
    print("refresh-v050-mujoco: 停止占用旧 Bundle 的进程：", flush=True)
    for pid, exe, cmd in users:
        print(f"  pid={pid} exe={exe} cmd={cmd}", flush=True)
        _signal_tree(pid, signal.SIGTERM)
    deadline = time.time() + timeout
    while time.time() < deadline:
        if not iter_bundle_users(bundle_roots):
            print("refresh-v050-mujoco: 旧 Bundle 进程已退出", flush=True)
            return
        time.sleep(0.2)
    leftover = iter_bundle_users(bundle_roots)
    for pid, _, _ in leftover:
        _signal_tree(pid, signal.SIGKILL)
    time.sleep(0.3)
    leftover = iter_bundle_users(bundle_roots)
    if leftover:
        details = "\n".join(f"  pid={pid} exe={exe} cmd={cmd}" for pid, exe, cmd in leftover)
        raise RefreshError(f"停止后仍有进程占用旧 MuJoCo Bundle：\n{details}")
    print("refresh-v050-mujoco: 旧 Bundle 进程已强制退出", flush=True)


def activate_bundle(
    staged: Path, catalog: Path, active: list[Path], *, stop_users: bool = False
) -> Path:
    if stop_users:
        stop_processes_using_bundle(active)
    running = process_using_bundle(active)
    if running:
        details = "\n".join(f"  {item}" for item in running)
        raise RefreshError(
            "仍有进程使用旧 MuJoCo Bundle。请先在 Web 停止场景并关闭 Server，"
            f"确认实例退出后重试：\n{details}"
        )
    archive_root = catalog.parent / "robot-bundle-archive"
    archive_root.mkdir(parents=True, exist_ok=True)
    stamp = time.strftime("%Y%m%d-%H%M%S")
    for index, old in enumerate(active, start=1):
        make_tree_directories_writable(old)
        archived = archive_root / f"{old.name}-{stamp}-{index}"
        old.rename(archived)
        make_tree_directories_readonly(archived)
    destination = catalog / "r1pro-mujoco-0.5.0-dev"
    if destination.exists():
        raise RefreshError(f"激活目标仍然存在: {destination}")
    # Bundle 构建完成后目录是只读的。某些受限运行环境会因此拒绝目录换名，
    # 所以只在 Catalog 原子切换期间恢复目录写权限，切换后立即重新锁为只读。
    make_tree_directories_writable(staged)
    staged.rename(destination)
    make_tree_directories_readonly(destination)
    return destination


def sync_agent_skills(framework: Path) -> None:
    """把本轮 Project 可绑定的只读 Skill同步到开发 Server 配置副本。"""
    source_root = framework / "configs/skills"
    destination_root = framework / ".output/configs/skills"
    for relative in AGENT_SKILL_PROJECTS:
        source = source_root / relative
        require_file(source / "SKILL.md", "拆码垛 Agent Skill")
        destination = destination_root / relative
        if destination.exists():
            shutil.rmtree(destination)
        destination.parent.mkdir(parents=True, exist_ok=True)
        shutil.copytree(source, destination)


def sync_agent_profiles(framework: Path) -> None:
    """同步本轮真实执行会使用的 Profile，避免 Server继续读取旧 allowlist。"""
    source_root = framework / "configs/agents"
    destination_root = framework / ".output/configs/agents"
    for name in AGENT_PROFILES:
        source = source_root / name
        require_file(source / "role.yaml", f"{name} Agent Profile")
        require_file(source / "AGENT.md", f"{name} Agent 指令")
        destination = destination_root / name
        if destination.exists():
            shutil.rmtree(destination)
        shutil.copytree(source, destination)


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


def workspace_roots(framework: Path) -> list[Path]:
    roots = []
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
        and (candidate["deployment"] / "type-packages/r1pro-mujoco").is_dir()
    )


def repositories(framework: Path) -> dict[str, Path]:
    tried: list[str] = []
    for root in workspace_roots(framework):
        for layout in WORKSPACE_LAYOUTS:
            candidate = {
                name: (root / relative).resolve()
                for name, relative in layout.items()
            }
            if repository_complete(candidate):
                return {"framework": framework, **candidate}
            tried.append(str(root))
    raise RefreshError(
        "找不到旁边的 SDK / Ability / Skill / Deployment 仓。"
        "把它们放在 $SEMANTIC 下，或 export SEMANTIC=工作根目录。"
        f" 已尝试: {tried}"
    )


VENDOR_LAYOUTS = (
    Path("semantic-ability/ability-runtime"),
    Path("ability-runtime"),
    Path("mcp-playground"),
)


def resolve_vendor_root(arguments: argparse.Namespace, framework: Path) -> Path:
    if arguments.vendor_root:
        return Path(arguments.vendor_root).expanduser().resolve()
    env = os.environ.get("SEMANTIC_ABILITY_VENDOR_ROOT", "").strip()
    if env:
        return Path(env).expanduser().resolve()
    for root in workspace_roots(framework):
        for relative in VENDOR_LAYOUTS:
            candidate = root / relative
            if (candidate / "AbilityFramework").is_file():
                return candidate.resolve()
    raise RefreshError(
        "未找到 AbilityFramework。"
        "把 ability-runtime 放在 $SEMANTIC/semantic-ability 下，或传 --vendor-root"
    )


def ensure_vendor_scaffold(vendor: Path, python: str) -> Path:
    """没有 .venv 时，用仓里的 scaffold Wheel 现装。"""
    scaffold = vendor / ".venv/bin/ability-scaffold"
    if scaffold.is_file():
        return scaffold
    wheel = vendor / "ability_scaffold-1.2.0-py3-none-any.whl"
    require_file(wheel, "ability_scaffold Wheel")
    run([python, "-m", "venv", str(vendor / ".venv")], vendor)
    pip = [str(vendor / ".venv/bin/python"), "-m", "pip"]
    run([*pip, "install", "-U", "pip"], vendor)
    run([*pip, "install", str(wheel)], vendor)
    return require_file(scaffold, "ability-scaffold")


def find_ability_py_wheel(vendor: Path, wheel_cache: Path) -> Path:
    names = ("ability_py-0.4.0-py3-none-any.whl",)
    search = (
        vendor,
        vendor / "requirments",
        wheel_cache,
    )
    for directory in search:
        for name in names:
            path = directory / name
            if path.is_file():
                return path.resolve()
    raise RefreshError(
        "找不到 ability_py-0.4.0 Wheel。"
        "它应在 ability-runtime 根目录，或 Wheel 缓存里。"
    )


def prepare_ability_test_env(
    ability: Path,
    python: str,
    ability_py: Path,
    wheel_cache: Path,
) -> Path:
    """在 Ability 仓建 .venv，装单测需要的 ability_py / PyYAML / pydantic。"""
    venv = ability / ".venv"
    venv_python = venv / "bin/python"
    run([python, "-m", "venv", str(venv)], ability)
    pip = [str(venv_python), "-m", "pip"]
    run([*pip, "install", "-U", "pip"], ability)
    yaml_deps = ["PyYAML>=6,<7", "pydantic>=2.8,<3"]
    if wheel_cache.is_dir():
        run(
            [*pip, "install", "--no-index", "--find-links", str(wheel_cache), *yaml_deps],
            ability,
        )
    else:
        run([*pip, "install", *yaml_deps], ability)
    run([*pip, "install", str(ability_py)], ability)
    return venv_python


# 本轮刷新会现打这些 Wheel，离线缓存不必自带。
REFRESH_WHEEL_PREFIXES = (
    "semantic_robot_sdk_core-",
    "semantic_robot_sdk_r1pro-",
    "semantic_r1pro_abilities-",
    "semantic_robot_skill_sdk-",
)


def ability_build_version(ability: Path, type_package: Path) -> str:
    """源码版本必须与类型包声明一致，不从 dist 或缓存猜测版本。"""
    project = tomllib.loads((ability / "pyproject.toml").read_text(encoding="utf-8"))
    version = str(project["project"]["version"])
    expected = f"semantic_r1pro_abilities-{version}-py3-none-any.whl"
    declared = re.findall(
        r"wheels/(semantic_r1pro_abilities-[A-Za-z0-9_.+-]+\.whl)",
        (type_package / "bundle.yaml").read_text(encoding="utf-8"),
    )
    if declared != [expected]:
        raise RefreshError(
            f"Ability 源码版本为 {version}，但 Bundle 声明为 {declared}；"
            f"请同步 Deployment 清单为 wheels/{expected} 后重新构建"
        )
    return version


def build_ability_wheel(
    ability: Path, python: str, wheels_dir: Path, version: str,
    test_env: dict[str, str],
) -> Path:
    run(["make", "test"], ability, env=test_env)
    run(
        ["uv", "build", "--wheel", "--python", python, "--out-dir", str(wheels_dir)],
        ability,
    )
    # wheels_dir 属于本次清空后创建的 build 目录；绝不从仓库 dist 复用旧包。
    return single_file(
        wheels_dir, f"semantic_r1pro_abilities-{version}-*.whl", "本次 Ability Wheel"
    )


def verify_bundle_ability(bundle: Path, wheel: Path, expected_version: str) -> dict:
    # Metadata alone does not load Pinocchio's native dependencies. Validate the
    # isolated installed runtime before activation, without host library paths.
    environment = {key: value for key, value in os.environ.items()
                   if key not in {"LD_LIBRARY_PATH", "PYTHONPATH", "PYTHONHOME"}}
    check = (
        "import sys; from pathlib import Path; from importlib.metadata import version; "
        "native = Path(sys.prefix) / 'lib' / f'python{sys.version_info.major}.{sys.version_info.minor}' "
        "/ 'site-packages/cmeel.prefix/lib'; "
        "assert (native / 'libtinyxml2.so.9').is_file(), 'Bundle 缺少 libtinyxml2.so.9'; "
        "import pinocchio; print(version('semantic-r1pro-abilities'))"
    )
    try:
        installed = subprocess.check_output(
            [str(bundle / "python/venv/bin/python"), "-I", "-c", check],
            text=True, stderr=subprocess.STDOUT, env=environment,
        ).strip()
    except subprocess.CalledProcessError as error:
        raise RefreshError(f"Bundle 原生依赖加载失败，未激活：{error.output.strip()}") from error
    if installed != expected_version:
        raise RefreshError(
            f"Bundle 内 Ability 实现版本为 {installed}，预期 {expected_version}；未激活"
        )
    return {
        "version": installed,
        "wheel": wheel.name,
        "sha256": hashlib.sha256(wheel.read_bytes()).hexdigest(),
    }


def required_cache_wheels(type_package: Path) -> list[str]:
    """类型包清单里必须从缓存提供的第三方 Wheel。"""
    manifest = type_package / "bundle.yaml"
    if not manifest.is_file():
        return []
    names = re.findall(r"wheels/([A-Za-z0-9_.+-]+\.whl)", manifest.read_text(encoding="utf-8"))
    return [
        name
        for name in names
        if not name.startswith(REFRESH_WHEEL_PREFIXES)
    ]


def missing_cache_wheels(bundle: Path, required: list[str]) -> list[str]:
    wheels = bundle / "wheels"
    if not (bundle / "bundle.yaml").is_file() or not wheels.is_dir():
        return required or ["bundle.yaml"]
    return [name for name in required if not (wheels / name).is_file()]


def resolve_base_bundle(
    arguments: argparse.Namespace,
    framework: Path,
    output_root: Path,
    current: list[Path],
    type_package: Path | None = None,
) -> Path:
    required = required_cache_wheels(type_package) if type_package else []
    if arguments.base_bundle:
        chosen = Path(arguments.base_bundle).expanduser().resolve()
        missing = missing_cache_wheels(chosen, required)
        if missing:
            raise RefreshError(
                f"--base-bundle 缺少类型包所需 Wheel: {missing[0]}"
                + (f" 等 {len(missing)} 个" if len(missing) > 1 else "")
            )
        return chosen
    seeded = output_root / "base-bundles/r1pro-mujoco-0.5.0-dev"
    candidates: list[Path] = [seeded]
    for root in workspace_roots(framework):
        for relative in (
            Path("semantic-ability/ability-runtime/base-bundles/r1pro-mujoco-0.5.0-dev"),
            Path("ability-runtime/base-bundles/r1pro-mujoco-0.5.0-dev"),
        ):
            candidates.append(root / relative)
    candidates.extend(current)
    if not current:
        candidates.extend(
            sorted(
                find_product_bundles(output_root / "robot-bundle-archive"),
                key=lambda path: path.stat().st_mtime,
                reverse=True,
            )
        )
    seen: set[Path] = set()
    for candidate in candidates:
        resolved = candidate.resolve() if candidate.exists() else candidate
        if resolved in seen:
            continue
        seen.add(resolved)
        missing = missing_cache_wheels(resolved, required)
        if not missing:
            if resolved != seeded.resolve() and missing_cache_wheels(seeded, required):
                print(
                    "refresh-v050-mujoco: 跳过不完整的 "
                    f"{seeded}（缺 {missing_cache_wheels(seeded, required)[0]}），"
                    f"改用 {resolved}",
                    flush=True,
                )
            return resolved
        if (resolved / "wheels").is_dir():
            print(
                f"refresh-v050-mujoco: 跳过 {resolved}（缺 {missing[0]}）",
                flush=True,
            )
    raise RefreshError(
        "没有与类型包一致的离线 Wheel 缓存。"
        "把 ability-runtime 放到 $SEMANTIC/semantic-ability 下，"
        f"或准备含 {required[0] if required else 'bundle.yaml + wheels/'} 的 "
        f"{seeded}，或传 --base-bundle"
    )


def build(arguments: argparse.Namespace) -> None:
    framework = Path(__file__).resolve().parents[1]
    repos = repositories(framework)
    type_package = repos["deployment"] / "type-packages/r1pro-mujoco"
    ability_version = ability_build_version(repos["ability"], type_package)
    output_root = framework / ".output"
    work = output_root / "v050-mujoco-refresh"
    catalog = (
        Path(arguments.catalog).resolve()
        if arguments.catalog
        else output_root / "robot-bundles"
    )
    current = find_product_bundles(catalog)
    base_bundle = resolve_base_bundle(
        arguments,
        framework,
        output_root,
        current,
        type_package=type_package,
    )
    require_file(base_bundle / "bundle.yaml", "基础 Bundle manifest")
    wheel_cache = base_bundle / "wheels"
    if not wheel_cache.is_dir():
        raise RefreshError(f"基础 Bundle 缺少 Wheel 目录: {wheel_cache}")

    python = arguments.python or shutil.which("python3.13") or sys.executable
    vendor = resolve_vendor_root(arguments, framework)
    ability_framework = require_file(vendor / "AbilityFramework", "AbilityFramework")
    scaffold = ensure_vendor_scaffold(vendor, python)
    print(
        "refresh-v050-mujoco repos:"
        f" sdk={repos['sdk']}"
        f" ability={repos['ability']}"
        f" skills={repos['skills']}"
        f" deployment={repos['deployment']}"
        f" vendor={vendor}"
        f" wheels={wheel_cache}",
        flush=True,
    )
    python_version = subprocess.check_output(
        [
            python,
            "-c",
            "import sys; print(f'{sys.version_info.major}.{sys.version_info.minor}')",
        ],
        text=True,
    ).strip()
    if python_version != "3.13":
        raise RefreshError(
            "当前类型包离线 Wheel 是 cp313，构建 Python 必须为 3.13，"
            f"实际为 {python_version}: {python}"
        )

    safe_recreate(work / "build", output_root)
    build_root = work / "build"
    abilities_dir = build_root / "abilities"
    wheels_dir = build_root / "wheels"
    skills_dir = work / "robot-skills"
    staged_bundle = build_root / "r1pro-mujoco-0.5.0-dev"
    abilities_dir.mkdir(parents=True)
    wheels_dir.mkdir(parents=True)
    safe_recreate(skills_dir, output_root)

    make_env = os.environ.copy()
    make_env["PYTHON"] = python
    make_env["ROBOT_SDK_PATH"] = str(repos["sdk"])
    for repository in ("sdk", "deployment", "framework"):
        run(["make", "build"], repos[repository], env=make_env)

    ability_py = find_ability_py_wheel(vendor, wheel_cache)
    ability_python = prepare_ability_test_env(
        repos["ability"], python, ability_py, wheel_cache
    )
    ability_env = make_env.copy()
    ability_env["PYTHON"] = str(ability_python)
    ability_wheel = build_ability_wheel(
        repos["ability"], python, wheels_dir, ability_version, ability_env
    )
    # uv 管理的基础 Python 不应被直接安装 setuptools 等构建依赖。
    # Robot Skill 与 Ability 统一交给 uv 的隔离构建环境，避免
    # --no-build-isolation 依赖开发机 Python 中偶然存在的 setuptools。
    run(
        [
            "uv",
            "build",
            "--wheel",
            "--python",
            python,
            "--out-dir",
            str(wheels_dir),
        ],
        repos["skills"],
    )
    for project in ABILITY_PROJECTS:
        run(
            [
                str(scaffold),
                "pack",
                str(repos["ability"] / "abilities" / project),
                "-o",
                str(abilities_dir / f"{project}.zip"),
            ],
            framework,
        )

    skill_versions: dict[str, str] = {}
    for name, directory in SKILL_PROJECTS.items():
        source = repos["skills"] / "semantic_robot_skills/skills" / directory
        provisional = skills_dir / f"{name}.zip"
        version = zip_skill(source, provisional)
        provisional.rename(skills_dir / f"{name}-{version}.zip")
        skill_versions[name] = version

    sdk_core = single_file(
        repos["sdk"] / "dist",
        "semantic_robot_sdk_core-0.5.0.dev0-*.whl",
        "core SDK Wheel",
    )
    sdk_r1pro = single_file(
        repos["sdk"] / "dist",
        "semantic_robot_sdk_r1pro-0.5.0.dev0-*.whl",
        "R1 Pro SDK Wheel",
    )
    skill_sdk = single_file(
        wheels_dir,
        "semantic_robot_skill_sdk-0.1.0.dev0-*.whl",
        "Robot Skill SDK Wheel",
    )
    mappings = [
        f"bin/semantic-robot-instance={repos['deployment'] / 'bin/semantic-robot-instance'}",
        f"bin/AbilityFramework={ability_framework}",
        f"bin/semantic-pilot={framework / '.output/bin/semantic-pilot'}",
        f"wheels/{sdk_core.name}={sdk_core}",
        f"wheels/{sdk_r1pro.name}={sdk_r1pro}",
        f"wheels/{ability_wheel.name}={ability_wheel}",
        f"wheels/{skill_sdk.name}={skill_sdk}",
    ]
    mappings.extend(
        f"abilities/{project}.zip={abilities_dir / f'{project}.zip'}"
        for project in ABILITY_PROJECTS
    )
    command = [
        str(repos["deployment"] / "bin/semantic-robot-bundle"),
        "build",
        "--source",
        str(type_package),
        "--output",
        str(staged_bundle),
        "--python",
        python,
        "--wheel-dir",
        str(wheel_cache),
    ]
    for mapping in mappings:
        command.extend(["--file", mapping])
    run(command, framework)
    ability_implementation = verify_bundle_ability(staged_bundle, ability_wheel, ability_version)

    active_path = staged_bundle
    if arguments.activate:
        catalog.mkdir(parents=True, exist_ok=True)
        active_path = activate_bundle(
            staged_bundle, catalog, current, stop_users=arguments.stop_users
        )
        # Project绑定的是 Server 只读 Agent Skill，不属于 Robot Bundle。开发刷新时
        # 同步 Skill 与实际读取它们的 Profile，避免 Bundle 已更新而运行中的
        # Leader/Robot Agent 仍使用旧 allowlist 或旧工具范围。
        sync_agent_skills(framework)
        sync_agent_profiles(framework)
    summary = {
        "bundle": str(active_path),
        "activated": bool(arguments.activate),
        "ability_implementation": ability_implementation,
        "base_wheel_cache": str(base_bundle),
        "robot_skills": {
            name: str(skills_dir / f"{name}-{version}.zip")
            for name, version in skill_versions.items()
        },
    }
    (work / "refresh-summary.json").write_text(
        json.dumps(summary, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    print(json.dumps(summary, ensure_ascii=False, indent=2))
    if arguments.activate:
        print("Bundle 已激活。请重新启动 Semantic Server，使内存 Catalog 读取新路径。")
        print(
            "Robot Skill 包已生成但尚未发布；Server 启动后请执行本脚本的 "
            "publish 命令，再启动 Robot Runtime。"
        )


def server_json(
    server: str,
    token: str,
    path: str,
    *,
    method: str = "GET",
    data: bytes | None = None,
    content_type: str = "application/json",
) -> dict:
    request = urllib.request.Request(
        server + path,
        data=data,
        method=method,
        headers={
            "Authorization": "Bearer " + token,
            "Content-Type": content_type,
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        body = error.read().decode("utf-8", errors="replace")
        raise RefreshError(f"请求 {path} 失败: HTTP {error.code}: {body}") from error


def publish(arguments: argparse.Namespace) -> None:
    framework = Path(__file__).resolve().parents[1]
    package_root = framework / ".output/v050-mujoco-refresh/robot-skills"
    token = arguments.access_token or os.environ.get("SEMANTIC_ACCESS_TOKEN", "")
    if not token:
        raise RefreshError(
            "发布 Robot Skill 需要 --access-token 或 SEMANTIC_ACCESS_TOKEN"
        )
    server = arguments.server_http.rstrip("/")
    published: dict[str, str] = {}
    for name in SKILL_PROJECTS:
        package = single_file(package_root, f"{name}-*.zip", f"{name} 发布包")
        payload = server_json(
            server,
            token,
            "/api/v1/robot-skills",
            data=package.read_bytes(),
            method="POST",
            content_type="application/zip",
        )
        skill = payload.get("skill", payload)
        published[name] = str(skill["version"])
        print(f"已发布 {name}: {skill}")

    # 受管 Robot 的 Deployment 只在首次注册时播种 desired Skill，设备页此后可以
    # 独立选择版本。这个开发刷新命令是用户显式要求升级整套 MuJoCo 产品制品，
    # 因此只更新当前 r1pro-mujoco 受管实例；不会改动 Fake 或真机 Robot。
    devices = server_json(server, token, "/api/v1/devices").get("devices", [])
    targets = [
        item for item in devices
        if (item.get("runtime_instance") or {}).get("bundle_name") == BUNDLE_NAME
    ]
    for device in targets:
        robot_id = urllib.parse.quote(str(device["robot_id"]), safe="")
        for name, version in published.items():
            path = (
                f"/api/v1/devices/{robot_id}/skills/"
                f"{urllib.parse.quote(name, safe='')}/{urllib.parse.quote(version, safe='')}/install"
            )
            server_json(server, token, path, method="POST", data=b"{}")
        print(f"已更新 {device['robot_id']} 的 desired Robot Skill: {published}")


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    commands = result.add_subparsers(dest="command", required=True)
    build_parser = commands.add_parser(
        "build", help="重建一致的 MuJoCo Bundle 和三个 Robot Skill 包"
    )
    build_parser.add_argument(
        "--activate",
        action="store_true",
        help="把新 Bundle 激活到 Framework Catalog",
    )
    build_parser.add_argument(
        "--stop-users",
        action="store_true",
        help="激活前先 SIGTERM/SIGKILL 占用旧 Bundle 的进程（Ability / 实例）",
    )
    build_parser.add_argument(
        "--catalog", help="Framework Robot Bundle Catalog，默认 .output/robot-bundles"
    )
    build_parser.add_argument(
        "--base-bundle",
        help="第三方 Wheel 缓存目录；默认 ability-runtime/base-bundles 或 .output/base-bundles",
    )
    build_parser.add_argument(
        "--vendor-root",
        help="AbilityFramework 目录；默认 $SEMANTIC/semantic-ability/ability-runtime",
    )
    build_parser.add_argument("--python", help="Python 3.13 路径")
    build_parser.set_defaults(handler=build)

    publish_parser = commands.add_parser(
        "publish", help="把刚构建的三个 Robot Skill 包发布到 Server Registry"
    )
    publish_parser.add_argument(
        "--server-http", default="http://127.0.0.1:8080"
    )
    publish_parser.add_argument("--access-token")
    publish_parser.set_defaults(handler=publish)
    return result


def main() -> int:
    arguments = parser().parse_args()
    try:
        arguments.handler(arguments)
    except (RefreshError, subprocess.CalledProcessError, OSError) as error:
        print(f"refresh-v050-mujoco: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
