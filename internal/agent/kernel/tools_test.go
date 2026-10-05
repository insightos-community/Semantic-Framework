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

package kernel

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"

	tooldef "insightos.cn/semantic-framework/internal/tool"
)

// TestSafeToolName 验证工具名净化规则：非 [a-zA-Z0-9_-] 字符替换为 '_'。
func TestSafeToolName(t *testing.T) {
	cases := map[string]string{
		"artifact.put":  "artifact_put",
		"system.time":   "system_time",
		"artifact.get":  "artifact_get",
		"get_weather":   "get_weather", // 已合法：保持不变
		"ns.sub.action": "ns_sub_action",
		"a/b":           "a_b",
		"a-b_c.d":       "a-b_c_d",
	}
	for in, want := range cases {
		if got := SafeToolName(in); got != want {
			t.Errorf("SafeToolName(%q) = %q，应为 %q", in, got, want)
		}
	}
}

// TestAdaptToolsSanitizesAndDetectsCollision 验证适配期净化与重名冲突检测。
func TestAdaptToolsSanitizesAndDetectsCollision(t *testing.T) {
	defs := []tooldef.Definition{{
		Name:           "artifact.put",
		Namespace:      "artifact",
		Description:    "保存产物",
		ParametersJSON: `{"type":"object","properties":{}}`,
	}}
	tools, err := AdaptTools(defs, nil)
	if err != nil {
		t.Fatalf("AdaptTools 失败: %v", err)
	}
	info, err := tools[0].Info(context.Background())
	if err != nil {
		t.Fatalf("Info 失败: %v", err)
	}
	if info.Name != "artifact_put" {
		t.Errorf("模型侧工具名应净化为 artifact_put，实际: %q", info.Name)
	}
	if info.Desc == "" || info.Desc[0] != '[' {
		t.Errorf("描述应保留原始全名前缀，实际: %q", info.Desc)
	}

	// 净化后重名：artifact.put 与 artifact_put 冲突，适配期必须报错。
	clash := []tooldef.Definition{
		{Name: "artifact.put", ParametersJSON: `{"type":"object"}`},
		{Name: "artifact_put", ParametersJSON: `{"type":"object"}`},
	}
	if _, err := AdaptTools(clash, nil); err == nil {
		t.Error("净化后重名应在适配期报错，实际通过")
	}
}

// guardSpy 记录门禁收到的 ToolCallMeta（验证名称还原）。
type guardSpy struct {
	gotName string
}

func (g *guardSpy) WrapToolCall(_ context.Context, meta ToolCallMeta, _ string, next ToolCallEndpoint) (string, error) {
	g.gotName = meta.Name
	return next(context.Background(), "{}")
}

// TestSafetyMiddlewareRestoresOriginalName 验证 safety middleware 把模型侧
// 净化名还原为契约原名后再交给门禁（approval_required 按原名匹配）。
func TestSafetyMiddlewareRestoresOriginalName(t *testing.T) {
	spy := &guardSpy{}
	mw := &safetyMiddleware{
		guard:   spy,
		nameMap: map[string]string{"artifact_put": "artifact.put"},
	}
	called := false
	endpoint, err := mw.WrapInvokableToolCall(context.Background(),
		func(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
			called = true
			return "ok", nil
		},
		&adk.ToolContext{Name: "artifact_put", CallID: "call-1"})
	if err != nil {
		t.Fatalf("WrapInvokableToolCall 失败: %v", err)
	}
	if _, err := endpoint(context.Background(), "{}"); err != nil {
		t.Fatalf("端点执行失败: %v", err)
	}
	if !called {
		t.Fatal("放行后端点应被调用")
	}
	if spy.gotName != "artifact.put" {
		t.Errorf("门禁应收到原始名 artifact.put，实际: %q", spy.gotName)
	}

	// 未映射的名字原样透传（内置工具名本就合法时不改写）。
	mw2 := &safetyMiddleware{guard: spy, nameMap: map[string]string{}}
	endpoint2, err := mw2.WrapInvokableToolCall(context.Background(),
		func(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
			return "ok", nil
		},
		&adk.ToolContext{Name: "system_time", CallID: "call-2"})
	if err != nil {
		t.Fatalf("WrapInvokableToolCall 失败: %v", err)
	}
	if _, err := endpoint2(context.Background(), "{}"); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("端点执行失败: %v", err)
	}
	if spy.gotName != "system_time" {
		t.Errorf("未映射名字应原样透传 system_time，实际: %q", spy.gotName)
	}
}
