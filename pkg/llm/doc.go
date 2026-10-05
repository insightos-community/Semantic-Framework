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

// Package llm 提供 LLM 提供方注册表与计量类型。
//
// 本包只建模配置与注册表，不 import 任何内核依赖（eino/eino-ext）：
// 内核适配收敛在 internal/agent/kernel（架构文档 01 §3 的 ACL 纪律）。
// api_key 不进入配置结构，解析顺序为：进程环境变量
// SEMANTIC_LLM_API_KEY_<名称大写>（含 .env 注入，同一 base_url 的多个
// 条目共享同一把 key，docs/architecture/02 §5）> 服务端托管密钥库
// （KeyStore 接口，internal/store 实现，bootstrap 装配时注入，nil 时仅
// env）。解析结果按端点名缓存（含来源标记），Reload/InvalidateKeyCache
// 清空重读。
package llm
