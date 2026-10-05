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

import "strings"

// Level 表示日志的严重程度级别，数值越大越严重。
// 取值与 slog.Level 兼容（Debug=-4、Info=0、Warn=4、Error=8），
// 日志系统只输出大于等于当前设定级别的消息。
type Level int8

const (
	// LevelTrace 追踪级别，最详细的调试信息，生产环境通常不启用。
	LevelTrace Level = -8

	// LevelDebug 调试级别，用于开发阶段的详细信息输出。
	LevelDebug Level = -4

	// LevelInfo 信息级别，用于常规运行状态记录，是生产环境推荐的默认级别。
	LevelInfo Level = 0

	// LevelWarn 警告级别，用于需要关注但不影响正常运行的事件。
	LevelWarn Level = 4

	// LevelError 错误级别，用于记录失败的操作。
	LevelError Level = 8

	// LevelFatal 致命级别，记录后程序以状态码 1 退出。
	LevelFatal Level = 12
)

// levelNames 将日志级别映射到可读的字符串名称。
var levelNames = map[Level]string{
	LevelTrace: "TRACE",
	LevelDebug: "DEBUG",
	LevelInfo:  "INFO",
	LevelWarn:  "WARN",
	LevelError: "ERROR",
	LevelFatal: "FATAL",
}

// String 返回日志级别的可读字符串表示，未知级别返回 "UNKNOWN"。
func (l Level) String() string {
	if name, ok := levelNames[l]; ok {
		return name
	}
	return "UNKNOWN"
}

// ParseLevel 将字符串解析为日志级别（不区分大小写，允许首尾空格）。
// 支持 trace、debug、info、warn/warning、error、fatal；
// 无法识别的输入默认返回 LevelInfo，避免配置笔误导致服务无法启动。
func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace":
		return LevelTrace
	case "debug":
		return LevelDebug
	case "info":
		return LevelInfo
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	case "fatal":
		return LevelFatal
	default:
		return LevelInfo
	}
}
