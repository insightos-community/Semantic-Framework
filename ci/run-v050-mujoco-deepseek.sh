#!/usr/bin/env bash
set -euo pipefail

# 真实 DeepSeek 只接受显式打开的手动流水线。密钥必须是 Protected 变量。
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

if [[ "${SEMANTIC_DEEPSEEK_ENABLED:-}" != "1" ]]; then
  echo "SEMANTIC_DEEPSEEK_ENABLED 必须明确设置为 1" >&2
  exit 2
fi

if [[ -n "${CI_PIPELINE_SOURCE:-}" && "${CI_PIPELINE_SOURCE}" != "web" ]]; then
  echo "DeepSeek Gate 只允许 GitLab web / 手动流水线" >&2
  exit 2
fi

test -x .output/bin/semantic-server || {
  echo "缺少 .output/bin/semantic-server。先让 build Job 成功，或在 Runner 上 make build。" >&2
  exit 1
}

python3 - <<'PY'
from pathlib import Path
import os
import sys

sys.path.insert(0, "tests/gate")
from workspace import WorkspaceError, resolve_mujoco_asset, resolve_mujoco_runtime

framework = Path(".").resolve()
try:
    resolve_mujoco_asset(framework)
    resolve_mujoco_runtime(framework)
except WorkspaceError as exc:
    print(exc, file=sys.stderr)
    sys.exit(1)

bundle = Path(os.environ.get(
    "SEMANTIC_MUJOCO_GATE_BUNDLE",
    framework / ".output" / "robot-bundles" / "r1pro-mujoco-0.5.0-dev",
)).resolve()
if not (bundle / "bin" / "semantic-robot-instance").exists():
    print(f"缺少 MuJoCo Bundle：{bundle}", file=sys.stderr)
    sys.exit(1)
PY

make test-v050-mujoco-deepseek-single
