#!/usr/bin/env bash
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

set -euo pipefail

# Fake Gate 启动前先解析旁路仓库和 Ability 制品。
# 缺路径或制品时立即失败，不能靠 Python 后半段 skip 伪装成通过。
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

python3 - <<'PY'
from pathlib import Path
import sys

sys.path.insert(0, "tests/gate")
from workspace import WorkspaceError, resolve_gate_layout

try:
    layout = resolve_gate_layout(Path(".").resolve())
except WorkspaceError as exc:
    print(exc, file=sys.stderr)
    sys.exit(1)

print("v0.5 Fake Gate 工作树：")
print(f"  sdk={layout.sdk}")
print(f"  ability={layout.ability}")
print(f"  skills={layout.skills}")
print(f"  deployment={layout.deployment}")
print(f"  web={layout.web}")
print(f"  vendor={layout.vendor}")
print(f"  AbilityFramework={layout.ability_framework}")
print(f"  ability_py={layout.ability_py_wheel}")
print(f"  scaffold={layout.ability_scaffold}")
PY

make test-v050-real-gate
