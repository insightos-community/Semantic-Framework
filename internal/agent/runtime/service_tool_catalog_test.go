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

package runtime

import (
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// TestMarshalEinoToolSchema 验证额外 Eino 工具通过公开转换接口生成真实
// JSON Schema，而不是把内部字段未导出的 ParamsOneOf 静默序列化成空对象。
func TestMarshalEinoToolSchema(t *testing.T) {
	params := schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
		"task": {Type: schema.String, Desc: "任务说明", Required: true},
	})
	raw, err := marshalEinoToolSchema(params)
	if err != nil {
		t.Fatalf("转换参数 Schema 失败: %v", err)
	}

	var document struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("参数 Schema 不是合法 JSON: %v（%s）", err, raw)
	}
	if document.Type != "object" || document.Properties["task"] == nil {
		t.Fatalf("参数 Schema 缺少 task 字段: %s", raw)
	}
	if len(document.Required) != 1 || document.Required[0] != "task" {
		t.Fatalf("task 应为必填参数: %s", raw)
	}

	withoutParams, err := marshalEinoToolSchema(nil)
	if err != nil || withoutParams != nil {
		t.Fatalf("无参数工具不应生成伪 Schema: raw=%s err=%v", withoutParams, err)
	}
}
