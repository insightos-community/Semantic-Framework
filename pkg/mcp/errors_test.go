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
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestIsConnError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"协议错误", errors.New("unknown tool nosuch"), false},
		{"JSON-RPC method not found", errors.New("method not found"), false},
		{"调用方取消", context.Canceled, false},
		{"调用方超时", context.DeadlineExceeded, false},
		{"瞬时 HTTP 502 不应拆连接", errors.New(`rejected by transport: sending "tools/call": Bad Gateway`), false},
		{"EOF", io.EOF, true},
		{"net.ErrClosed", net.ErrClosed, true},
		{"ECONNREFUSED", syscall.ECONNREFUSED, true},
		{"SDK ErrConnectionClosed", sdkmcp.ErrConnectionClosed, true},
		{"包装后的 ErrConnectionClosed", fmt.Errorf("调用失败: %w", sdkmcp.ErrConnectionClosed), true},
		{"SDK 文本 connection closed", errors.New(`connection closed: calling "tools/call": client is closing`), true},
		{"jsonrpc2 client is closing", errors.New("client is closing"), true},
		{"jsonrpc2 server is closing", errors.New("server is closing"), true},
		{"connection refused", errors.New(`Post "http://127.0.0.1:1/mcp": connection refused`), true},
		{"write to closed stream", errors.New("rejected by transport: write to closed stream"), true},
		{"%v 包装的 EOF", fmt.Errorf("Post http://127.0.0.1:1/mcp: %v", io.EOF), true},
		{"CI 实测 rejected-by-transport EOF", errors.New(`calling "tools/call": rejected by transport: sending "tools/call": Post "http://127.0.0.1:32785/mcp": EOF`), true},
		{"idle connection 被关", errors.New(`calling "tools/call": rejected by transport: sending "tools/call": Post "http://127.0.0.1:1/mcp": http: server closed idle connection`), true},
		{"http2 连接被关", errors.New(`Get "http://127.0.0.1:1/mcp": http2: client conn is closed`), true},
		{"session not found", errors.New(`sending "tools/call": failed to connect (session ID: abc): session not found`), true},
		{"no such host", errors.New(`Post "http://missing.invalid/mcp": no such host`), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isConnError(tc.err); got != tc.want {
				t.Fatalf("isConnError(%v) = %v, 期望 %v", tc.err, got, tc.want)
			}
		})
	}
}
