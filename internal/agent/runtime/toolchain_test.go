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
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/log"
)

// toolchainTestTool 是工具链切分测试的占位工具（test 命名空间，risk=low）。
type toolchainTestTool struct {
	// name 工具全名（如 test.t00）。
	name string
}

// Def 返回该占位工具的契约。
func (t toolchainTestTool) Def() tool.Definition {
	return tool.Definition{
		Name: t.name, Namespace: "test", Description: "占位工具 " + t.name,
		ParametersJSON: `{"type":"object","properties":{},"additionalProperties":false}`,
		Annotations:    tool.Annotations{Risk: tool.RiskLow},
	}
}

// Run 执行：返回固定成功结果。
func (t toolchainTestTool) Run(_ context.Context, _ string) (string, error) {
	return tool.OKResult(map[string]any{"ok": true})
}

// newToolchainService 创建带 n 个占位工具（test.t00..）的 Service
// （只装配 buildToolchain 依赖的注册表/执行器/日志器）。
func newToolchainService(t *testing.T, n int) *Service {
	t.Helper()
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
	reg := tool.NewRegistry()
	for i := 0; i < n; i++ {
		if err := reg.Register(toolchainTestTool{name: fmt.Sprintf("test.t%02d", i)}); err != nil {
			t.Fatalf("注册工具失败: %v", err)
		}
	}
	return NewService(Deps{
		Logger:   logger,
		Registry: reg,
		Executor: tool.NewExecutor(reg, tool.ExecutorOptions{Logger: logger}),
	})
}

// toolNames 返回内核工具清单的模型侧名字集（净化名）。
func toolNames(t *testing.T, tools []kernel.Tool) []string {
	t.Helper()
	names := make([]string, 0, len(tools))
	for _, tt := range tools {
		info, err := tt.Info(context.Background())
		if err != nil {
			t.Fatalf("工具 Info 失败: %v", err)
		}
		names = append(names, info.Name)
	}
	return names
}

// TestBuildToolchainDirectInjection 验证 tool_search 关闭时 pinned 只作
// 元信息、全部工具稳定直通。
func TestBuildToolchainDirectInjection(t *testing.T) {
	prof := &profile.Profile{
		Name: "leader",
		Tools: profile.ToolsConfig{
			Namespaces: []string{"test.*"},
			Pinned:     []string{"test.t00"},
		},
	}

	// tool_search 关闭：10 个工具全部直通，无动态集。
	svc := newToolchainService(t, 10)
	tools, dynamic, safety, err := svc.buildToolchain(prof, prof.Name)
	if err != nil {
		t.Fatalf("buildToolchain 失败: %v", err)
	}
	if len(tools) != 10 || dynamic != nil {
		t.Errorf("关闭时应 10 个直通且无动态集，实际: tools=%d dynamic=%d", len(tools), len(dynamic))
	}
	if safety == nil {
		t.Errorf("命中清单非空时 safety 应构建")
	}

}

// TestBuildToolchainSearchSplit 验证 ToolSearch 显式切分形态：开启时 pinned
// 直通、其余进动态集；safety 仍以全量清单构建。
func TestBuildToolchainSearchSplit(t *testing.T) {
	prof := &profile.Profile{
		Name: "leader",
		Tools: profile.ToolsConfig{
			Namespaces: []string{"test.*"},
			Pinned:     []string{"test.t00"},
			ToolSearch: true,
		},
		Limits: profile.Limits{ContextTokens: 100},
	}
	svc := newToolchainService(t, 10)
	tools, dynamic, safety, err := svc.buildToolchain(prof, prof.Name)
	if err != nil {
		t.Fatalf("buildToolchain 失败: %v", err)
	}

	// pinned 直通：只含 test.t00（净化名 test_t00）。
	names := toolNames(t, tools)
	if len(names) != 1 || names[0] != "test_t00" {
		t.Errorf("直通清单应只含 pinned 工具，实际: %v", names)
	}
	// 动态集：其余 9 个（不含 pinned）。
	dynamicNames := strings.Join(toolNames(t, dynamic), ",")
	if len(dynamic) != 9 || strings.Contains(dynamicNames, "test_t00") {
		t.Errorf("动态集应为 9 个非 pinned 工具，实际: %v", dynamicNames)
	}

	// safety 覆盖动态集：动态集里的工具调用不被 UNKNOWN_TOOL 拒绝
	// （risk=low 无审批，直接放行执行）。
	out, err := safety.WrapToolCall(context.Background(),
		kernel.ToolCallMeta{Name: "test.t05", CallID: "c1"}, `{}`,
		func(_ context.Context, _ string) (string, error) { return `{"ok":true}`, nil })
	if err != nil {
		t.Fatalf("动态集工具过门禁出错: %v", err)
	}
	if tool.IsErrorResult(out) {
		t.Errorf("动态集工具应照常过门禁，实际结果: %s", out)
	}
}

// TestBuildToolchainSmallSetDirect 验证允许 ToolSearch 不等于强制检索：小工具
// 集 schema 未超过上下文 10% 时全部直接装配，避免额外模型轮次。
func TestBuildToolchainSmallSetDirect(t *testing.T) {
	prof := &profile.Profile{
		Name: "leader",
		Tools: profile.ToolsConfig{
			Namespaces: []string{"test.*"},
			Pinned:     []string{"test.t00"},
			ToolSearch: true,
		},
		Limits: profile.Limits{ContextTokens: 120000},
	}
	for _, count := range []int{2, 10} {
		svc := newToolchainService(t, count)
		tools, dynamic, _, err := svc.buildToolchain(prof, prof.Name)
		if err != nil {
			t.Fatalf("count=%d buildToolchain 失败: %v", count, err)
		}
		if names := toolNames(t, tools); len(names) != count || len(dynamic) != 0 {
			t.Errorf("count=%d 小工具集应全部直通: tools=%v dynamic=%v", count, names, dynamic)
		}
	}
}

// TestBuildToolchainFallbackThreshold 验证无法取得上下文容量时的后备规则：
// 动态工具 10 个仍直通，超过 10 个才进入 Eino ToolSearch；无论分区如何，
// 原有工具都仍存在于“直通 + 可检索”的可调用全集中。
func TestBuildToolchainFallbackThreshold(t *testing.T) {
	prof := &profile.Profile{Name: "leader", Tools: profile.ToolsConfig{
		Namespaces: []string{"test.*"}, Pinned: []string{"test.t00"}, ToolSearch: true,
	}}
	for _, tc := range []struct {
		count       int
		wantDirect  int
		wantDynamic int
	}{{count: 11, wantDirect: 11}, {count: 12, wantDirect: 1, wantDynamic: 11}} {
		svc := newToolchainService(t, tc.count)
		tools, dynamic, _, err := svc.buildToolchain(prof, prof.Name)
		if err != nil {
			t.Fatalf("count=%d buildToolchain 失败: %v", tc.count, err)
		}
		if len(tools) != tc.wantDirect || len(dynamic) != tc.wantDynamic {
			t.Errorf("count=%d 分区不符: direct=%d dynamic=%d", tc.count, len(tools), len(dynamic))
		}
		allNames := append(toolNames(t, tools), toolNames(t, dynamic)...)
		if len(allNames) != tc.count || !containsString(allNames, "test_t01") {
			t.Errorf("count=%d 原有工具可调用全集不完整: %v", tc.count, allNames)
		}
	}
}

// TestBuildToolchainPinnedMiss 验证 pinned 未命中命名空间边界：记 WARN
// 忽略（配置笔误不阻断装配），全部工具按动态集处理。
func TestBuildToolchainPinnedMiss(t *testing.T) {
	prof := &profile.Profile{
		Name: "leader",
		Tools: profile.ToolsConfig{
			Namespaces: []string{"test.*"},
			Pinned:     []string{"ghost.missing"},
			ToolSearch: true,
		},
		Limits: profile.Limits{ContextTokens: 100},
	}
	// 10 个工具全进动态集（pinned 未命中）→ 全部可检索。
	svc := newToolchainService(t, 10)
	tools, dynamic, _, err := svc.buildToolchain(prof, prof.Name)
	if err != nil {
		t.Fatalf("buildToolchain 失败: %v", err)
	}
	if len(tools) != 0 || len(dynamic) != 10 {
		t.Errorf("pinned 未命中应全部进动态集，实际: tools=%d dynamic=%d", len(tools), len(dynamic))
	}
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
