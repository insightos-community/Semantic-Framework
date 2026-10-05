#!/usr/bin/env bash
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
