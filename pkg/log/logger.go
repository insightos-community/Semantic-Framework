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

package log

import (
	"context"
	"io"
	"log/slog"
	"os"
)

// Options 定义日志器的创建参数。
type Options struct {
	// Level 日志输出的最低级别，低于此级别的消息将被丢弃。
	Level Level

	// Writer 日志输出目标；为 nil 时输出到标准输出。
	// 测试中可注入 bytes.Buffer 以断言输出内容。
	Writer io.Writer
}

// Logger 是基于 log/slog 的结构化 JSON 日志器。
// 所有方法并发安全；With 系列方法以不可变方式派生子日志器，
// 不修改原实例，可安全地在请求链路中层层附加字段。
type Logger struct {
	// logger 底层的 slog.Logger 实例，完成实际写入。
	logger *slog.Logger

	// levelVar 当前日志级别，基于 slog.LevelVar 支持运行时原子调整。
	levelVar *slog.LevelVar

	// fields 预附加的结构化字段，由 With 系列方法累积。
	fields []any
}

// New 创建一个新的 Logger，固定 JSON 格式输出。
// opts.Level 决定初始级别，opts.Writer 决定输出目标（nil 时为 os.Stdout）。
func New(opts Options) *Logger {
	w := opts.Writer
	if w == nil {
		w = os.Stdout
	}
	levelVar := &slog.LevelVar{}
	levelVar.Set(slog.Level(opts.Level))
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: levelVar})
	return &Logger{
		logger:   slog.New(handler),
		levelVar: levelVar,
	}
}

// Trace 记录追踪级别日志，用于最细粒度的链路细节。
func (l *Logger) Trace(msg string, args ...any) {
	l.log(slog.Level(LevelTrace), msg, args...)
}

// Debug 记录调试级别日志，用于开发阶段的详细信息。
func (l *Logger) Debug(msg string, args ...any) {
	l.log(slog.LevelDebug, msg, args...)
}

// Info 记录信息级别日志，用于常规运行状态变更。
func (l *Logger) Info(msg string, args ...any) {
	l.log(slog.LevelInfo, msg, args...)
}

// Warn 记录警告级别日志，用于需要关注但不影响运行的事件。
func (l *Logger) Warn(msg string, args ...any) {
	l.log(slog.LevelWarn, msg, args...)
}

// Error 记录错误级别日志，用于失败的操作。
func (l *Logger) Error(msg string, args ...any) {
	l.log(slog.LevelError, msg, args...)
}

// Fatal 记录致命级别日志，随后以状态码 1 终止进程。
// 仅在不可恢复的严重错误（如启动失败）时使用。
func (l *Logger) Fatal(msg string, args ...any) {
	l.log(slog.Level(LevelFatal), msg, args...)
	os.Exit(1)
}

// log 是所有级别输出的统一入口：合并预附加字段与本次调用的参数后写入 slog。
func (l *Logger) log(level slog.Level, msg string, args ...any) {
	allArgs := make([]any, 0, len(l.fields)+len(args))
	allArgs = append(allArgs, l.fields...)
	allArgs = append(allArgs, args...)
	l.logger.Log(context.Background(), level, msg, allArgs...)
}

// WithField 返回携带单个结构化字段的子日志器，原日志器不受影响。
func (l *Logger) WithField(key string, value any) *Logger {
	newFields := make([]any, len(l.fields), len(l.fields)+2)
	copy(newFields, l.fields)
	newFields = append(newFields, key, value)
	return &Logger{logger: l.logger, levelVar: l.levelVar, fields: newFields}
}

// WithFields 返回携带多个结构化字段的子日志器。
func (l *Logger) WithFields(fields map[string]any) *Logger {
	newFields := make([]any, len(l.fields), len(l.fields)+len(fields)*2)
	copy(newFields, l.fields)
	for k, v := range fields {
		newFields = append(newFields, k, v)
	}
	return &Logger{logger: l.logger, levelVar: l.levelVar, fields: newFields}
}

// WithTraceID 返回携带链路追踪 ID（trace_id 字段）的子日志器，
// 用于关联同一请求链路上的所有日志。
func (l *Logger) WithTraceID(traceID string) *Logger {
	return l.WithField("trace_id", traceID)
}

// WithError 返回携带错误信息（error 字段）的子日志器。
// 传入 nil 时返回原日志器，不添加字段。
func (l *Logger) WithError(err error) *Logger {
	if err == nil {
		return l
	}
	return l.WithField("error", err.Error())
}

// SetLevel 在运行时动态调整日志级别，原子操作，立即生效。
func (l *Logger) SetLevel(level Level) {
	l.levelVar.Set(slog.Level(level))
}

// Level 返回当前生效的日志级别。
func (l *Logger) Level() Level {
	return Level(l.levelVar.Level())
}
