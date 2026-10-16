#!/usr/bin/env bash
# Copyright 2026 InsightOS
# SPDX-License-Identifier: Apache-2.0
# Local/CI checks only. No robot, model provider or deployment endpoint is used.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
go test -p "${TEST_PARALLELISM:-4}" -race -count=1 -timeout=10m ./...
go build ./cmd/semantic-server ./cmd/semantic-pilot ./cmd/semantic
