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

"""采集 Semantic 开发环境的只读诊断信息。"""

from __future__ import annotations

import argparse
import json
import platform
import subprocess
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any


def parse_args() -> argparse.Namespace:
    """解析诊断目标与输出路径。"""
    parser = argparse.ArgumentParser(description="Semantic 只读诊断")
    parser.add_argument("--server-url", default="http://127.0.0.1:8080")
    parser.add_argument("--json-output", required=True)
    parser.add_argument("--markdown-output", required=True)
    return parser.parse_args()


def run_readonly(command: list[str], timeout: int = 5) -> dict[str, Any]:
    """执行固定只读命令，失败也返回结构化结果，不抛出敏感环境信息。"""
    try:
        completed = subprocess.run(
            command,
            capture_output=True,
            text=True,
            timeout=timeout,
            check=False,
        )
        return {
            "ok": completed.returncode == 0,
            "exit_code": completed.returncode,
            "stdout": completed.stdout.strip(),
            "stderr": completed.stderr.strip(),
        }
    except (FileNotFoundError, subprocess.TimeoutExpired) as error:
        return {"ok": False, "error": type(error).__name__, "message": str(error)}


def check_health(server_url: str) -> dict[str, Any]:
    """读取 Semantic 健康端点，不携带认证头或其他凭据。"""
    url = server_url.rstrip("/") + "/api/v1/system/healthz"
    try:
        with urllib.request.urlopen(url, timeout=3) as response:
            body = response.read(4096).decode("utf-8", errors="replace")
            return {"ok": 200 <= response.status < 300, "status": response.status, "body": body}
    except (urllib.error.URLError, TimeoutError) as error:
        return {"ok": False, "url": url, "error": type(error).__name__, "message": str(error)}


def collect(server_url: str) -> dict[str, Any]:
    """汇总操作系统、工具链、Docker 与 Server 健康状态。"""
    return {
        "platform": {
            "system": platform.system(),
            "release": platform.release(),
            "machine": platform.machine(),
            "python": platform.python_version(),
        },
        "go": run_readonly(["go", "version"]),
        "docker_version": run_readonly(["docker", "version", "--format", "{{json .}}"]),
        "docker_info": run_readonly([
            "docker", "info", "--format",
            "{{json .ServerVersion}} {{json .Driver}} {{json .OperatingSystem}}",
        ]),
        "semantic_health": check_health(server_url),
    }


def render_markdown(result: dict[str, Any]) -> str:
    """生成不包含密钥的简洁 Markdown 报告。"""
    platform_info = result["platform"]
    lines = [
        "# Semantic 诊断报告",
        "",
        f"- 系统：{platform_info['system']} {platform_info['release']} ({platform_info['machine']})",
        f"- Python：{platform_info['python']}",
        "",
        "| 检查项 | 状态 | 摘要 |",
        "|---|---|---|",
    ]
    for key in ("go", "docker_version", "docker_info", "semantic_health"):
        item = result[key]
        status = "通过" if item.get("ok") else "失败"
        summary = item.get("stdout") or item.get("body") or item.get("message") or "-"
        summary = str(summary).replace("\n", " ")[:500]
        lines.append(f"| {key} | {status} | {summary} |")
    return "\n".join(lines) + "\n"


def main() -> None:
    """采集并写入 JSON/Markdown；局部检查失败仍正常产出报告。"""
    args = parse_args()
    result = collect(args.server_url)
    json_output = Path(args.json_output)
    markdown_output = Path(args.markdown_output)
    json_output.parent.mkdir(parents=True, exist_ok=True)
    markdown_output.parent.mkdir(parents=True, exist_ok=True)
    json_output.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    markdown_output.write_text(render_markdown(result), encoding="utf-8")
    print(json.dumps({
        "checks": {key: value.get("ok", True) for key, value in result.items() if key != "platform"},
        "json_output": str(json_output),
        "markdown_output": str(markdown_output),
    }, ensure_ascii=False))


if __name__ == "__main__":
    main()
