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

// Package bootstrap 是 semantic-server 的装配层。
//
// main 只负责解析参数与加载配置，组件的创建、路由注册与生命周期管理
// 全部收敛在本包：wire_access.go 的 Wire 按启动序列装配接入层
// （store 迁移 → auth 种子 → HTTP/WS 网关），bootstrap.go 的 App.Run
// 负责 HTTP/WS 双监听的启动与基于信号的优雅退出（含 store 关闭）。
package bootstrap
