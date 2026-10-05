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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"insightos.cn/semantic-framework/internal/pilot"
)

// 事件型等待的期限只用于避免测试挂死，不用来断言启动速度：这些事件迟早会发生，
// 而 CI Runner 比本机慢一个数量级（同一条命令本机约 1 秒，CI 上 3 秒仍不够），
// 共享 Runner 的负载波动又很大。本地实测注册用时在毫秒级，这里留足余量。
const pilotEventWait = 20 * time.Second

// Pilot 退出前最多用 15 秒确认没有残留的物理动作（见 runPermanentContext 的
// stopTimeout），等待停止必须长于该预算，否则会把“仍在安全收尾”误判为失败。
const pilotStopWait = 20 * time.Second

func TestPermanentPilotRegistersDeploymentAndStopsWithContext(t *testing.T) {
	af := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ability-heartbeat" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer af.Close()

	registered := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/pilot" {
			http.NotFound(w, r)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		_, body, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var message map[string]any
		if json.Unmarshal(body, &message) == nil {
			registered <- message
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	root := t.TempDir()
	reportPath := filepath.Join(root, "pilot", "stop-result.json")
	t.Setenv("SEMANTIC_PILOT_STOP_REPORT", reportPath)
	profile := filepath.Join(root, "robot.yaml")
	content := "api_version: 1\n" +
		"robot:\n  id: r1pro-test\n  display_name: R1 Pro Test\n  model: r1pro\n  backend: fake\n" +
		"ability_framework:\n  endpoint: " + af.URL + "\n" +
		"abilities:\n  navigation: {}\n  manipulator_motion: {}\n  end_effector: {}\n" +
		"  robot_state: {}\n  sensor_capture: {}\n  object_perception: {}\n  grasp_planning: {}\n" +
		"pilot:\n  heartbeat_interval_seconds: 1\n  allow_ability_debug: true\n"
	if err := os.WriteFile(profile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sdkSource := filepath.Join(root, "skill-sdk")
	if err := os.MkdirAll(sdkSource, 0o750); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runPermanentContext(ctx, profile,
			"ws"+strings.TrimPrefix(server.URL, "http")+"/ws/pilot", server.URL, "test-token",
			"pilot-test", filepath.Join(root, "data"), filepath.Join(root, "skills"),
			sdkSource, "", "python", "0.5.0-test")
	}()

	select {
	case message := <-registered:
		if message["type"] != "register" {
			t.Fatalf("unexpected message: %#v", message)
		}
		pilot, _ := message["pilot"].(map[string]any)
		if pilot["robot_id"] != "r1pro-test" || pilot["pilot_instance_id"] != "pilot-test" {
			t.Fatalf("deployment identity not registered: %#v", pilot)
		}
	case err := <-done:
		// Pilot 在注册前就退出时，真实原因在 done 里。没有这一分支，任何
		// 启动错误都会只报“没有注册”，把可诊断的失败变成疑似超时。
		t.Fatalf("Pilot 在注册前退出: %v", err)
	case <-time.After(pilotEventWait):
		t.Fatalf("Pilot 未在 %s 内注册", pilotEventWait)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(pilotStopWait):
		t.Fatalf("Pilot 未在 %s 内停止", pilotStopWait)
	}
	reportData, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report pilotStopReport
	if err := json.Unmarshal(reportData, &report); err != nil {
		t.Fatal(err)
	}
	if !report.Safe || !report.HoldConfirmed || report.FinishedAt.IsZero() {
		t.Fatalf("空闲 Pilot 也必须确认没有活动物理动作: %+v", report)
	}
}

type shutdownWorkStore struct {
	skills  []pilot.SkillExecution
	debug   []pilot.AbilityDebugExecution
	actions []pilot.ActionExecution
}

func (s shutdownWorkStore) ListRecoverableSkillExecutions() ([]pilot.SkillExecution, error) {
	return s.skills, nil
}

func (s shutdownWorkStore) ListActiveAbilityDebug() ([]pilot.AbilityDebugExecution, error) {
	return s.debug, nil
}

func (s shutdownWorkStore) ListActiveActions() ([]pilot.ActionExecution, error) {
	return s.actions, nil
}

type shutdownSkillStopper struct {
	calls  *[]string
	status pilot.SkillExecutionStatus
	err    error
}

func (s shutdownSkillStopper) Stop(_ context.Context, id, _, _, _ string) (pilot.SkillExecution, error) {
	*s.calls = append(*s.calls, "skill:"+id)
	return pilot.SkillExecution{ID: id, Status: s.status,
		StopOutcome: map[string]any{"safe": s.err == nil}}, s.err
}

type shutdownDebugStopper struct {
	calls  *[]string
	status string
}

func (s shutdownDebugStopper) Stop(_ context.Context, id, _ string) (pilot.AbilityDebugExecution, error) {
	*s.calls = append(*s.calls, "debug:"+id)
	return pilot.AbilityDebugExecution{ID: id, Status: s.status}, nil
}

func TestStopActivePilotWorkOrdersSkillBeforeAbilityDebug(t *testing.T) {
	calls := []string{}
	store := shutdownWorkStore{
		skills: []pilot.SkillExecution{{ID: "skill-a"}, {ID: "skill-b"}},
		debug:  []pilot.AbilityDebugExecution{{ID: "debug-a"}},
	}
	report, err := stopActivePilotWork(context.Background(), store,
		shutdownSkillStopper{calls: &calls, status: pilot.SkillStopped},
		shutdownDebugStopper{calls: &calls, status: "stopped"})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Safe || !report.HoldConfirmed || report.ExecutionID != "skill-a" {
		t.Fatalf("停止证据不完整: %+v", report)
	}
	want := []string{"skill:skill-a", "skill:skill-b", "debug:debug-a"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("graceful stop 顺序错误: got=%v want=%v", calls, want)
	}
}

func TestStopActivePilotWorkReturnsErrorButStillStopsDebug(t *testing.T) {
	calls := []string{}
	store := shutdownWorkStore{
		skills: []pilot.SkillExecution{{ID: "skill-unknown"}},
		debug:  []pilot.AbilityDebugExecution{{ID: "debug-active"}},
	}
	report, err := stopActivePilotWork(context.Background(), store,
		shutdownSkillStopper{calls: &calls, status: pilot.SkillInterrupted, err: errors.New("hold 未确认")},
		shutdownDebugStopper{calls: &calls, status: "stopped"})
	if err == nil {
		t.Fatal("物理停止未确认必须让 Pilot 非零退出")
	}
	if report.Safe || report.HoldConfirmed {
		t.Fatalf("未确认物理状态不能生成安全证据: %+v", report)
	}
	if strings.Join(calls, ",") != "skill:skill-unknown,debug:debug-active" {
		t.Fatalf("一个 Skill 停止失败后仍应继续停止 Ability debug: %v", calls)
	}
}
