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

package bootstrap

import (
	"testing"

	"insightos.cn/semantic-framework/internal/agent/kernel"
)

// TestMaterializeProviderDefaults 验证 settings PATCH 时为新增 OpenAI
// 兼容端点物化默认 options：只有新增端点被注入，显式 options、已有端点
// 和非 openai 驱动都不被触碰。
func TestMaterializeProviderDefaults(t *testing.T) {
	cur := map[string]any{
		"llm": map[string]any{
			"providers": map[string]any{
				"existing": map[string]any{
					"component": "openai",
					"model":     "m0",
					"options":   map[string]any{"temperature": 0.7},
				},
			},
		},
	}
	merged := map[string]any{
		"llm": map[string]any{
			"providers": map[string]any{
				"existing": map[string]any{
					"component": "openai",
					"model":     "m0",
					"options":   map[string]any{"temperature": 0.7},
				},
				// 新增端点：应被物化默认 options。
				"new-bare": map[string]any{
					"component": "openai",
					"model":     "m1",
				},
				// 新增端点但 patch 显式带了 options：应完全尊重。
				"new-explicit": map[string]any{
					"component": "openai",
					"model":     "m2",
					"options":   map[string]any{"max_tokens": 4096},
				},
				// 新增端点显式带空 options：同样视为显式配置，不注入。
				"new-empty-options": map[string]any{
					"component": "openai",
					"model":     "m3",
					"options":   map[string]any{},
				},
				// 新增 claude 端点：options 白名单不含 timeout_seconds，不物化。
				"new-claude": map[string]any{
					"component": "claude",
					"model":     "m4",
				},
			},
		},
	}

	materializeProviderDefaults(cur, merged)

	providers := treeProviders(merged)

	if got := providers["new-bare"].(map[string]any)["options"]; got == nil {
		t.Fatal("新增 openai 端点应被物化默认 options")
	} else {
		options := got.(map[string]any)
		if options["timeout_seconds"] != kernel.DefaultModelRequestTimeoutSeconds {
			t.Errorf("timeout_seconds = %v，期望 %d", options["timeout_seconds"], kernel.DefaultModelRequestTimeoutSeconds)
		}
		if options["max_tokens"] != kernel.DefaultMaxOutputTokens {
			t.Errorf("max_tokens = %v，期望 %d", options["max_tokens"], kernel.DefaultMaxOutputTokens)
		}
	}

	if got := providers["new-explicit"].(map[string]any)["options"].(map[string]any); len(got) != 1 || got["max_tokens"] != 4096 {
		t.Errorf("显式 options 应完全保留，实际: %v", got)
	}
	if got := providers["new-empty-options"].(map[string]any)["options"].(map[string]any); len(got) != 0 {
		t.Errorf("显式空 options 应保持为空，实际: %v", got)
	}
	if got := providers["new-claude"].(map[string]any)["options"]; got != nil {
		t.Errorf("claude 端点不应被物化，实际: %v", got)
	}
	if got := providers["existing"].(map[string]any)["options"].(map[string]any); len(got) != 1 || got["temperature"] != 0.7 {
		t.Errorf("已有端点的 options 不应被触碰，实际: %v", got)
	}
}

// TestTreeProvidersMissing 验证 llm 段缺失时返回空表而不 panic。
func TestTreeProvidersMissing(t *testing.T) {
	if got := treeProviders(map[string]any{}); len(got) != 0 {
		t.Errorf("空树应返回空 providers，实际: %v", got)
	}
	if got := treeProviders(map[string]any{"llm": "not-a-map"}); len(got) != 0 {
		t.Errorf("llm 非映射应返回空 providers，实际: %v", got)
	}
}
