package mcp

// OnToolsChanged 注册 tools/list_changed 通知回调：server 工具集变化时
// fn 被调用。SDK v1.3.1 原生支持 notifications/tools/list_changed
// （ClientOptions.ToolListChangedHandler），本方法直接桥接，
// 不提供轮询对账入口。
//
// 送达前提：server 声明 tools.listChanged 能力；http 传输下 SDK 默认
// 建立独立 SSE 流接收服务端推送（StreamableClientTransport 的
// DisableStandaloneSSE 保持默认 false）；stdio 是天然双向通道。
// 回调在 SDK 接收 goroutine 中执行，应快速返回（重活投递给消费方
// 自己的队列）。连接前或连接后注册均可。
func (c *Client) OnToolsChanged(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.toolsChangedFn = fn
}

// dispatchToolsChanged 把 SDK 通知转发到已注册回调
// （处理器在 SDK client 构造时挂载，回调允许连接后再注册）。
func (c *Client) dispatchToolsChanged() {
	c.mu.RLock()
	fn := c.toolsChangedFn
	c.mu.RUnlock()
	if fn != nil {
		fn()
	}
}
