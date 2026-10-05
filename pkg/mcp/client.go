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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"insightos.cn/semantic-framework/pkg/version"
)

// 传输类型常量。只支持 streamable HTTP 与 stdio 两种（架构 16 §3）。
const (
	// TransportHTTP 是 streamable HTTP 传输，用于远程/卫星服务。
	TransportHTTP = "http"

	// TransportStdio 是 stdio 传输，用于本地 sidecar 子进程。
	TransportStdio = "stdio"
)

// maxDescriptionRunes 是工具描述的最大字符数（按 rune 计）。
// OpenAPI 衍生的 MCP server 会把 15-60KB 端点文档塞进 description，
// 截断保住模型上下文窗口，与 Claude Code 的 2048 上限同值。
const maxDescriptionRunes = 2048

// defaultTimeout 是 ServerConfig.Timeout 为零时的单次操作超时
// （建连/ping/ListTools/CallTool 共用同一超时模型，调用方 ctx 可再收紧）。
const defaultTimeout = 30 * time.Second

// clientName 是 initialize 握手中上报的客户端实现名。
const clientName = "semantic-server"

// ServerConfig 描述一个 MCP server 的连接参数。
// 与配置段 mcp_servers 的条目对应（namespace/enabled 是目录层概念，不进本结构）。
type ServerConfig struct {
	// Name server 唯一标识，参与 memoize 键计算。
	Name string

	// Transport 传输类型：TransportHTTP | TransportStdio。
	Transport string

	// Endpoint streamable HTTP 的 MCP endpoint URL（http 传输必填）。
	Endpoint string

	// Command stdio 子进程可执行文件（stdio 传输必填）。
	Command string

	// Args stdio 子进程参数（顺序敏感，参与 memoize 键计算）。
	Args []string

	// Env stdio 子进程追加的环境变量（"K=V" 形式，叠加在进程环境之上；
	// 键计算前排序，与书写顺序无关）。http 传输忽略本字段。
	Env []string

	// Timeout 单次操作（建连/ping/ListTools/CallTool）超时；零值用 defaultTimeout。
	Timeout time.Duration
}

// ToolInfo 是 tools/list 条目的客户端视图（Tool 的薄投影）。
type ToolInfo struct {
	// Name 工具名（server 侧原名，命名空间前缀由 R16 目录层加）。
	Name string

	// Description 工具描述，超过 2048 字符被截断。
	Description string

	// InputSchemaJSON 输入参数的 JSON Schema 序列化文本；server 未提供时为空串。
	InputSchemaJSON string
}

// Client 是到单个 MCP server 的会话封装，持有底层 SDK 会话。
// 方法并发安全（SDK ClientSession 本身并发安全）。
type Client struct {
	// cfg 连接参数快照（memoize 键的计算输入，不再变化）。
	cfg ServerConfig

	// session 底层 SDK 会话，Connect 成功后赋值。
	session *sdkmcp.ClientSession

	// cancel stdio 子进程生命周期的兜底取消（Close 后进程必然退出）；
	// http 传输为 nil。
	cancel context.CancelFunc

	// broken 连接类错误置位（见 isConnError）：Pool.Get 据此剔除重建，
	// 不再把失效连接发给调用方。
	broken atomic.Bool

	// mu 保护 toolsChangedFn（注册与通知分发可能并发）。
	mu sync.RWMutex

	// toolsChangedFn tools/list_changed 通知回调，由 OnToolsChanged 注册。
	toolsChangedFn func()
}

// Connect 建立到 cfg 描述的 MCP server 的会话（含 initialize 握手）。
// 超时为 cfg.Timeout（零值 30s）；失败时清理已建传输，不残留子进程。
func Connect(ctx context.Context, cfg ServerConfig) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	c := &Client{cfg: cfg}
	transport, err := c.buildTransport()
	if err != nil {
		return nil, err
	}

	// 通知处理器在 SDK client 构造时挂载，经 dispatch 转发到
	// OnToolsChanged 注册的回调（允许连接后再注册，见 notify.go）。
	sdkClient := sdkmcp.NewClient(
		&sdkmcp.Implementation{Name: clientName, Version: version.String()},
		&sdkmcp.ClientOptions{
			ToolListChangedHandler: func(context.Context, *sdkmcp.ToolListChangedRequest) {
				c.dispatchToolsChanged()
			},
		},
	)

	opCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	session, err := sdkClient.Connect(opCtx, transport, nil)
	if err != nil {
		c.closeTransport()
		return nil, fmt.Errorf("连接 MCP server %q 失败: %w", cfg.Name, err)
	}
	c.session = session
	return c, nil
}

// ListTools 拉取 server 全量工具清单（SDK 迭代器自动翻页）。
// 描述超过 2048 字符截断；连接类错误置 broken 标记。
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	opCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	var tools []ToolInfo
	for tool, err := range c.session.Tools(opCtx, nil) {
		if err != nil {
			c.markBroken(err)
			return nil, fmt.Errorf("列出 MCP server %q 工具失败: %w", c.cfg.Name, err)
		}
		tools = append(tools, toToolInfo(tool))
	}
	return tools, nil
}

// CallTool 调用指定工具。argsJSON 为参数的 JSON 文本（空串等价于无参数），
// 非法 JSON 在本地直接报错，不发往 server。
// 返回值为工具输出的文本内容（多段 TextContent 拼接；无文本而有
// StructuredContent 时返回其 JSON 序列化）。工具自身报错（IsError）
// 以 error 返回，错误信息含 server 给出的文本。
func (c *Client) CallTool(ctx context.Context, name, argsJSON string) (string, error) {
	var args json.RawMessage
	if argsJSON != "" {
		args = json.RawMessage(argsJSON)
		// 本地预检：json.Marshal 会校验 RawMessage 合法性，这里显式提前报出。
		if !json.Valid(args) {
			return "", fmt.Errorf("工具 %q 参数不是合法 JSON: %q", name, argsJSON)
		}
	}

	opCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	res, err := c.session.CallTool(opCtx, &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		c.markBroken(err)
		return "", fmt.Errorf("调用 MCP server %q 工具 %q 失败: %w", c.cfg.Name, name, err)
	}

	text := resultText(res)
	if res.IsError {
		return "", fmt.Errorf("工具 %q 执行失败: %s", name, text)
	}
	if text == "" && res.StructuredContent != nil {
		data, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return "", fmt.Errorf("序列化工具 %q 结构化输出失败: %w", name, err)
		}
		text = string(data)
	}
	return text, nil
}

// Close 关闭会话（http 发送 DELETE 终止会话；stdio 关闭子进程 stdin 并等待退出，
// 超时由 SDK 发送 SIGTERM，cancel 兜底防泄漏）。多次调用安全。
func (c *Client) Close() {
	if c.session != nil {
		_ = c.session.Close()
	}
	c.closeTransport()
}

// Broken 报告会话是否已因连接类错误失效（Pool 据此剔除重建）。
func (c *Client) Broken() bool {
	return c.broken.Load()
}

// markBroken 在连接类错误时置位 broken；协议/工具错误不影响连接复用。
func (c *Client) markBroken(err error) {
	if isConnError(err) {
		c.broken.Store(true)
	}
}

// ping 探测会话活性：Pool.Get 返回缓存连接前的健康验证；
// 失败同样经 markBroken 置位，保证调用方与 pool 看到一致状态。
func (c *Client) ping(ctx context.Context) error {
	opCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	if err := c.session.Ping(opCtx, &sdkmcp.PingParams{}); err != nil {
		c.markBroken(err)
		return err
	}
	return nil
}

// timeout 返回单次操作超时（cfg.Timeout 零值时用默认）。
func (c *Client) timeout() time.Duration {
	if c.cfg.Timeout > 0 {
		return c.cfg.Timeout
	}
	return defaultTimeout
}

// buildTransport 按 Transport 类型构建 SDK 传输层。
func (c *Client) buildTransport() (sdkmcp.Transport, error) {
	switch c.cfg.Transport {
	case TransportHTTP:
		// 不设置 http.Client.Timeout：它会切断 SDK 默认建立的独立 SSE
		// 长连（服务端推送通道）；操作超时一律走 context。
		return &sdkmcp.StreamableClientTransport{Endpoint: c.cfg.Endpoint}, nil

	case TransportStdio:
		// CommandContext + cancel 兜底：Close 后子进程必然退出，
		// 不依赖 SDK 的 SIGTERM 时序。stderr 丢弃，避免污染 server 日志流。
		ctx, cancel := context.WithCancel(context.Background())
		c.cancel = cancel
		cmd := exec.CommandContext(ctx, c.cfg.Command, c.cfg.Args...)
		cmd.Env = append(os.Environ(), c.cfg.Env...)
		return &sdkmcp.CommandTransport{Command: cmd}, nil

	default:
		return nil, fmt.Errorf("MCP server %q 传输类型 %q 不支持（仅 http|stdio）", c.cfg.Name, c.cfg.Transport)
	}
}

// closeTransport 释放传输层资源（stdio 的兜底取消）。
func (c *Client) closeTransport() {
	if c.cancel != nil {
		c.cancel()
	}
}

// validate 校验连接参数的完整性与传输类型白名单。
func (cfg ServerConfig) validate() error {
	if cfg.Name == "" {
		return errors.New("MCP server 配置缺少 name")
	}
	switch cfg.Transport {
	case TransportHTTP:
		if cfg.Endpoint == "" {
			return fmt.Errorf("MCP server %q 使用 http 传输但缺少 endpoint", cfg.Name)
		}
	case TransportStdio:
		if cfg.Command == "" {
			return fmt.Errorf("MCP server %q 使用 stdio 传输但缺少 command", cfg.Name)
		}
	default:
		return fmt.Errorf("MCP server %q 传输类型 %q 不支持（仅 http|stdio）", cfg.Name, cfg.Transport)
	}
	return nil
}

// toToolInfo 投影 SDK Tool 到客户端视图：描述截断、schema 序列化。
func toToolInfo(tool *sdkmcp.Tool) ToolInfo {
	info := ToolInfo{
		Name:        tool.Name,
		Description: truncateRunes(tool.Description, maxDescriptionRunes),
	}
	if tool.InputSchema != nil {
		if data, err := json.Marshal(tool.InputSchema); err == nil {
			info.InputSchemaJSON = string(data)
		}
	}
	return info
}

// truncateRunes 按 rune 截断字符串（中文等多字节字符按 1 字符计）。
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// resultText 拼接 CallToolResult 的全部 TextContent 段（"\n" 连接）。
func resultText(res *sdkmcp.CallToolResult) string {
	var parts []string
	for _, content := range res.Content {
		if tc, ok := content.(*sdkmcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}
