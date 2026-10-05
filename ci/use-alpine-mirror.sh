#!/bin/sh
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

set -eu

# Alpine 官方镜像没有 bash。发布 Job 必须用 sh。
mirror="${ALPINE_MIRROR:-https://mirrors.aliyun.com/alpine}"
if [ -f /etc/apk/repositories ]; then
  sed -i "s#https\\?://dl-cdn.alpinelinux.org/alpine#${mirror}#g" /etc/apk/repositories
fi
