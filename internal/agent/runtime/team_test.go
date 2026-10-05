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
	"os"
	"path/filepath"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/agent/profile"
	"insightos.cn/semantic-framework/internal/agent/team"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
)

// newLoaderForRoot 创建指定根目录的 profile 加载器。
func newLoaderForRoot(_ *testing.T, root string) *profile.Loader {
	return profile.NewLoader(root)
}

// writeTeamProfiles 在临时根目录写入 leader/query 两个角色 profile。
func writeTeamProfiles(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	roles := map[string]map[string]string{
		"leader": {
			"role.yaml": "name: leader\nmode: coordinator\ndescription: 团队指挥官\nmodel: mock\n",
			"AGENT.md":  "# Role\n你是 Leader。",
		},
		"query": {
			"role.yaml": "name: query\nmode: service\ndescription: 系统与产物查询助手\nmodel: mock\n",
			"AGENT.md":  "# Role\n你是查询助手。",
		},
	}
	for role, files := range roles {
		dir := filepath.Join(root, role)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("创建角色目录失败: %v", err)
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatalf("写入 %s/%s 失败: %v", role, name, err)
			}
		}
	}
	return root
}

// testDef 是 leader + query-1 的 Team 定义。
func testDef() *team.Def {
	return &team.Def{
		Name:    "default",
		Leader:  team.MemberDef{ID: "leader", Role: "leader"},
		Members: []team.MemberDef{{ID: "query-1", Role: "query"}},
	}
}

// rosterEntry 按 ID 取目录条目。
func rosterEntry(t *testing.T, svc *Service, id string) AgentInfo {
	t.Helper()
	for _, info := range svc.Roster() {
		if info.ID == id {
			return info
		}
	}
	t.Fatalf("roster 缺少成员 %q: %+v", id, svc.Roster())
	return AgentInfo{}
}

// TestAssembleTeam 验证 Team 组建、成员目录状态与 Leader 对话状态迁移。
func TestAssembleTeam(t *testing.T) {
	fx := newTestFixture(t)
	fx.loader = newLoaderForRoot(t, writeTeamProfiles(t))
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
	})

	if err := svc.AssembleTeam(context.Background(), testDef()); err != nil {
		t.Fatalf("AssembleTeam 失败: %v", err)
	}
	defer svc.Shutdown()

	roster := svc.Roster()
	if len(roster) != 2 {
		t.Fatalf("roster 应有 2 个成员，实际: %+v", roster)
	}

	leader := rosterEntry(t, svc, "leader")
	if leader.Role != "leader" || leader.Mode != "coordinator" || leader.Status != AgentStatusIdle || leader.Model != "mock" {
		t.Errorf("leader 条目不符: %+v", leader)
	}
	query := rosterEntry(t, svc, "query-1")
	if query.Role != "query" || query.Mode != "service" || query.Status != AgentStatusIdle {
		t.Errorf("query 条目不符: %+v", query)
	}

	// leader 对话后回到 idle（HandleMessage 的状态迁移闭环）。
	if _, err := svc.HandleMessage(context.Background(), "u1", "", "你好"); err != nil {
		t.Fatalf("HandleMessage 失败: %v", err)
	}
	if got := rosterEntry(t, svc, "leader"); got.Status != AgentStatusIdle {
		t.Errorf("对话结束后 leader 应回到 idle，实际: %+v", got)
	}

	svc.Shutdown()
}

// TestLeaderRunUsesMemberInstanceID 验证 Run 与事件使用 Team 成员实例 ID，
// 而不是把角色 profile 名误当成实例 ID。一个角色未来可有多个独立实例。
func TestLeaderRunUsesMemberInstanceID(t *testing.T) {
	fx := newTestFixture(t)
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus,
		Logger: fx.logger,
	})
	def := &team.Def{Name: "instance-team", Leader: team.MemberDef{
		ID: "leader-instance-1", Role: "leader",
	}}
	if err := svc.AssembleTeam(context.Background(), def); err != nil {
		t.Fatalf("AssembleTeam 失败: %v", err)
	}
	defer svc.Shutdown()

	runID, err := svc.HandleMessage(context.Background(), "u-instance", "", "你好")
	if err != nil {
		t.Fatalf("HandleMessage 失败: %v", err)
	}
	run, err := fx.st.GetRunSession(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.AgentID != def.Leader.ID || run.AgentName != def.Leader.Role {
		t.Fatalf("Run Agent 归属不符: run=%+v leader=%+v", run, def.Leader)
	}
	var runEvents int
	for _, item := range drainEvents(fx.events) {
		envelope, ok := item.Payload.(ws.Envelope)
		if !ok || envelope.ResourceType != "agent_run" || envelope.ResourceID != runID {
			continue
		}
		runEvents++
		if envelope.Agent.ID != def.Leader.ID || envelope.Agent.Name != def.Leader.Role {
			t.Errorf("Run 事件 Agent 归属不符: %+v", envelope.Agent)
		}
	}
	if runEvents != 2 {
		t.Fatalf("正常 Run 应发布 started/completed 两个状态事件，实际: %d", runEvents)
	}
}

// TestRosterDerivesDynamicRobotAgentFromPilot 验证 Pilot 完成注册后无需再
// 手工创建 Agent：统一目录会派生稳定的 robot:<robot-id> 逻辑身份，并展示
// 该 Pilot 实际启用的 Robot Skill。模型进程仍只在 Task Run 时按需创建。
func TestRosterDerivesDynamicRobotAgentFromPilot(t *testing.T) {
	fx := newTestFixture(t)
	root := writeTeamProfiles(t)
	robotDir := filepath.Join(root, "robot")
	if err := os.MkdirAll(robotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(robotDir, "role.yaml"), []byte(
		"name: robot\nmode: worker\ndescription: Robot Task Agent\nmodel: mock\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(robotDir, "AGENT.md"),
		[]byte("# Robot Agent\n只处理分配给当前 Robot 的 Task。"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := NewService(Deps{Profiles: newLoaderForRoot(t, root), LLM: fx.llmReg,
		Store: fx.st, Bus: fx.bus, Logger: fx.logger})
	now := time.Now().UTC()
	pilot := store.RobotPilot{PilotInstanceID: "pilot-r1pro-02", RobotID: "r1pro-fake-02",
		RobotModel: "r1pro", Backend: "fake", Status: "online", RobotStatus: "idle",
		AbilityFrameworkStatus: "ready", LastSeenAt: now}
	if err := fx.st.SaveRobotPilot(pilot); err != nil {
		t.Fatal(err)
	}
	if err := fx.st.SaveRobotPilotSkill(store.RobotPilotSkill{
		PilotInstanceID: pilot.PilotInstanceID, Name: "grasp-object", Version: "0.1.0",
		Enabled: true, Status: "installed", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	info := rosterEntry(t, svc, "robot:r1pro-fake-02")
	if info.Source != "pilot" || info.Status != AgentStatusIdle ||
		info.PilotInstanceID != pilot.PilotInstanceID || len(info.SkillNames) != 1 ||
		info.SkillNames[0] != "grasp-object" {
		t.Fatalf("Pilot 应派生可分配的 Robot Agent: %+v", info)
	}
	if len(info.RobotSkillNames) != 1 || info.RobotSkillNames[0] != "grasp-object" || len(info.AgentSkillNames) != 0 {
		t.Fatalf("Robot 执行技能不能冒充 Agent 技能: %+v", info)
	}
	pilot.Status = "offline"
	pilot.LastSeenAt = now.Add(time.Second)
	if err := fx.st.SaveRobotPilot(pilot); err != nil {
		t.Fatal(err)
	}
	if got := rosterEntry(t, svc, "robot:r1pro-fake-02"); got.Status != AgentStatusOffline {
		t.Fatalf("Pilot 离线后逻辑 Agent 应保留但不可分配: %+v", got)
	}
}

// TestAssembleTeamRejected 验证角色目录缺失会失败；worker 则作为 Workflow
// Agent 登记，但不会进入 Leader 的 SubAgent Registry。
func TestAssembleTeamRejected(t *testing.T) {
	fx := newTestFixture(t)
	fx.loader = newLoaderForRoot(t, writeTeamProfiles(t))
	svc := NewService(Deps{
		Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
	})

	// 成员角色目录缺失。
	bad := testDef()
	bad.Members = []team.MemberDef{{ID: "ghost-1", Role: "ghost"}}
	if err := svc.AssembleTeam(context.Background(), bad); err == nil {
		t.Error("角色目录缺失应组建失败")
	}

	// worker 模式由 Workflow Runtime 直接装配。
	root := writeTeamProfiles(t)
	robotDir := filepath.Join(root, "robot")
	if err := os.MkdirAll(robotDir, 0o755); err != nil {
		t.Fatalf("创建 robot 目录失败: %v", err)
	}
	for name, content := range map[string]string{
		"role.yaml": "name: robot\nmode: worker\ndescription: 执行\nmodel: mock\n",
		"AGENT.md":  "# Role\n你是 Robot。",
	} {
		if err := os.WriteFile(filepath.Join(robotDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("写入 robot/%s 失败: %v", name, err)
		}
	}
	svc2 := NewService(Deps{
		Profiles: newLoaderForRoot(t, root), LLM: fx.llmReg, Store: fx.st, Bus: fx.bus, Logger: fx.logger,
	})
	workerTeam := testDef()
	workerTeam.Members = []team.MemberDef{{ID: "robot-a", Role: "robot"}}
	if err := svc2.AssembleTeam(context.Background(), workerTeam); err != nil {
		t.Fatalf("worker 模式应组建成功: %v", err)
	}
	if len(svc2.Roster()) != 2 {
		t.Errorf("roster 应包含 leader 与 worker，实际: %+v", svc2.Roster())
	}
	if _, exposed := svc2.subAgents.Get("robot-a"); exposed {
		t.Error("worker 不能被包装成 Leader AgentTool")
	}
}
