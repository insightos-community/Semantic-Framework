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
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// newTestLogger 创建输出到 bytes.Buffer 的测试日志器，返回日志器与缓冲，
// 便于捕获并断言日志内容，避免测试依赖标准输出。
func newTestLogger(level Level) (*Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return New(Options{Level: level, Writer: buf}), buf
}

// parseLastEntry 解析缓冲区中最后一行 JSON 日志为 map，用于断言字段内容。
func parseLastEntry(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	var entry map[string]any
	if err := json.Unmarshal(lines[len(lines)-1], &entry); err != nil {
		t.Fatalf("解析日志行失败: %v, 原始内容: %q", err, lines[len(lines)-1])
	}
	return entry
}

// TestLogLevels 验证各级别日志在不同设定级别下的过滤行为：
// 高于等于当前级别的消息应被记录，低于当前级别的应被丢弃。
func TestLogLevels(t *testing.T) {
	t.Run("Debug级别记录所有日志", func(t *testing.T) {
		logger, buf := newTestLogger(LevelDebug)

		for _, msg := range []string{"调试消息", "信息消息", "警告消息", "错误消息"} {
			buf.Reset()
			switch msg {
			case "调试消息":
				logger.Debug(msg)
			case "信息消息":
				logger.Info(msg)
			case "警告消息":
				logger.Warn(msg)
			case "错误消息":
				logger.Error(msg)
			}
			if !strings.Contains(buf.String(), msg) {
				t.Errorf("DEBUG 级别下应记录 %q，实际输出: %q", msg, buf.String())
			}
		}
	})

	t.Run("Info级别过滤Debug", func(t *testing.T) {
		logger, buf := newTestLogger(LevelInfo)

		logger.Debug("调试消息")
		if buf.Len() != 0 {
			t.Errorf("INFO 级别下不应记录 Debug 消息，实际输出: %q", buf.String())
		}

		logger.Info("信息消息")
		if !strings.Contains(buf.String(), "信息消息") {
			t.Errorf("INFO 级别下应记录 Info 消息，实际输出: %q", buf.String())
		}
	})

	t.Run("Error级别过滤低级别", func(t *testing.T) {
		logger, buf := newTestLogger(LevelError)

		logger.Debug("调试消息")
		logger.Info("信息消息")
		logger.Warn("警告消息")
		if buf.Len() != 0 {
			t.Errorf("ERROR 级别下不应记录 Debug/Info/Warn 消息，实际输出: %q", buf.String())
		}

		logger.Error("错误消息")
		if !strings.Contains(buf.String(), "错误消息") {
			t.Errorf("ERROR 级别下应记录 Error 消息，实际输出: %q", buf.String())
		}
	})
}

// TestTraceLevel 验证 Trace 级别的输出与过滤行为。
func TestTraceLevel(t *testing.T) {
	t.Run("Trace级别记录Trace消息", func(t *testing.T) {
		logger, buf := newTestLogger(LevelTrace)
		logger.Trace("追踪消息")
		if !strings.Contains(buf.String(), "追踪消息") {
			t.Errorf("TRACE 级别下应记录 Trace 消息，实际输出: %q", buf.String())
		}
	})

	t.Run("Debug级别过滤Trace", func(t *testing.T) {
		logger, buf := newTestLogger(LevelDebug)
		logger.Trace("追踪消息")
		if buf.Len() != 0 {
			t.Errorf("DEBUG 级别下不应记录 Trace 消息，实际输出: %q", buf.String())
		}
	})
}

// TestWithField 验证 WithField 正确附加单个键值对到日志输出。
func TestWithField(t *testing.T) {
	logger, buf := newTestLogger(LevelDebug)

	logger.WithField("component", "scheduler").Info("任务调度开始")

	entry := parseLastEntry(t, buf)
	if entry["component"] != "scheduler" {
		t.Errorf("日志条目应包含 component=scheduler，实际: %v", entry["component"])
	}
	if entry["msg"] != "任务调度开始" {
		t.Errorf("日志消息内容应正确，实际: %v", entry["msg"])
	}
}

// TestWithFields 验证 WithFields 同时附加多个字段到日志输出。
func TestWithFields(t *testing.T) {
	logger, buf := newTestLogger(LevelDebug)

	logger.WithFields(map[string]any{
		"module":  "discovery",
		"version": "1.0.0",
	}).Info("服务发现模块启动")

	entry := parseLastEntry(t, buf)
	if entry["module"] != "discovery" {
		t.Errorf("应包含 module=discovery，实际: %v", entry["module"])
	}
	if entry["version"] != "1.0.0" {
		t.Errorf("应包含 version=1.0.0，实际: %v", entry["version"])
	}
}

// TestWithTraceID 验证 WithTraceID 将 trace_id 附加到日志输出。
func TestWithTraceID(t *testing.T) {
	logger, buf := newTestLogger(LevelDebug)

	logger.WithTraceID("abc-123").Info("处理请求")

	entry := parseLastEntry(t, buf)
	if entry["trace_id"] != "abc-123" {
		t.Errorf("日志条目应包含 trace_id 字段，实际: %v", entry["trace_id"])
	}
}

// TestWithError 验证 WithError 将错误信息以 error 字段附加到日志。
func TestWithError(t *testing.T) {
	logger, buf := newTestLogger(LevelDebug)

	logger.WithError(errors.New("连接超时")).Error("数据库操作失败")

	entry := parseLastEntry(t, buf)
	if entry["error"] != "连接超时" {
		t.Errorf("日志条目应包含 error 字段，实际: %v", entry["error"])
	}
}

// TestWithErrorNil 验证 WithError(nil) 返回原日志器且不添加 error 字段。
func TestWithErrorNil(t *testing.T) {
	logger, buf := newTestLogger(LevelDebug)

	logger.WithError(nil).Info("正常操作")

	entry := parseLastEntry(t, buf)
	if _, hasError := entry["error"]; hasError {
		t.Errorf("WithError(nil) 不应添加 error 字段，实际条目: %v", entry)
	}
}

// TestChainedWith 验证多个 With 方法链式组合后所有字段均出现在日志中。
func TestChainedWith(t *testing.T) {
	logger, buf := newTestLogger(LevelDebug)

	logger.
		WithField("service", "pilot").
		WithTraceID("trace-001").
		WithError(errors.New("传感器异常")).
		Error("设备通信失败")

	entry := parseLastEntry(t, buf)
	if entry["service"] != "pilot" {
		t.Errorf("应包含 service=pilot，实际: %v", entry["service"])
	}
	if entry["trace_id"] != "trace-001" {
		t.Errorf("应包含 trace_id=trace-001，实际: %v", entry["trace_id"])
	}
	if entry["error"] != "传感器异常" {
		t.Errorf("应包含 error 字段，实际: %v", entry["error"])
	}
}

// TestStructuredArgs 验证在日志方法中直接传递 key-value 参数对。
func TestStructuredArgs(t *testing.T) {
	logger, buf := newTestLogger(LevelDebug)

	logger.Info("请求完成", "method", "GET", "status", 200)

	entry := parseLastEntry(t, buf)
	if entry["method"] != "GET" {
		t.Errorf("应包含 method=GET，实际: %v", entry["method"])
	}
	if entry["status"] != float64(200) {
		t.Errorf("应包含 status=200，实际: %v", entry["status"])
	}
}

// TestSetLevel 验证运行时动态调整日志级别立即生效。
func TestSetLevel(t *testing.T) {
	logger, buf := newTestLogger(LevelInfo)

	if got := logger.Level(); got != LevelInfo {
		t.Fatalf("初始级别应为 INFO，实际: %s", got)
	}

	logger.SetLevel(LevelDebug)
	if got := logger.Level(); got != LevelDebug {
		t.Errorf("SetLevel 后级别应为 DEBUG，实际: %s", got)
	}

	logger.Debug("调整后可见")
	if !strings.Contains(buf.String(), "调整后可见") {
		t.Errorf("调整级别后 Debug 消息应被记录，实际输出: %q", buf.String())
	}
}

// TestParseLevel 验证级别字符串解析（大小写、空格、别名、未知输入）。
func TestParseLevel(t *testing.T) {
	tests := []struct {
		input    string
		expected Level
	}{
		{"trace", LevelTrace},
		{"TRACE", LevelTrace},
		{"debug", LevelDebug},
		{"Debug", LevelDebug},
		{"info", LevelInfo},
		{"INFO", LevelInfo},
		{"warn", LevelWarn},
		{"warning", LevelWarn},
		{"error", LevelError},
		{"ERROR", LevelError},
		{"fatal", LevelFatal},
		{"  info  ", LevelInfo},
		{"unknown", LevelInfo},
		{"", LevelInfo},
	}
	for _, tt := range tests {
		if got := ParseLevel(tt.input); got != tt.expected {
			t.Errorf("ParseLevel(%q) = %s，期望 %s", tt.input, got, tt.expected)
		}
	}
}

// TestLevelString 验证 Level.String() 返回正确的大写字符串表示。
func TestLevelString(t *testing.T) {
	tests := []struct {
		level    Level
		expected string
	}{
		{LevelTrace, "TRACE"},
		{LevelDebug, "DEBUG"},
		{LevelInfo, "INFO"},
		{LevelWarn, "WARN"},
		{LevelError, "ERROR"},
		{LevelFatal, "FATAL"},
		{Level(99), "UNKNOWN"},
	}
	for _, tt := range tests {
		if got := tt.level.String(); got != tt.expected {
			t.Errorf("Level(%d).String() = %q，期望 %q", int(tt.level), got, tt.expected)
		}
	}
}
