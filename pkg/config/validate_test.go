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

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadYAMLString 把 yaml 文本写入临时文件并调用 Load，返回聚合错误文本。
func loadYAMLString(t *testing.T, content string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "semantic-server.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入临时配置文件失败: %v", err)
	}
	_, err := Load(path)
	return err
}

// TestValidateUnknownKey 验证未知键被拒绝，报错携带完整 yaml 路径。
func TestValidateUnknownKey(t *testing.T) {
	err := loadYAMLString(t, "server:\n  http-addr: \":19090\"\n")
	if err == nil {
		t.Fatal("未知键 server.http-addr 应导致 Load 失败")
	}
	if !strings.Contains(err.Error(), "server.http-addr: 未知配置键") {
		t.Errorf("错误应指出未知键路径，实际: %v", err)
	}
}

// TestValidateUnknownKeyInProvider 验证 llm.providers 动态条目内部的
// 未知键同样被拒绝（条目名本身不参与白名单比对）。
func TestValidateUnknownKeyInProvider(t *testing.T) {
	err := loadYAMLString(t, "llm:\n  providers:\n    mock:\n      componet: mock\n")
	if err == nil {
		t.Fatal("provider 条目内的未知键应导致 Load 失败")
	}
	if !strings.Contains(err.Error(), "llm.providers.mock.componet: 未知配置键") {
		t.Errorf("错误应指出条目内未知键路径，实际: %v", err)
	}
}

// TestValidateTypeError 验证类型错误被拒绝：无法解码到字段类型即报错。
func TestValidateTypeError(t *testing.T) {
	cases := map[string]string{
		"时长格式非法":   "server:\n  read_timeout: 10x\n",
		"整数赋给字符串":  "server:\n  http_addr: 8080\n",
		"标量赋给结构体段": "store: sqlite\n",
	}
	for name, content := range cases {
		err := loadYAMLString(t, content)
		if err == nil {
			t.Errorf("%s：应导致 Load 失败", name)
			continue
		}
		if !strings.Contains(err.Error(), "类型错误") {
			t.Errorf("%s：错误应标明类型错误，实际: %v", name, err)
		}
	}
}

// TestValidateAggregatesProblems 验证多处问题被一次性聚合返回，
// 且错误可经 errors.As 还原为 ValidationError。
func TestValidateAggregatesProblems(t *testing.T) {
	err := loadYAMLString(t, "server:\n  http-addr: \":1\"\nlog:\n  levl: debug\nstore:\n  driver: 1\n")
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("错误应可还原为 ValidationError，实际: %T", err)
	}
	if len(verr.Problems) != 3 {
		t.Fatalf("应聚合 3 处问题，实际 %d 处: %v", len(verr.Problems), verr.Problems)
	}
}

// TestValidateExistingConfig 验证仓内真实配置文件必须原样通过校验。
func TestValidateExistingConfig(t *testing.T) {
	if _, err := Load(filepath.Join("..", "..", "configs", "semantic-server.yaml")); err != nil {
		t.Fatalf("configs/semantic-server.yaml 必须通过校验: %v", err)
	}
}

// TestValidateNewProviderPasses 验证新增的动态 provider 条目（字段合法）可通过校验。
func TestValidateNewProviderPasses(t *testing.T) {
	content := `
llm:
  providers:
    qwen-max:
      component: openai
      base_url: "https://dashscope.aliyuncs.com/compatible-mode/v1"
      model: "qwen-max"
      capabilities: [text, tool_call]
      price: {prompt: 0.002, completion: 0.006}
`
	if err := loadYAMLString(t, content); err != nil {
		t.Fatalf("合法的新 provider 条目应通过校验: %v", err)
	}
}

// TestValidateUnknownKeyInMCPServers 验证 mcp_servers 列表元素内部的
// 未知键同样被拒绝（fail-closed），报错路径带元素下标。
func TestValidateUnknownKeyInMCPServers(t *testing.T) {
	err := loadYAMLString(t, "mcp_servers:\n  - name: map\n    transport: http\n    endpiont: \"http://x/mcp\"\n")
	if err == nil {
		t.Fatal("mcp_servers 条目内的未知键应导致 Load 失败")
	}
	if !strings.Contains(err.Error(), "mcp_servers[0].endpiont: 未知配置键") {
		t.Errorf("错误应指出条目内未知键路径，实际: %v", err)
	}
}

// TestValidateMCPServersTypeError 验证 mcp_servers 不是列表时被拒绝。
func TestValidateMCPServersTypeError(t *testing.T) {
	err := loadYAMLString(t, "mcp_servers: not-a-list\n")
	if err == nil {
		t.Fatal("mcp_servers 为标量时应导致 Load 失败")
	}
	if !strings.Contains(err.Error(), "mcp_servers") {
		t.Errorf("错误应指出 mcp_servers 路径，实际: %v", err)
	}
}
