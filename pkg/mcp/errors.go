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

package mcp

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrServerUnavailable 是 Pool 短路期间 Get 返回的错误。
// 错误文本即稳定错误码 MCP_SERVER_UNAVAILABLE，供上层（R16 目录层）
// 经 errors.Is 判定后转为结构化不可用响应。
var ErrServerUnavailable = errors.New("MCP_SERVER_UNAVAILABLE")

// isConnError 判定"连接类错误"——传输层断开或服务端会话过期，
// 与协议错误（JSON-RPC WireError，如未知工具）和工具错误（IsError）区分：
// 只有连接类错误会置 broken 并触发 Pool 的剔除重建。
//
// SDK streamable 把 net/http client.Do 的错误用 %v（不是 %w）包进
// "rejected by transport"，错误链在此断开，errors.Is(io.EOF) 对不上。
// 因此除哨兵外，还认 HTTP 客户端失败形态 `Post "url": ...` / `Get "url": ...`。
// 502/503 等瞬时 HTTP 状态被 SDK 包成 "rejected by transport: ...: Bad Gateway"，
// 不含 Post/Get，不会被当成连接死亡（与 SDK 注释一致）。
func isConnError(err error) bool {
	if err == nil {
		return false
	}
	// 调用方取消/超时不是传输层死亡：下次 Get 应复用连接再 ping。
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ETIMEDOUT) ||
		errors.Is(err, sdkmcp.ErrConnectionClosed) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}

	msg := err.Error()
	// SDK %v 包装后的 net/http 拨号/读写失败。
	if strings.Contains(msg, `Post "`) || strings.Contains(msg, `Get "`) {
		return true
	}
	for _, pat := range []string{
		"session not found",
		"connection refused",
		"connection reset",
		"broken pipe",
		"connection closed",
		"client is closing",
		"server is closing",
		"write to closed stream",
		"use of closed network connection",
		"http: server closed",
		"http2: client conn",
		"no such host",
		"network is unreachable",
		"connection aborted",
		"EOF",
	} {
		if strings.Contains(msg, pat) {
			return true
		}
	}
	return false
}
