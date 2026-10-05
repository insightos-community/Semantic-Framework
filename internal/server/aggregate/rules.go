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

package aggregate

import (
	"encoding/json"
	"strings"

	"insightos.cn/semantic-framework/internal/server/ws"
)

// alertLevelCritical 是 alert 频道 level 字段进主流（critical）的下限
// 数值 level≥3 作为 critical 进入主视图，较低级别可以折叠。
const alertLevelCritical = 3

// channelDefaults 是各频道的基础分级（规则表 v1 的静态部分）。
// 未列入的频道默认 normal（宁可见、不可漏：未知事件进主流比静默折叠安全）。
var channelDefaults = map[ws.Channel]ws.Importance{
	ws.ChannelDialogue: ws.ImportanceNormal,
	ws.ChannelTrace:    ws.ImportanceLow,
	ws.ChannelArtifact: ws.ImportanceNormal,
}

// 告警级别的字符串取值映射（v1 兼容数值与字符串两种 level 表达：
// 域模块尚未统一告警 schema，两种都认，数值优先）。
var alertSeverity = map[string]ws.Importance{
	"critical": ws.ImportanceCritical,
	"fatal":    ws.ImportanceCritical,
	"high":     ws.ImportanceCritical,
	"warning":  ws.ImportanceNormal,
	"warn":     ws.ImportanceNormal,
	"medium":   ws.ImportanceNormal,
	"info":     ws.ImportanceLow,
	"low":      ws.ImportanceLow,
	"debug":    ws.ImportanceLow,
}

// Classify 按分级规则表 v1 判定事件的重要级别（纯函数，可单测）：
//   - interaction 一律 critical：审批请求必须进主流，任何情况下不可被
//     折叠或错过（审批超时按拒绝处理，错过的代价是危险操作被默认否决）；
//   - alert 按 payload.level 分级：数值 ≥3 → critical，1-2 → low
//     （02 §3.4）；字符串按 alertSeverity 映射；缺失/不可解析 → normal
//     （告警宁可见不可漏，不因 schema 缺漏静默降级）；
//   - 其余频道按 channelDefaults，未知频道 normal。
func Classify(channel ws.Channel, payload any) ws.Importance {
	if channel == ws.ChannelInteraction {
		return ws.ImportanceCritical
	}
	if channel == ws.ChannelAlert {
		return classifyAlert(payload)
	}
	if imp, ok := channelDefaults[channel]; ok {
		return imp
	}
	return ws.ImportanceNormal
}

// classifyAlert 解析 alert 负载的 level 字段并映射分级。
// payload 预期为对象（map 或 json.RawMessage），level 取数值或字符串。
func classifyAlert(payload any) ws.Importance {
	fields, ok := payloadFields(payload)
	if !ok {
		return ws.ImportanceNormal
	}
	level, exists := fields["level"]
	if !exists {
		return ws.ImportanceNormal
	}
	switch v := level.(type) {
	case float64: // JSON 数值的统一反序列化形态
		return classifyAlertLevel(int(v))
	case int:
		return classifyAlertLevel(v)
	case string:
		if imp, ok := alertSeverity[strings.ToLower(v)]; ok {
			return imp
		}
	}
	return ws.ImportanceNormal
}

// classifyAlertLevel 按数值级别映射：≥3 critical，1-2 low，其余 normal。
func classifyAlertLevel(level int) ws.Importance {
	if level >= alertLevelCritical {
		return ws.ImportanceCritical
	}
	if level >= 1 {
		return ws.ImportanceLow
	}
	return ws.ImportanceNormal
}

// payloadFields 把负载解为字段 map；非对象负载（数组/标量/nil）返回 false。
func payloadFields(payload any) (map[string]any, bool) {
	switch p := payload.(type) {
	case map[string]any:
		return p, true
	case json.RawMessage:
		var fields map[string]any
		if err := json.Unmarshal(p, &fields); err == nil {
			return fields, true
		}
	case []byte:
		var fields map[string]any
		if err := json.Unmarshal(p, &fields); err == nil {
			return fields, true
		}
	}
	return nil, false
}
