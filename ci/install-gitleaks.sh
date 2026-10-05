#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="${GITLEAKS_VERSION:-8.24.2}"
bindir="${1:-$repo_root/.ci-bin}"
mkdir -p "$bindir"

if [[ -x "$bindir/gitleaks" ]]; then
  exit 0
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
bash "$repo_root/ci/fetch-github.sh" \
  "https://github.com/gitleaks/gitleaks/releases/download/v${version}/gitleaks_${version}_linux_x64.tar.gz" \
  "$tmp/gitleaks.tgz"
tar -xzf "$tmp/gitleaks.tgz" -C "$tmp"
install -m 0755 "$tmp/gitleaks" "$bindir/gitleaks"
