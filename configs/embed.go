// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package configs 向 semantic 安装器提供编译期内置的仓库配置模板。
// 运行时设置绝不能写入这些内置文件；semantic init 会先把它们复制到
// 安装目录，之后 Server、热重载与前端设置只读写安装副本。
package configs

import "embed"

// Templates 包含不可变的 Server 配置模板，以及首次安装所需的默认 Agent
// 和 Skill 目录树。go:embed 让发布后的 CLI 不再依赖源码仓库位置。
//
//go:embed semantic-server.yaml agents skills runtimes.d scenes.d
var Templates embed.FS
