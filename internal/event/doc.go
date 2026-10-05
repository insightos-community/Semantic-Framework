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

// Package event 提供进程内事件总线。
//
// 总线连接事件的发布者（Agent 运行时、任务体系等域模块）与订阅者
// （WS 网关等接入层组件）：发布方只面向 topic 投递，不关心谁订阅；
// 订阅方按 topic 接收 Event。Publish 异步非阻塞——订阅者消费不及时
// 时丢弃事件并记录 DEBUG 日志，保证慢订阅者不会拖垮发布链路。
package event
