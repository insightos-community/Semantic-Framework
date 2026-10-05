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

# 静态检查发布流水线的数据链，避免 Tag Pipeline 到 create_release 时才发现
# 校验文件或 dotenv 变量遗漏。这里检查稳定的关键片段，不尝试实现 YAML 解析器。
ci_file="${1:-ci/release.yml}"

if [[ ! -f "$ci_file" ]]; then
  echo "发布流水线文件不存在：$ci_file" >&2
  exit 1
fi

required_fragments=(
  'CHECKSUM_FILE="${PACKAGE_FILE}.sha256"'
  'sha256sum "$PACKAGE_FILE" > "$CHECKSUM_FILE"'
  'PACKAGE_FILE=%s\nCHECKSUM_FILE=%s\n'
  '- "*.sha256"'
  '--upload-file "$CHECKSUM_FILE" "${BASE_URL}/${CHECKSUM_FILE}"'
  'RELEASE_PACKAGE_URL=%s\nRELEASE_CHECKSUM_URL=%s\n'
  'test -n "$CHECKSUM_FILE"'
  'test -n "$RELEASE_CHECKSUM_URL"'
  "name: '\$CHECKSUM_FILE'"
  "url: '\$RELEASE_CHECKSUM_URL'"
)

for fragment in "${required_fragments[@]}"; do
  if ! grep -Fq -- "$fragment" "$ci_file"; then
    echo "发布流水线缺少必要配置：$fragment" >&2
    exit 1
  fi
done

if [[ "$(grep -Fc -- '--upload-file' "$ci_file")" -lt 2 ]]; then
  echo "发布流水线必须同时上传安装包和 SHA-256 文件" >&2
  exit 1
fi

echo "发布制品流水线静态校验通过"
