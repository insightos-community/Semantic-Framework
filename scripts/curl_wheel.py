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

"""可靠的 PyPI Wheel 下载器：用 curl 替代 pip 的下载环节。

背景：实测同一批依赖（torch/nvidia 等 GB 级 Wheel），pip 会在大文件上连接僵死
（317 MB 的 nvidia-cublas 卡 25 分钟零字节增长），而 curl 同文件 28 秒完成。
pip 的 HTTP 客户端在此网络环境下不可靠，因此依赖解析仍交给 pip/uv（成熟），
下载改为 curl（带僵死检测与重试）。

用法（命令行）：
    python3 curl_wheel.py --requirements req.txt --dest wheels/ \
        [--index https://mirrors.aliyun.com/pypi/simple] [--seed dir ...]

用法（库）：
    from curl_wheel import collect
    collect(requirements=Path("req.txt"), dest=Path("wheels"),
            index="https://mirrors.aliyun.com/pypi/simple", seeds=[...])
"""
from __future__ import annotations

import argparse
import concurrent.futures
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path
from urllib.parse import urljoin

DEFAULT_INDEX = "https://mirrors.aliyun.com/pypi/simple"
# 镜像回退链：国内镜像快但 index 页偶有缺失（实测 docopt/httpcore/httpx 在阿里云
# 页面不全），官方 PyPI 作为兜底保证完整性。
FALLBACK_INDEXES = [
    "https://mirrors.aliyun.com/pypi/simple",
    "https://pypi.tuna.tsinghua.edu.cn/simple",
    "https://pypi.org/simple",
]
# curl 僵死检测：速度低于 10 KB/s 持续 20 秒即放弃；单文件最多 10 分钟；失败重试 3 次。
CURL_ARGS = ["--fail", "--location", "--speed-limit", "10240", "--speed-time", "20",
             "--max-time", "600", "--retry", "3", "--retry-delay", "2", "--silent", "--show-error"]


def normalize(name: str) -> str:
    """PEP 503 规范化：包名统一小写、连续分隔符转单连字符（simple index 的目录名形式）。"""
    return re.sub(r"[-_.]+", "-", name).lower()


def filename_slug(name: str) -> str:
    """wheel 文件名里的包名形式：连字符转下划线。"""
    return normalize(name).replace("-", "_")


def parse_requirements(path: Path) -> list[tuple[str, str]]:
    """解析 requirements 文件，返回 (name, version) 列表；跳过路径行与标记行。"""
    pins: list[tuple[str, str]] = []
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.split("#", 1)[0].strip()
        if not line or line.startswith(("-", "http://", "https://", "file:")):
            continue
        # 形如 name==version[ ; marker]
        match = re.match(r"^([A-Za-z0-9][A-Za-z0-9._-]*)==([^;\s]+)", line)
        if match:
            pins.append((match.group(1), match.group(2)))
    return pins


def abi_tag(python: str) -> str:
    """把 Python 版本转成 wheel 的 ABI 标签：3.12 -> cp312，3.8.20 -> cp38。

    只取主次版本号——传完整补丁号（如 3.8.20）时若直接拼接会得到 cp3820，
    永远匹配不到任何 wheel。
    """
    parts = python.split(".")
    major = parts[0] if parts else "3"
    minor = parts[1] if len(parts) > 1 else ""
    return f"cp{major}{minor}"


def sdist_url(index: str, name: str, version: str, timeout: int = 25) -> str | None:
    """解析该版本的源码包（.tar.gz/.zip）URL，供无 wheel 的包使用。"""
    slug = normalize(name)
    page_url = f"{index.rstrip('/')}/{slug}/"
    result = subprocess.run(["curl", "--silent", "--fail", "--location",
                             "--max-time", str(timeout), page_url],
                            capture_output=True, text=True)
    if result.returncode != 0:
        return None
    # 源码包文件名用连字符形式（wheel 才用下划线），两种都试。
    for slug_form in {normalize(name), filename_slug(name)}:
        pattern = re.compile(
            r'href="([^"]*/' + re.escape(slug_form) + r"-" + re.escape(version) + r'\.(?:tar\.gz|zip)[^"]*)"')
        candidates = [urljoin(page_url, href) for href in pattern.findall(result.stdout)]
        if candidates:
            return candidates[0].split("#", 1)[0]
    return None


def wheel_rank(filename: str, python: str) -> tuple[int, int, int]:
    """wheel 候选排序权重，越小越优：目标 ABI > 通用 py3 > 其他；同级 manylinux x86_64 优先。

    同一包同版本可能有多个平台标签的 wheel（如 opencv 同时有 manylinux2014 与
    manylinux_2_28）；它们共存会让 pip 判定为版本冲突，因此选优后必须去重。
    """
    tag = abi_tag(python)
    if f"-{tag}-" in filename:
        abi = 0
    elif "-py3-" in filename or "-py2.py3-" in filename:
        abi = 1
    else:
        abi = 2
    if "manylinux" in filename and "x86_64" in filename:
        plat = 0
    elif "manylinux" in filename:
        plat = 1
    elif "linux_x86_64" in filename:
        plat = 2
    else:
        plat = 3
    return (abi, plat, len(filename))


def deduplicate(dest: Path, python: str, progress=print) -> int:
    """同一包同版本只保留一个 wheel（按平台/ABI 选优），其余删除。

    种子目录与 curl 下载可能带进同一版本的多个平台标签变体，pip 会因此报
    ResolutionImpossible。返回删除数量。
    """
    best: dict[tuple[str, str], tuple[tuple[int, int, int], Path]] = {}
    removed = 0
    for wheel in sorted(dest.glob("*.whl")):
        parts = wheel.name.split("-")
        if len(parts) < 2:
            continue
        key = (parts[0].lower(), parts[1].lower())   # (name, version)
        weight = wheel_rank(wheel.name, python)
        current = best.get(key)
        if current is None or weight < current[0]:
            if current is not None:
                current[1].unlink(missing_ok=True)
                removed += 1
            best[key] = (weight, wheel)
        else:
            wheel.unlink(missing_ok=True)
            removed += 1
    if removed:
        progress(f"curl 下载器: 去除 {removed} 个同版本重复 Wheel（平台标签变体）")
    return removed


def wheel_url(index: str, name: str, version: str, python: str = "3.12",
              timeout: int = 25) -> str | None:
    """从 simple index 页解析该版本的 wheel URL。

    候选排序优先：目标 Python ABI（如 cp312）> 通用 py3 > 其他 ABI；同级再按
    manylinux x86_64 优先。选错 ABI 会导致安装阶段失败，因此必须显式匹配。
    """
    slug = normalize(name)
    page_url = f"{index.rstrip('/')}/{slug}/"
    result = subprocess.run(["curl", "--silent", "--fail", "--location",
                             "--max-time", str(timeout), page_url],
                            capture_output=True, text=True)
    if result.returncode != 0:
        return None
    file_slug = filename_slug(name)
    pattern = re.compile(
        r'href="([^"]*/' + re.escape(file_slug) + r"-" + re.escape(version) + r'-[^"]*\.whl[^"]*)"')
    candidates = [urljoin(page_url, href) for href in pattern.findall(result.stdout)]
    if not candidates:
        loose = re.compile(r'href="([^"]*' + re.escape(version) + r'-[^"]*\.whl[^"]*)"')
        candidates = [urljoin(page_url, href) for href in loose.findall(result.stdout)]
    if not candidates:
        return None
    candidates.sort(key=lambda url: wheel_rank(Path(url.split("#", 1)[0]).name, python))
    return candidates[0].split("#", 1)[0]


def download(url: str, dest: Path) -> bool:
    """下载单个文件。

    注：曾尝试分段并发（Range）下载大文件，实测无效——本网络总带宽约 11.5 MB/s，
    单流已能跑满（11.2 MB/s），4 段并发合计仍是 11.5 MB/s，只是把带宽均分，
    反而增加分片失败与合并开销。故保持单连接。
    """
    result = subprocess.run(["curl", *CURL_ARGS, "--output", str(dest), url])
    return result.returncode == 0 and dest.is_file() and dest.stat().st_size > 1024


def collect(requirements: Path, dest: Path, index: str = DEFAULT_INDEX,
            seeds: list[Path] | None = None, jobs: int = 6, python: str = "3.12",
            progress=print) -> int:
    """下载 requirements 中所有 Wheel 到 dest；已存在或种子中已有的跳过。

    返回成功下载数。种子目录仅用于跳过（其内容由调用方通过 --find-links 交给
    pip 解析），不复制——避免重复占用空间。

    注意：仅覆盖 PyPI 上有 wheel 的包。少数只有源码包（如 docopt）或本地构建的
    产品 wheel（semantic_*）会跳过，由调用方回退给 pip 处理。
    """
    dest.mkdir(parents=True, exist_ok=True)
    seed_files: dict[str, Path] = {}
    for seed in seeds or []:
        if seed.is_dir():
            for wheel in seed.glob("*.whl"):
                seed_files.setdefault(wheel.name, wheel)
    pins = parse_requirements(requirements)
    todo: list[tuple[str, str]] = []
    linked = 0
    for name, version in pins:
        file_slug = filename_slug(name)
        # 目标目录里已有同名 wheel 或源码包（前缀匹配版本）即跳过
        def present(directory: Path) -> bool:
            return any(p.name.startswith(f"{file_slug}-{version}-")
                       or p.name.startswith(f"{file_slug}-{version}.")
                       for p in directory.iterdir() if p.is_file())
        if present(dest):
            continue
        # 种子目录命中：硬链接（跨设备则复制）到目标，使 pip 能直接消费。
        # 只标记跳过是不够的——种子是外部目录，pip 的 --wheel-dir 不会去那里取。
        seed_hit = next((p for n, p in seed_files.items()
                         if n.startswith(f"{file_slug}-{version}-")
                         or n.startswith(f"{file_slug}-{version}.")), None)
        if seed_hit is not None:
            target = dest / seed_hit.name
            try:
                os.link(seed_hit, target)
            except OSError:
                shutil.copy2(seed_hit, target)
            linked += 1
            continue
        todo.append((name, version))
    if linked:
        progress(f"curl 下载器: 从种子复用 {linked} 个 Wheel（硬链接）")
    if not todo:
        progress(f"curl 下载器: 全部 {len(pins)} 个依赖已就绪，无需下载")
        deduplicate(dest, python, progress)
        return 0
    progress(f"curl 下载器: 需下载 {len(todo)}/{len(pins)} 个（其余已缓存）")

    def worker(item: tuple[str, str]) -> tuple[str, bool, str]:
        name, version = item
        # 依次尝试各镜像：单个镜像的 index 页可能不完整，回退链保证覆盖率。
        indexes = [index] + [i for i in FALLBACK_INDEXES if i != index]
        for candidate_index in indexes:
            url = wheel_url(candidate_index, name, version, python)
            if url:
                target = dest / Path(url).name
                if download(url, target):
                    return name, True, target.name
        # 无 wheel 时取源码包：pip 随后会用 --no-index 从本地构建，避免再联网。
        for candidate_index in indexes:
            url = sdist_url(candidate_index, name, version)
            if url:
                target = dest / Path(url).name
                if download(url, target):
                    return name, True, target.name + "（源码包）"
        return name, False, "无 wheel 也无源码包"

    ok = 0
    skipped: list[str] = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=jobs) as pool:
        for name, success, info in pool.map(worker, todo):
            if success:
                ok += 1
                progress(f"  ✓ {info}")
            else:
                skipped.append(name)
                progress(f"  · {name}: {info}，留给 pip 处理")
    if skipped:
        progress(f"curl 下载器: {len(skipped)} 个需 pip 兜底（源码包或本地产品 wheel）")
    if ok == 0 and skipped:
        progress(f"curl 下载器: 未下载到任何 Wheel（ABI 标签 {abi_tag(python)} 可能不匹配），"
                 f"全部交由 pip 处理")
    # 种子链接与下载可能带进同版本的多个平台标签变体，pip 会判定为冲突。
    deduplicate(dest, python, progress)
    return ok


def locate(anchor: Path | None = None) -> Path:
    """定位本模块（供兄弟仓库的构建脚本 import）。

    各仓库在不同工作区布局下位置不一，按以下顺序查找 semantic-framework/scripts：
      1. $SEMANTIC/semantic-framework/scripts
      2. anchor 及其各级父目录下的 semantic-framework/scripts
      3. 本文件所在目录（同仓直接调用时）

    返回 scripts 目录；调用方加入 sys.path 后即可 `from curl_wheel import collect`。
    """
    candidates: list[Path] = []
    semantic = os.environ.get("SEMANTIC", "").strip()
    if semantic:
        candidates.append(Path(semantic) / "semantic-framework/scripts")
    base = (anchor or Path.cwd()).resolve()
    for parent in [base, *base.parents]:
        candidates.append(parent / "semantic-framework/scripts")
    candidates.append(Path(__file__).resolve().parent)
    for candidate in candidates:
        if (candidate / "curl_wheel.py").is_file():
            return candidate
    raise FileNotFoundError(
        "找不到 semantic-framework/scripts/curl_wheel.py；"
        "请把各仓库放在同一工作区下，或 export SEMANTIC=<工作区根目录>"
    )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--requirements", type=Path)
    parser.add_argument("--dest", type=Path)
    parser.add_argument("--index", default=DEFAULT_INDEX)
    parser.add_argument("--seed", type=Path, action="append", default=[])
    parser.add_argument("--jobs", type=int, default=6)
    parser.add_argument("--python", default="3.12", help="目标 Python 版本（决定 ABI 标签，如 3.12）")
    parser.add_argument("--dedup-only", action="store_true",
                        help="只对 --dest 做同版本去重，不下载（供调用方在 pip 之后清理）")
    args = parser.parse_args()
    if args.dedup_only:
        if not args.dest:
            parser.error("--dedup-only 需要 --dest")
        deduplicate(args.dest, args.python)
        return 0
    if not args.requirements or not args.dest:
        parser.error("需要 --requirements 与 --dest（或 --dedup-only --dest）")
    ok = collect(args.requirements, args.dest, args.index, args.seed, args.jobs, args.python)
    print(f"完成：下载 {ok} 个 Wheel → {args.dest}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
