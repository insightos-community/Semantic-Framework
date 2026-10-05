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

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"insightos.cn/semantic-framework/internal/tool"
)

// fakeTool 是最小 tool.Tool 实现（目录视图测试用）。
type fakeTool struct {
	def tool.Definition
}

// Def 返回工具契约。
func (f *fakeTool) Def() tool.Definition { return f.def }

// Run 不会被目录端点调用。
func (f *fakeTool) Run(context.Context, string) (string, error) { return "", nil }

// TestListToolsBuiltinOnly 验证无 MCP 目录时的响应：只含 builtin 组，
// 契约字段完整（名称/命名空间/描述/风险/schema/health 恒 healthy）。
func TestListToolsBuiltinOnly(t *testing.T) {
	registry := tool.NewRegistry()
	err := registry.Register(&fakeTool{def: tool.Definition{
		Name: "artifact.put", Namespace: "artifact", Description: "写入产物",
		ParametersJSON: `{"type":"object","properties":{"content":{"type":"string"}}}`,
		Annotations:    tool.Annotations{Risk: tool.RiskHigh},
	}})
	if err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}

	h := NewToolsHandler(registry, nil)
	rec := httptest.NewRecorder()
	h.HandleListTools(rec, httptest.NewRequest(http.MethodGet, "/api/v1/tools", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际: %d（%s）", rec.Code, rec.Body.String())
	}

	var resp struct {
		Sources []struct {
			Kind  string `json:"kind"`
			Tools []struct {
				Name      string          `json:"name"`
				Namespace string          `json:"namespace"`
				Risk      string          `json:"risk"`
				Health    string          `json:"health"`
				Schema    json.RawMessage `json:"schema"`
				Server    string          `json:"server"`
			} `json:"tools"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(resp.Sources) != 1 || resp.Sources[0].Kind != "builtin" {
		t.Fatalf("无 MCP 目录时应只含 builtin 组，实际: %+v", resp.Sources)
	}
	tools := resp.Sources[0].Tools
	if len(tools) != 1 {
		t.Fatalf("builtin 组应含 1 个工具，实际: %d", len(tools))
	}
	got := tools[0]
	if got.Name != "artifact.put" || got.Namespace != "artifact" || got.Risk != "high" ||
		got.Health != "healthy" || got.Server != "" {
		t.Errorf("工具视图契约不符: %+v", got)
	}
	var schema map[string]any
	if err := json.Unmarshal(got.Schema, &schema); err != nil || schema["type"] != "object" {
		t.Errorf("schema 应为原始 JSON 透传，实际: %s（err=%v）", got.Schema, err)
	}
}

// TestListToolsEmpty 验证空注册表的响应：builtin 组为空数组（非 null，
// 前端不需要判空分支）。
func TestListToolsEmpty(t *testing.T) {
	h := NewToolsHandler(tool.NewRegistry(), nil)
	rec := httptest.NewRecorder()
	h.HandleListTools(rec, httptest.NewRequest(http.MethodGet, "/api/v1/tools", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际: %d", rec.Code)
	}
	var resp struct {
		Sources []struct {
			Kind  string          `json:"kind"`
			Tools json.RawMessage `json:"tools"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(resp.Sources) != 1 || string(resp.Sources[0].Tools) != "[]" {
		t.Errorf("空目录应返回空数组而非 null，实际: %+v", resp.Sources)
	}
}
