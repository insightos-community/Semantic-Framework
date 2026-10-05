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

// stdio test server：pkg/mcp 测试用的最小 MCP stdio server。
// 用 SDK server 端实现而非 shell 脚本模拟 JSON-RPC——真实 initialize
// 握手/工具调用不依赖 jq 等外部工具，go build 即可运行。
package main

import (
	"context"
	"fmt"
	"os"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// pingArgs 是 ping 工具的入参。
type pingArgs struct {
	Text string `json:"text" jsonschema:"要回显的文本"`
}

func main() {
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "stdio-test-server", Version: "v0.0.1"}, nil)
	sdkmcp.AddTool(srv, &sdkmcp.Tool{Name: "ping", Description: "回显文本"},
		func(_ context.Context, _ *sdkmcp.CallToolRequest, in pingArgs) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "pong: " + in.Text}},
			}, nil, nil
		})
	// getenv 返回指定环境变量在子进程中的值，供测试验证 Env 透传。
	sdkmcp.AddTool(srv, &sdkmcp.Tool{Name: "getenv", Description: "返回环境变量值"},
		func(_ context.Context, _ *sdkmcp.CallToolRequest, in pingArgs) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: os.Getenv(in.Text)}},
			}, nil, nil
		})
	// stdin 关闭（client Close）后 Run 返回，进程自然退出。
	if err := srv.Run(context.Background(), &sdkmcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "stdio test server 运行失败: %v\n", err)
		os.Exit(1)
	}
}
