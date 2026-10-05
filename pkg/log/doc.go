// Package log 提供框架统一的结构化日志能力。
//
// 本包基于 Go 标准库 log/slog 实现，固定 JSON 格式输出，日志级别可在
// 创建时与运行时配置；通过 WithField/WithTraceID/WithError 等方法以
// 不可变方式派生携带上下文字段的子日志器。traceid.go 提供 trace_id 的
// 生成与 context 注入/提取，用于跨模块关联同一请求链路的日志。
//
// 使用方式:
//
//	logger := log.New(log.Options{Level: log.LevelInfo})
//	logger.Info("服务启动", "addr", ":8080")
//	logger.WithTraceID(log.GenerateTraceID()).Info("处理请求")
package log
