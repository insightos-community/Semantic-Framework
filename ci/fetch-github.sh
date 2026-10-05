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

# 按国内代理链下载 GitHub 文件。第一个参数是原始 https://github.com/... URL。
if [[ $# -ne 2 ]]; then
  echo "usage: $0 <github-https-url> <output-file>" >&2
  exit 2
fi

src="$1"
dest="$2"
mkdir -p "$(dirname "$dest")"

normalize_prefix() {
  local prefix="$1"
  [[ -z "$prefix" ]] && return 0
  printf '%s/' "${prefix%/}"
}

try_get() {
  curl -fL --retry 2 --retry-delay 2 --connect-timeout 20 --max-time 180 -o "$dest" "$1"
}

prefixes=()
if [[ -n "${GITHUB_PROXY:-}" ]]; then
  prefixes+=("$(normalize_prefix "$GITHUB_PROXY")")
fi
prefixes+=(
  "https://ghfast.top/"
  "https://gh-proxy.com/"
  "https://mirror.ghproxy.com/"
  ""
)

seen=" "
for prefix in "${prefixes[@]}"; do
  case "$seen" in
    *" ${prefix} "*) continue ;;
  esac
  seen+="${prefix} "
  if [[ -z "$prefix" ]]; then
    url="$src"
  else
    url="${prefix}${src}"
  fi
  echo "fetch: $url"
  if try_get "$url"; then
    exit 0
  fi
done

echo "failed to download $src via GitHub proxies" >&2
exit 1
