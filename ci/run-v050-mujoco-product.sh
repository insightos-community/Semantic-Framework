#!/usr/bin/env bash
set -euo pipefail

# MuJoCo 产品 Gate 需要已构建的 Server、已刷新 Bundle 和资产仓。
# 缺任何一项都失败，不能把开发态 Skip 记成验收通过。
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

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
    asset = resolve_mujoco_asset(framework)
    runtime = resolve_mujoco_runtime(framework)
except WorkspaceError as exc:
    print(exc, file=sys.stderr)
    sys.exit(1)

bundle = Path(os.environ.get(
    "SEMANTIC_MUJOCO_GATE_BUNDLE",
    framework / ".output" / "robot-bundles" / "r1pro-mujoco-0.5.0-dev",
)).resolve()
if not (bundle / "bin" / "semantic-robot-instance").exists():
    print(
        f"缺少 MuJoCo Bundle：{bundle}。"
        "在 Runner 上先执行 make refresh-v050-mujoco-bundle，"
        "或设置 SEMANTIC_MUJOCO_GATE_BUNDLE。",
        file=sys.stderr,
    )
    sys.exit(1)

print(f"mujoco_asset={asset}")
print(f"mujoco_runtime={runtime}")
print(f"bundle={bundle}")
PY

make test-v050-mujoco-product
