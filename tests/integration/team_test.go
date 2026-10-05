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

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/bootstrap"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/log"
)

// startTeamApp 以真实 configs/agents（含 teams/ 定义）启动装配后的服务
// （mock 模型），Leader 与请求式 Query 随启动组建上线。
func startTeamApp(t *testing.T) (httpBase, wsBase string, app *bootstrap.App, stop func()) {
	t.Helper()
	t.Setenv("SEMANTIC_ADMIN_PASSWORD", "test-admin-pass")

	agentsDir, err := filepath.Abs(filepath.Join("..", "..", "configs", "agents"))
	if err != nil {
		t.Fatalf("解析 agents 目录失败: %v", err)
	}
	cfg := config.Default()
	cfg.Server.HTTPAddr = freeAddr(t)
	cfg.Server.WSAddr = freeAddr(t)
	cfg.Store.SQLitePath = filepath.Join(t.TempDir(), "test.db")
	storetest.SeedMigratedAt(t, cfg.Store.SQLitePath)
	cfg.LLM.Default = "mock" // 无 key 环境：默认模型走 mock 驱动
	cfg.Agents.ProfilesDir = agentsDir
	cfg.Agents.TeamsDir = filepath.Join(agentsDir, "teams")
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})

	app, err = bootstrap.Wire(cfg, logger)
	if err != nil {
		t.Fatalf("Wire 装配失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- app.Run(ctx)
	}()

	httpBase = fmt.Sprintf("http://%s", cfg.Server.HTTPAddr)
	wsBase = fmt.Sprintf("ws://%s", cfg.Server.WSAddr)
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(httpBase + "/api/v1/system/healthz")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("服务在 30s 内未就绪，最后一次错误: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	stop = func() {
		cancel()
		select {
		case err := <-runDone:
			if err != nil {
				t.Errorf("App.Run 应随 ctx 取消正常退出，实际返回: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Errorf("App.Run 未在 15s 内退出")
		}
	}
	return httpBase, wsBase, app, stop
}

// TestTeamAssembly 验证静态 Team 只声明可复用的角色实例。Robot Agent
// 必须等 Task 分配真实 Robot 后以 robot:<robot-id> 动态生成，不能恢复
// default Team 中的 robot-1 占位。
func TestTeamAssembly(t *testing.T) {
	httpBase, _, _, stop := startTeamApp(t)
	defer stop()

	token := login(t, httpBase)
	req, err := http.NewRequest(http.MethodGet, httpBase+"/api/v1/agents", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /agents 失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /agents 应返回 200，实际: %d", resp.StatusCode)
	}
	var body struct {
		Agents []struct {
			ID       string `json:"id"`
			Role     string `json:"role"`
			Mode     string `json:"mode"`
			Status   string `json:"status"`
			Model    string `json:"model"`
			Activity string `json:"activity"`
		} `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析 /agents 响应失败: %v", err)
	}
	if len(body.Agents) != 5 {
		t.Fatalf("Team 应有 Leader、Query、Developer、Map 与 Monitor，实际: %+v", body.Agents)
	}
	byID := make(map[string]struct {
		ID       string `json:"id"`
		Role     string `json:"role"`
		Mode     string `json:"mode"`
		Status   string `json:"status"`
		Model    string `json:"model"`
		Activity string `json:"activity"`
	}, 2)
	for _, a := range body.Agents {
		byID[a.ID] = a
	}
	if a := byID["leader"]; a.Role != "leader" || a.Mode != "coordinator" || a.Status != "idle" || a.Model != "mock" {
		t.Errorf("leader 目录条目不符: %+v", a)
	}
	if a := byID["query-1"]; a.Role != "query" || a.Mode != "service" || a.Status != "idle" {
		t.Errorf("query-1 目录条目不符: %+v", a)
	}
	if a := byID["developer-1"]; a.Role != "developer" || a.Mode != "worker" || a.Status != "idle" {
		t.Errorf("developer-1 Workflow Worker 目录条目不符: %+v", a)
	}
	if a := byID["map-1"]; a.Role != "map" || a.Mode != "worker" || a.Status != "idle" {
		t.Errorf("map-1 Workflow Worker 目录条目不符: %+v", a)
	}
	if a := byID["monitor-1"]; a.Role != "monitor" || a.Mode != "worker" || a.Status != "idle" {
		t.Errorf("monitor-1 Workflow Worker 目录条目不符: %+v", a)
	}
	if _, exists := byID["robot-1"]; exists {
		t.Fatal("Robot Agent 必须后绑定真实 Robot，不应预置 robot-1")
	}
}
