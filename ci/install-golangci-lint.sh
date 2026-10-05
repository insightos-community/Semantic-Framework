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

# 优先走 GOPROXY 安装；失败再从 GitHub 代理拉官方二进制。
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="${GOLANGCI_LINT_VERSION:-1.64.8}"
bindir="${GOBIN:-$repo_root/.go/bin}"
mkdir -p "$bindir"
export PATH="$bindir:$PATH"

if command -v golangci-lint >/dev/null 2>&1; then
  current="$(golangci-lint version 2>/dev/null | awk '{print $4}' || true)"
  if [[ "$current" == "v${version}" || "$current" == "$version" ]]; then
    exit 0
  fi
fi

if go install "github.com/golangci/golangci-lint/cmd/golangci-lint@v${version}"; then
  exit 0
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
bash "$repo_root/ci/fetch-github.sh" \
  "https://github.com/golangci/golangci-lint/releases/download/v${version}/golangci-lint-${version}-linux-amd64.tar.gz" \
  "$tmp/lint.tgz"
tar -xzf "$tmp/lint.tgz" -C "$tmp"
install -m 0755 "$tmp/golangci-lint-${version}-linux-amd64/golangci-lint" "$bindir/golangci-lint"
