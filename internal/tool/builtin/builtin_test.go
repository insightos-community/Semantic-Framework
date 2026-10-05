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

package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/log"
)

// newTestRegistry 创建注册好全部内置工具的注册表（临时库）。
func newTestRegistry(t *testing.T) (*tool.Registry, *store.Store) {
	t.Helper()
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
	st := storetest.OpenMigrated(t, logger)

	reg := tool.NewRegistry()
	if err := RegisterAll(reg, st); err != nil {
		t.Fatalf("RegisterAll 失败: %v", err)
	}
	return reg, st
}

// runTool 按名执行工具并解析结果 envelope。
func runTool(t *testing.T, reg *tool.Registry, name, argsJSON string) map[string]any {
	t.Helper()
	tt, ok := reg.Get(name)
	if !ok {
		t.Fatalf("工具 %q 未注册", name)
	}
	out, err := tt.Run(context.Background(), argsJSON)
	if err != nil {
		t.Fatalf("工具 %q 执行失败: %v", name, err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("工具 %q 结果不是合法 JSON: %q", name, out)
	}
	return parsed
}

// runToolErr 按名执行工具并断言返回 *tool.Error。
func runToolErr(t *testing.T, reg *tool.Registry, name, argsJSON string) *tool.Error {
	t.Helper()
	tt, _ := reg.Get(name)
	_, err := tt.Run(context.Background(), argsJSON)
	var terr *tool.Error
	if !errors.As(err, &terr) {
		t.Fatalf("工具 %q 应返回 *tool.Error，实际: %v", name, err)
	}
	return terr
}

// TestRegisterAllContracts 验证内置工具全部注册且契约合法：
// 命名空间、jsonschema 可解析、风险等级按约定。
func TestRegisterAllContracts(t *testing.T) {
	reg, _ := newTestRegistry(t)

	defs := reg.List()
	if len(defs) != 12 {
		t.Fatalf("应注册 12 个内置工具，实际: %d", len(defs))
	}

	riskByName := map[string]string{}
	for _, def := range defs {
		riskByName[def.Name] = def.Annotations.Risk

		// 契约完整性：命名空间 = 名字前缀；schema 是合法 JSON 且 object 根。
		// execute 是经产品契约确认的单名称工具，其名称本身也是权限命名空间；
		// 其余分组工具继续使用 namespace.action 形式。
		if def.Name != def.Namespace && !strings.HasPrefix(def.Name, def.Namespace+".") {
			t.Errorf("工具 %q 的命名空间 %q 与名字前缀不符", def.Name, def.Namespace)
		}
		var schema map[string]any
		if err := json.Unmarshal([]byte(def.ParametersJSON), &schema); err != nil {
			t.Errorf("工具 %q 的 ParametersJSON 不是合法 JSON: %v", def.Name, err)
		}
		if schema["type"] != "object" {
			t.Errorf("工具 %q 的 schema 根应为 object，实际: %v", def.Name, schema["type"])
		}
	}

	wantRisk := map[string]string{
		"system.time":       tool.RiskLow,
		"system.echo":       tool.RiskLow,
		"system.calc":       tool.RiskLow,
		"artifact.put":      tool.RiskHigh,
		"artifact.get":      tool.RiskMedium,
		"artifact.list":     tool.RiskLow,
		"artifact.register": tool.RiskHigh,
		"execute":           tool.RiskHigh,
		"execute_host":      tool.RiskHigh,
		"map.query":         tool.RiskLow,
		"plan.suggest":      tool.RiskLow,
		"interaction.ask":   tool.RiskLow,
	}
	for name, want := range wantRisk {
		if got := riskByName[name]; got != want {
			t.Errorf("工具 %q 风险等级应为 %q，实际: %q", name, want, got)
		}
	}
}

// TestSystemTime 验证 system.time 返回 RFC3339 时间。
func TestSystemTime(t *testing.T) {
	reg, _ := newTestRegistry(t)
	res := runTool(t, reg, "system.time", `{}`)
	if res["ok"] != true {
		t.Fatalf("应成功，实际: %+v", res)
	}
	data := res["data"].(map[string]any)
	ts, _ := data["time"].(string)
	if ts == "" || !strings.Contains(ts, "T") || !strings.HasSuffix(ts, "Z") {
		t.Errorf("时间应为 RFC3339 UTC，实际: %q", ts)
	}
}

// TestSystemEcho 验证 system.echo 回显。
func TestSystemEcho(t *testing.T) {
	reg, _ := newTestRegistry(t)
	res := runTool(t, reg, "system.echo", `{"text":"你好"}`)
	data := res["data"].(map[string]any)
	if data["text"] != "你好" {
		t.Errorf("应回显文本，实际: %+v", data)
	}
}

// TestSystemCalc 验证 system.calc 四则运算与除零结构化错误。
func TestSystemCalc(t *testing.T) {
	reg, _ := newTestRegistry(t)

	cases := []struct {
		args string
		want float64
	}{
		{`{"a":1,"op":"+","b":2}`, 3},
		{`{"a":5,"op":"-","b":2}`, 3},
		{`{"a":3,"op":"*","b":4}`, 12},
		{`{"a":7,"op":"/","b":2}`, 3.5},
	}
	for _, c := range cases {
		res := runTool(t, reg, "system.calc", c.args)
		data := res["data"].(map[string]any)
		if data["result"] != c.want {
			t.Errorf("calc(%s) 应为 %v，实际: %+v", c.args, c.want, data)
		}
	}

	terr := runToolErr(t, reg, "system.calc", `{"a":1,"op":"/","b":0}`)
	if terr.Code != "DIVIDE_BY_ZERO" || terr.Retryable {
		t.Errorf("除零应返回 DIVIDE_BY_ZERO 不可重试，实际: %+v", terr)
	}
	terr = runToolErr(t, reg, "system.calc", `{"a":1,"op":"%","b":2}`)
	if terr.Code != "UNKNOWN_OPERATOR" {
		t.Errorf("非法运算符应返回 UNKNOWN_OPERATOR，实际: %+v", terr)
	}
}

// TestArtifactPutGet 验证 artifact.put 写入 → artifact.get 读回的闭环。
func TestArtifactPutGet(t *testing.T) {
	reg, _ := newTestRegistry(t)

	putRes := runTool(t, reg, "artifact.put",
		`{"content":"# 报告\n正文内容","media_type":"text/markdown","summary":"测试报告","metadata":{"source":"test"}}`)
	data := putRes["data"].(map[string]any)
	artifactID, _ := data["artifact_id"].(string)
	if artifactID == "" || data["uri"] != "artifact://"+artifactID || data["summary"] != "测试报告" {
		t.Fatalf("put 结果不符: %+v", data)
	}

	getRes := runTool(t, reg, "artifact.get", `{"artifact_id":"`+artifactID+`"}`)
	got := getRes["data"].(map[string]any)
	if got["content"] != "# 报告\n正文内容" || got["media_type"] != "text/markdown" ||
		got["summary"] != "测试报告" {
		t.Errorf("get 结果不符: %+v", got)
	}
	md, _ := got["metadata"].(map[string]any)
	if md["source"] != "test" {
		t.Errorf("metadata 不符: %+v", got["metadata"])
	}
}

// TestArtifactPutDefaults 验证 artifact.put 的缺省值：media_type 默认
// text/plain，summary 缺省自动截取内容开头。
func TestArtifactPutDefaults(t *testing.T) {
	reg, st := newTestRegistry(t)

	long := strings.Repeat("长", 100)
	res := runTool(t, reg, "artifact.put", `{"content":"`+long+`"}`)
	data := res["data"].(map[string]any)
	summary, _ := data["summary"].(string)
	if !strings.HasSuffix(summary, "…") || len([]rune(summary)) > 51 {
		t.Errorf("自动摘要应截断到 50 字加省略号，实际: %q", summary)
	}

	a, err := st.GetArtifactMeta(data["artifact_id"].(string))
	if err != nil {
		t.Fatalf("GetArtifactMeta 失败: %v", err)
	}
	if a.MediaType != "text/plain" {
		t.Errorf("media_type 应默认 text/plain，实际: %q", a.MediaType)
	}
}

// TestArtifactGetNotFound 验证读取缺失产物的结构化错误。
func TestArtifactGetNotFound(t *testing.T) {
	reg, _ := newTestRegistry(t)
	terr := runToolErr(t, reg, "artifact.get", `{"artifact_id":"art-missing"}`)
	if terr.Code != "ARTIFACT_NOT_FOUND" {
		t.Errorf("缺失产物应返回 ARTIFACT_NOT_FOUND，实际: %+v", terr)
	}
}

// TestArtifactList 验证 artifact.list 的列举闭环：空列表、写入后按创建
// 时间倒序返回元数据（不含内容本体）、limit 上限生效。
func TestArtifactList(t *testing.T) {
	reg, st := newTestRegistry(t)

	dataOf := func(res map[string]any) map[string]any {
		t.Helper()
		okFlag, _ := res["ok"].(bool)
		if !okFlag {
			t.Fatalf("工具结果应为 ok=true: %v", res)
		}
		data, ok := res["data"].(map[string]any)
		if !ok {
			t.Fatalf("工具结果缺 data 字段: %v", res)
		}
		return data
	}

	// 空列表。
	res := dataOf(runTool(t, reg, "artifact.list", `{}`))
	if total, _ := res["total"].(float64); total != 0 {
		t.Fatalf("空产物列表应返回 total=0，实际: %v", res)
	}

	// 写入两条后列举：倒序（后写在前）且字段齐全、不含 content。
	if _, err := st.PutArtifact("text/plain", "第一条", "{}", []byte("c1")); err != nil {
		t.Fatalf("写入产物1失败: %v", err)
	}
	if _, err := st.PutArtifact("text/plain", "第二条", "{}", []byte("c2")); err != nil {
		t.Fatalf("写入产物2失败: %v", err)
	}
	res = dataOf(runTool(t, reg, "artifact.list", `{"limit":10}`))
	if total, _ := res["total"].(float64); total != 2 {
		t.Fatalf("应有 2 条产物，实际: %v", res)
	}
	items, ok := res["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items 应有 2 条，实际: %v", res)
	}
	first, _ := items[0].(map[string]any)
	if first["summary"] != "第二条" {
		t.Errorf("应按创建时间倒序（后写在前），实际首条: %v", first["summary"])
	}
	if _, hasContent := first["content"]; hasContent {
		t.Errorf("列表不应返回内容本体: %v", first)
	}

	// limit=1 只回一条。
	res = dataOf(runTool(t, reg, "artifact.list", `{"limit":1}`))
	if total, _ := res["total"].(float64); total != 1 {
		t.Fatalf("limit=1 应只返回 1 条，实际: %v", res)
	}
}
