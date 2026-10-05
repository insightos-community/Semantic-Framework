// Package http 是 semantic-server 的 HTTP 网关（接入层）。
//
// 只做协议适配：请求 ID 注入、panic 恢复、访问日志、路由装配与
// 统一错误格式，不含任何业务逻辑。路由分公开组（登录、系统探活）
// 与受保护组（经 auth 中间件鉴权），具体业务 handler 由各域模块提供。
package http
