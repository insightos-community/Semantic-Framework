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

package mcpregistry

import (
	"context"
	"fmt"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// quietSyncerOpts 关闭周期对账的测试节奏（FastRounds=0 + 长稳态周期）：
// 只保留首轮同步与 list-changed 即时同步，消除后台时序对断言的干扰。
var quietSyncerOpts = SyncerOptions{FastRounds: 0, SteadyInterval: time.Hour}

// startSyncer 创建目录与同步器并 Start（注册清理）。
func startSyncer(t *testing.T, opts SyncerOptions, initial ...ServerSpec) (*Registry, *Syncer) {
	t.Helper()
	_, reg := newTestRegistry(t)
	syncer := NewSyncer(reg, nil, opts)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		syncer.Stop()
		cancel()
	})
	syncer.Start(ctx, initial)
	return reg, syncer
}

// TestSyncerInitialSync 验证 Start 后每个 enabled server 立即首轮对账。
func TestSyncerInitialSync(t *testing.T) {
	serverA := newTestMCPServer(t)
	serverB := newTestMCPServer(t)

	reg, _ := startSyncer(t, quietSyncerOpts, serverA.spec("alpha"), serverB.spec("beta"))

	waitFor(t, "两个 server 首轮对账完成", func() bool {
		return len(reg.List()) == 4
	})
	for _, fullName := range []string{"alpha.echo", "alpha.weather", "beta.echo", "beta.weather"} {
		if e := entryOf(t, reg, fullName); e.Health != HealthHealthy {
			t.Errorf("条目 %q 应为 healthy，实际: %q", fullName, e.Health)
		}
	}
}

// TestSyncerListChangedTriggersSingleServerSync 验证 tools/list_changed 通知
// 只触发该 server 的即时对账（另一 server 不重同步：条目 UpdatedAt 不变）。
func TestSyncerListChangedTriggersSingleServerSync(t *testing.T) {
	serverA := newTestMCPServer(t)
	serverB := newTestMCPServer(t)

	reg, _ := startSyncer(t, quietSyncerOpts, serverA.spec("alpha"), serverB.spec("beta"))
	waitFor(t, "首轮对账完成", func() bool { return len(reg.List()) == 4 })

	before := entryOf(t, reg, "beta.echo").UpdatedAt

	// serverA 热加工具：list_changed 通知 → 只对 alpha 即时对账。
	// 循环加热加工具消除 SSE 推送通道建立的时序抖动（同 pkg/mcp notify 测试）：
	// 任一 hotN 进入目录即证明通知链路生效。
	deadline := time.Now().Add(10 * time.Second)
	synced := ""
	for i := 0; time.Now().Before(deadline) && synced == ""; i++ {
		name := fmt.Sprintf("hot%d", i)
		sdkmcp.AddTool(serverA.sdk, &sdkmcp.Tool{Name: name, Description: "热加工具"},
			func(_ context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, any, error) {
				return &sdkmcp.CallToolResult{
					Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "hot"}},
				}, nil, nil
			})
		// 给通知投递与对账留出窗口（首轮 SSE 通道建立可能错过第一次通知，
		// 下一轮循环会再加一个工具触发新的通知）。
		pollDeadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(pollDeadline) {
			if _, ok := reg.Get("alpha." + name); ok {
				synced = "alpha." + name
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if synced == "" {
		t.Fatal("list-changed 未触发即时对账")
	}
	if e := entryOf(t, reg, synced); e.Health != HealthHealthy {
		t.Errorf("热加工具应为 healthy，实际: %q", e.Health)
	}

	// beta 未被重同步：条目 UpdatedAt 保持首轮对账的值。
	if after := entryOf(t, reg, "beta.echo").UpdatedAt; after != before {
		t.Errorf("list-changed 应只同步 alpha（beta UpdatedAt 变化: %v → %v）", before, after)
	}
}

// TestSyncerReconcileDiff 验证热重载对账三态：新增 server 启动同步项、
// 删除的停同步并移除条目、spec 变更的重建同步项（risk 变化随对账生效）。
func TestSyncerReconcileDiff(t *testing.T) {
	serverA := newTestMCPServer(t)
	serverB := newTestMCPServer(t)

	reg, syncer := startSyncer(t, quietSyncerOpts, serverA.spec("alpha"))
	waitFor(t, "alpha 首轮对账完成", func() bool { return len(reg.List()) == 2 })

	// ① 新增 beta：启动同步项并对账。
	syncer.Reconcile([]ServerSpec{serverA.spec("alpha"), serverB.spec("beta")})
	waitFor(t, "beta 进入目录", func() bool { return len(reg.List()) == 4 })

	// ② 变更 alpha（risk 覆盖）：重建同步项，对账后条目 risk 刷新。
	changed := serverA.spec("alpha")
	changed.Risk = "high"
	syncer.Reconcile([]ServerSpec{changed, serverB.spec("beta")})
	waitFor(t, "alpha risk 刷新", func() bool {
		e, ok := reg.Get("alpha.echo")
		return ok && e.Risk == "high"
	})

	// ③ 删除 beta：停同步并移除条目（RemoveServer 在 Reconcile 内同步完成）。
	syncer.Reconcile([]ServerSpec{changed})
	if got := len(reg.List()); got != 2 {
		t.Fatalf("删除 beta 后目录应剩 2 条，实际: %d", got)
	}
	if _, ok := reg.Get("beta.echo"); ok {
		t.Error("被删 server 的条目应移除")
	}

	// ④ 全部删除：目录清空。
	syncer.Reconcile(nil)
	if got := len(reg.List()); got != 0 {
		t.Errorf("清空配置后目录应为空，实际: %d", got)
	}
}

// TestSyncerPeriodicResync 验证周期对账真实发生（快频节奏注入小间隔）：
// server 侧热删工具但不依赖 list-changed（直连 pool 无通知桥接场景由
// 稳态兜底——此处用快频轮模拟），目录随对账移除该工具。
func TestSyncerPeriodicResync(t *testing.T) {
	serverA := newTestMCPServer(t)

	reg, _ := startSyncer(t,
		SyncerOptions{FastInterval: 50 * time.Millisecond, FastRounds: 100, SteadyInterval: time.Hour},
		serverA.spec("alpha"))
	waitFor(t, "alpha 首轮对账完成", func() bool { return len(reg.List()) == 2 })

	// 热删 weather：周期对账兜底移除（list-changed 可能先到，两者殊途同归——
	// 断言的是"目录最终收敛"，这正是双频 + 通知双通道的设计目标）。
	serverA.sdk.RemoveTools("weather")
	waitFor(t, "weather 随对账移除", func() bool {
		_, ok := reg.Get("alpha.weather")
		return !ok
	})
}

// TestSyncerReconnectAfterCrash 验证同步项在 server 崩溃-恢复周期中的
// 自愈：失联标 unavailable（不清仓），恢复后随对账回 healthy。
func TestSyncerReconnectAfterCrash(t *testing.T) {
	serverA := newTestMCPServer(t)

	reg, _ := startSyncer(t,
		SyncerOptions{FastInterval: 50 * time.Millisecond, FastRounds: 200, SteadyInterval: time.Hour},
		serverA.spec("alpha"))
	waitFor(t, "alpha 首轮对账完成", func() bool { return len(reg.List()) == 2 })

	serverA.stop()
	waitFor(t, "失联标 unavailable", func() bool {
		e, ok := reg.Get("alpha.echo")
		return ok && e.Health == HealthUnavailable
	})
	if got := len(reg.List()); got != 2 {
		t.Fatalf("失联不应清空条目，实际: %d", got)
	}

	serverA.restart(t)
	waitFor(t, "恢复标 healthy", func() bool {
		e, ok := reg.Get("alpha.echo")
		return ok && e.Health == HealthHealthy
	})
}

// TestSyncerReconcileBeforeStart 验证未 Start 的 Reconcile 被忽略（防御分支）。
func TestSyncerReconcileBeforeStart(t *testing.T) {
	serverA := newTestMCPServer(t)
	_, reg := newTestRegistry(t)
	syncer := NewSyncer(reg, nil, quietSyncerOpts)
	syncer.Reconcile([]ServerSpec{serverA.spec("alpha")})
	if got := len(reg.List()); got != 0 {
		t.Errorf("未启动的 syncer 不应产生目录条目，实际: %d", got)
	}
}
