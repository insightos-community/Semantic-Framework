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
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadDefaults 验证空路径加载时返回代码内默认值，
// 默认值必须与 configs/semantic-server.yaml 保持一致。
func TestLoadDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") 不应返回错误: %v", err)
	}

	if cfg.Server.HTTPAddr != ":8080" {
		t.Errorf("server.http_addr 默认值应为 :8080，实际: %q", cfg.Server.HTTPAddr)
	}
	if cfg.Server.WSAddr != ":8081" {
		t.Errorf("server.ws_addr 默认值应为 :8081，实际: %q", cfg.Server.WSAddr)
	}
	if time.Duration(cfg.Server.ReadTimeout) != 10*time.Second {
		t.Errorf("server.read_timeout 默认值应为 10s，实际: %s", cfg.Server.ReadTimeout)
	}
	if time.Duration(cfg.Server.WriteTimeout) != 10*time.Second {
		t.Errorf("server.write_timeout 默认值应为 10s，实际: %s", cfg.Server.WriteTimeout)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("log.level 默认值应为 info，实际: %q", cfg.Log.Level)
	}
	if cfg.Store.Driver != "sqlite" {
		t.Errorf("store.driver 默认值应为 sqlite，实际: %q", cfg.Store.Driver)
	}
	if cfg.Store.SQLitePath != ".output/semantic.db" {
		t.Errorf("store.sqlite_path 默认值应为 .output/semantic.db，实际: %q", cfg.Store.SQLitePath)
	}
	if cfg.LLM.Default != "mock" {
		t.Errorf("llm.default 默认值应为 mock，实际: %q", cfg.LLM.Default)
	}
	if len(cfg.LLM.Providers) != 1 {
		t.Fatalf("llm.providers 安装默认值应仅含 mock，实际: %d", len(cfg.LLM.Providers))
	}
	if cfg.LLM.Providers["mock"].Component != "mock" {
		t.Errorf("mock 端点 component 应为 mock，实际: %+v", cfg.LLM.Providers["mock"])
	}
}

// TestLoadYAMLOverride 验证 yaml 文件覆盖默认值，未出现的字段保留默认值。
func TestLoadYAMLOverride(t *testing.T) {
	yamlContent := `
server:
  http_addr: ":19090"
  read_timeout: 3s
log:
  level: debug
`
	path := filepath.Join(t.TempDir(), "semantic-server.yaml")
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("写入临时配置文件失败: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) 不应返回错误: %v", path, err)
	}

	if cfg.Server.HTTPAddr != ":19090" {
		t.Errorf("server.http_addr 应被 yaml 覆盖为 :19090，实际: %q", cfg.Server.HTTPAddr)
	}
	if time.Duration(cfg.Server.ReadTimeout) != 3*time.Second {
		t.Errorf("server.read_timeout 应被 yaml 覆盖为 3s，实际: %s", cfg.Server.ReadTimeout)
	}
	// yaml 未出现的字段应保留默认值
	if time.Duration(cfg.Server.WriteTimeout) != 10*time.Second {
		t.Errorf("server.write_timeout 未在 yaml 中出现，应保留默认值 10s，实际: %s", cfg.Server.WriteTimeout)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log.level 应被 yaml 覆盖为 debug，实际: %q", cfg.Log.Level)
	}
	if cfg.Store.Driver != "sqlite" {
		t.Errorf("store.driver 未在 yaml 中出现，应保留默认值 sqlite，实际: %q", cfg.Store.Driver)
	}
}

// TestLoadEnvOverride 验证 SEMANTIC_ 前缀环境变量覆盖 yaml 与默认值。
func TestLoadEnvOverride(t *testing.T) {
	t.Setenv("SEMANTIC_SERVER_HTTP_ADDR", ":29090")
	t.Setenv("SEMANTIC_SERVER_WS_ADDR", ":29091")
	t.Setenv("SEMANTIC_SERVER_READ_TIMEOUT", "5s")
	t.Setenv("SEMANTIC_LOG_LEVEL", "warn")
	t.Setenv("SEMANTIC_STORE_SQLITE_PATH", "/tmp/test.db")

	yamlContent := `
server:
  http_addr: ":19090"
log:
  level: debug
`
	path := filepath.Join(t.TempDir(), "semantic-server.yaml")
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("写入临时配置文件失败: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) 不应返回错误: %v", path, err)
	}

	if cfg.Server.HTTPAddr != ":29090" {
		t.Errorf("server.http_addr 应被环境变量覆盖为 :29090，实际: %q", cfg.Server.HTTPAddr)
	}
	if cfg.Server.WSAddr != ":29091" {
		t.Errorf("server.ws_addr 应被环境变量覆盖为 :29091，实际: %q", cfg.Server.WSAddr)
	}
	if time.Duration(cfg.Server.ReadTimeout) != 5*time.Second {
		t.Errorf("server.read_timeout 应被环境变量覆盖为 5s，实际: %s", cfg.Server.ReadTimeout)
	}
	if cfg.Log.Level != "warn" {
		t.Errorf("log.level 应被环境变量覆盖为 warn，实际: %q", cfg.Log.Level)
	}
	if cfg.Store.SQLitePath != "/tmp/test.db" {
		t.Errorf("store.sqlite_path 应被环境变量覆盖，实际: %q", cfg.Store.SQLitePath)
	}
}

// TestLoadYAMLOverrideLLM 验证 yaml 中 llm 段可以覆盖 mock 的能力声明，
// 且代码默认值不会在安装配置背后隐式补入任何真实模型端点。
func TestLoadYAMLOverrideLLM(t *testing.T) {
	yamlContent := `
llm:
  default: mock
  providers:
    mock:
      component: mock
      model: "mock"
      capabilities: [text]
      price: {prompt: 0, completion: 0}
`
	path := filepath.Join(t.TempDir(), "semantic-server.yaml")
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("写入临时配置文件失败: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) 不应返回错误: %v", path, err)
	}
	if cfg.LLM.Default != "mock" {
		t.Errorf("llm.default 应被 yaml 覆盖为 mock，实际: %q", cfg.LLM.Default)
	}
	p := cfg.LLM.Providers["mock"]
	if p.Component != "mock" || p.Model != "mock" || len(p.Capabilities) != 1 || p.Capabilities[0] != "text" {
		t.Errorf("mock 端点应被 yaml 覆盖，实际: %+v", p)
	}
	if len(cfg.LLM.Providers) != 1 {
		t.Errorf("加载安装模板后不应隐式补入真实模型，实际: %+v", cfg.LLM.Providers)
	}
}

// TestLoadFileNotFound 验证配置文件不存在时返回明确的错误。
func TestLoadFileNotFound(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "not-exist.yaml")); err == nil {
		t.Error("配置文件不存在时 Load 应返回错误")
	}
}

// TestLoadInvalidEnvDuration 验证非法的时长环境变量值导致加载失败。
func TestLoadInvalidEnvDuration(t *testing.T) {
	t.Setenv("SEMANTIC_SERVER_READ_TIMEOUT", "not-a-duration")
	if _, err := Load(""); err == nil {
		t.Error("非法的 SEMANTIC_SERVER_READ_TIMEOUT 值应导致 Load 返回错误")
	}
}

// TestLoadYAMLOverrideMCPServers 验证 yaml 中 mcp_servers 段覆盖默认空列表，
// 条目字段（含 enabled/namespace/risk）完整解析。
func TestLoadYAMLOverrideMCPServers(t *testing.T) {
	yamlContent := `
mcp_servers:
  - name: semantic-map
    transport: http
    endpoint: "http://127.0.0.1:8082/mcp"
    enabled: true
    namespace: map
    risk: high
  - name: local-helper
    transport: stdio
    command: "/usr/local/bin/helper-mcp"
    args: ["--serve"]
    env: ["HELPER_TOKEN=abc"]
    enabled: false
    namespace: helper
`
	path := filepath.Join(t.TempDir(), "semantic-server.yaml")
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("写入临时配置文件失败: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) 不应返回错误: %v", path, err)
	}
	if len(cfg.MCPServers) != 2 {
		t.Fatalf("mcp_servers 应有 2 条，实际: %d", len(cfg.MCPServers))
	}
	m := cfg.MCPServers[0]
	if m.Name != "semantic-map" || m.Transport != "http" || m.Endpoint != "http://127.0.0.1:8082/mcp" ||
		!m.Enabled || m.Namespace != "map" || m.Risk != "high" {
		t.Errorf("http 条目解析不符: %+v", m)
	}
	h := cfg.MCPServers[1]
	if h.Name != "local-helper" || h.Transport != "stdio" || h.Command != "/usr/local/bin/helper-mcp" ||
		len(h.Args) != 1 || h.Args[0] != "--serve" ||
		len(h.Env) != 1 || h.Env[0] != "HELPER_TOKEN=abc" ||
		h.Enabled || h.Namespace != "helper" || h.Risk != "" {
		t.Errorf("stdio 条目解析不符: %+v", h)
	}
}

// TestLoadEnvOverrideMCPServers 验证 SEMANTIC_MCP_SERVERS（JSON 数组）
// 整体替换 yaml 中的 mcp_servers；非法 JSON 导致加载失败。
func TestLoadEnvOverrideMCPServers(t *testing.T) {
	t.Setenv("SEMANTIC_MCP_SERVERS", `[{"name":"map","transport":"http","endpoint":"http://127.0.0.1:8082/mcp","enabled":true,"namespace":"map"}]`)

	yamlContent := `
mcp_servers:
  - name: from-yaml
    transport: stdio
    command: "/bin/cat"
    enabled: true
    namespace: y
`
	path := filepath.Join(t.TempDir(), "semantic-server.yaml")
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("写入临时配置文件失败: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) 不应返回错误: %v", path, err)
	}
	if len(cfg.MCPServers) != 1 || cfg.MCPServers[0].Name != "map" || cfg.MCPServers[0].Transport != "http" {
		t.Errorf("mcp_servers 应被 env 整体替换，实际: %+v", cfg.MCPServers)
	}
}

// TestLoadInvalidEnvMCPServers 验证非法的 SEMANTIC_MCP_SERVERS 值导致加载失败。
func TestLoadInvalidEnvMCPServers(t *testing.T) {
	t.Setenv("SEMANTIC_MCP_SERVERS", `{not-json`)
	if _, err := Load(""); err == nil {
		t.Error("非法的 SEMANTIC_MCP_SERVERS 值应导致 Load 返回错误")
	}
}

// TestDefaultMCPServersEmpty 验证默认配置中 mcp_servers 为显式空列表
// （与 yaml "mcp_servers: []" 及 env "[]" 解析结果 DeepEqual 一致）。
func TestDefaultMCPServersEmpty(t *testing.T) {
	cfg := Default()
	if cfg.MCPServers == nil || len(cfg.MCPServers) != 0 {
		t.Errorf("默认 mcp_servers 应为显式空列表，实际: %+v", cfg.MCPServers)
	}
}
