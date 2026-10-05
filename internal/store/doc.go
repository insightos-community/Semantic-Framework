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

// Package store 提供 semantic-server 的元数据持久化能力。
//
// 首版仅实现 SQLite 驱动（modernc.org/sqlite，纯 Go 无 CGO），存储
// 认证所需的用户与 token 两类记录；chat 等业务表随后续里程碑按
// 版本化迁移逐个引入（见 migrate.go）。store 处于分层依赖最底层，
// 只依赖 pkg/config 与 pkg/log，不依赖任何 server 侧模块。
package store
