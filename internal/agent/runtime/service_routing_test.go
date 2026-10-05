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
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/llm"
)

func routedConversationFixture(t *testing.T, build ModelBuilder) (*testFixture, *Service) {
	t.Helper()
	fx := newTestFixture(t)
	root := writeTeamProfiles(t)
	robotRoot := filepath.Join(root, "robot")
	if err := os.MkdirAll(robotRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(robotRoot, "role.yaml"), []byte("name: robot\nmode: worker\nmodel: mock\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(robotRoot, "AGENT.md"), []byte("只解释状态，不执行动作。"), 0o644); err != nil {
		t.Fatal(err)
	}
	fx.loader = newLoaderForRoot(t, root)
	svc := NewService(Deps{Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st,
		Bus: fx.bus, Logger: fx.logger, BuildModel: build})
	svc.roster.register(AgentInfo{ID: "leader", Role: "leader", Model: "mock"})
	svc.roster.register(AgentInfo{ID: "query-1", Role: "query", Model: "mock"})
	if err := fx.st.SaveRobotPilot(store.RobotPilot{PilotInstanceID: "pilot-route",
		RobotID: "route", Status: "online", RobotStatus: "idle", LastSeenAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	return fx, svc
}

func TestConversationRecipientDoesNotBuildBrokenLeader(t *testing.T) {
	for _, recipient := range []string{"query-1", "robot:route"} {
		t.Run(recipient, func(t *testing.T) {
			var models []string
			fx, svc := routedConversationFixture(t, func(_ context.Context, entry llm.Provider, _ string) (kernel.Model, error) {
				models = append(models, entry.Model)
				m := kernel.NewMockChatModel()
				m.SetResponse("状态已解释")
				return m, nil
			})
			t.Setenv(llm.APIKeyEnv("broken-leader"), "")
			if err := fx.llmReg.Reload(config.LLMConfig{Default: "mock", Providers: map[string]config.LLMProviderConfig{
				"mock":          {Component: "mock", Model: "recipient-model"},
				"broken-leader": {Component: "openai", BaseURL: "https://example.invalid/v1", Model: "broken-model"},
			}}); err != nil {
				t.Fatal(err)
			}
			if err := fx.loader.UpdateModels("leader", "broken-leader", "auto", "auto"); err != nil {
				t.Fatal(err)
			}
			runID, err := svc.HandleMessageToAgent(context.Background(), "route-user", "", "解释当前状态",
				nil, "inherit", "inherit", "collaboration", recipient)
			if err != nil {
				t.Fatalf("有效收件人不能被 Leader 缺少凭据阻断: %v", err)
			}
			run, err := fx.st.GetRunSession(runID)
			if err != nil || run.AgentID != recipient || len(models) != 1 || models[0] != "recipient-model" {
				t.Fatalf("只能构建实际收件人: run=%+v models=%v err=%v", run, models, err)
			}
			if _, exists := svc.sessions[run.ChatSessionID]; exists {
				t.Fatal("非 Leader 对话不应偷偷构建 Leader 缓存")
			}
			if _, err := svc.HandleMessage(context.Background(), "route-user", run.ChatSessionID, "询问 Leader"); err == nil || !strings.Contains(err.Error(), "未配置 API Token") {
				t.Fatalf("明确发给坏 Leader 时仍应真实报错: %v", err)
			}
		})
	}
}

func TestConversationReasoningEffortMatchesSavedAndPerRunOptions(t *testing.T) {
	for _, recipient := range []string{"leader", "query-1", "robot:route"} {
		t.Run(recipient, func(t *testing.T) {
			var actualEffort atomic.Value
			actualEffort.Store("")
			fx, svc := routedConversationFixture(t, func(_ context.Context, entry llm.Provider, _ string) (kernel.Model, error) {
				effort, _ := entry.Options["reasoning_effort"].(string)
				m := kernel.NewMockChatModel()
				m.SetResponse("已完成分析")
				return &effortRecordingModel{MockChatModel: m, effort: effort, actual: &actualEffort}, nil
			})
			if err := fx.llmReg.Reload(config.LLMConfig{Default: "mock", Providers: map[string]config.LLMProviderConfig{
				"mock": {Component: "mock", Model: "mock", Capabilities: []string{"reasoning_effort"}},
			}}); err != nil {
				t.Fatal(err)
			}
			sess, err := svc.loadOrCreateSession("effort-user", "", "保存的推理强度")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.SetSessionAgentModel("effort-user", sess.ID, recipient, "mock", "high"); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct{ effort, visibility, want string }{
				{"", "inherit", "high"}, {"inherit", "inherit", "high"},
				{"auto", "inherit", "auto"}, {"inherit", "inherit", "high"},
				{"inherit", "hide", "high"}, {"low", "inherit", "low"},
				{"", "inherit", "high"}, {"", "hide", "high"},
			} {
				_, err := svc.HandleMessageToAgent(context.Background(), "effort-user", sess.ID, "分析这轮",
					nil, tc.effort, tc.visibility, "collaboration", recipient)
				if err != nil {
					t.Fatal(err)
				}
				messages, err := fx.st.ListChatMessages(sess.ID, 0, 0)
				if err != nil {
					t.Fatal(err)
				}
				var metadata RunMetadata
				if err := json.Unmarshal([]byte(messages[len(messages)-1].Metadata), &metadata); err != nil {
					t.Fatal(err)
				}
				if metadata.ReasoningEffort != tc.want || actualEffort.Load() != effectiveReasoningEffort(tc.want) {
					t.Fatalf("effort=%q visibility=%q: metadata=%q model=%q want=%q", tc.effort,
						tc.visibility, metadata.ReasoningEffort, actualEffort.Load(), tc.want)
				}
			}
			snapshot, err := fx.st.GetSessionAgentModel(sess.ID, recipient)
			if err != nil || snapshot.ReasoningEffort != "high" {
				t.Fatalf("单轮覆盖污染了已保存会话设置: %+v %v", snapshot, err)
			}
			prof, err := svc.profileForAgent(recipient)
			if err != nil || prof.ReasoningEffort != "auto" {
				t.Fatalf("单轮覆盖污染共享 Profile: %+v %v", prof, err)
			}
		})
	}
}

type effortRecordingModel struct {
	*kernel.MockChatModel
	effort string
	actual *atomic.Value
}

func (m *effortRecordingModel) Stream(ctx context.Context, input []*schema.Message,
	opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.actual.Store(m.effort)
	return m.MockChatModel.Stream(ctx, input, opts...)
}

func TestConversationSettingsRejectBusyDuringFirstRecipientBuild(t *testing.T) {
	for _, recipient := range []string{"leader", "query-1", "robot:route"} {
		t.Run(recipient, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			fx, svc := routedConversationFixture(t, func(_ context.Context, _ llm.Provider, _ string) (kernel.Model, error) {
				close(started)
				<-release
				return kernel.NewMockChatModel(), nil
			})
			sess, err := svc.loadOrCreateSession("busy-user", "", "尚未缓存的模型")
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := svc.HandleMessageToAgent(context.Background(), "busy-user", sess.ID, "开始分析",
					nil, "inherit", "inherit", "collaboration", recipient)
				done <- err
			}()
			select {
			case <-started:
			case <-time.After(runtimeEventWait):
				t.Fatalf("首轮装配未在 %s 内开始", runtimeEventWait)
			}
			if _, err := svc.SetSessionAgentModel("busy-user", sess.ID, recipient, "mock", "auto"); !errors.Is(err, ErrSessionBusy) {
				t.Fatalf("首次装配尚无 Runner 也必须拒绝模型切换: %v", err)
			}
			if _, err := svc.SetSessionExecutionPolicy("busy-user", sess.ID, store.ExecutionModeAuto, false); !errors.Is(err, ErrSessionBusy) {
				t.Fatalf("首次装配尚无 Run 也必须拒绝执行策略切换: %v", err)
			}
			once.Do(func() { close(release) })
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if _, err := svc.SetSessionExecutionPolicy("busy-user", sess.ID, store.ExecutionModeAuto, false); err != nil {
				t.Fatalf("结束后应允许策略切换: %v", err)
			}
			if policy, err := fx.st.GetSessionExecutionPolicy(sess.ID); err != nil || policy.Mode != store.ExecutionModeAuto {
				t.Fatalf("设置没有生效: %+v %v", policy, err)
			}
		})
	}
}

func TestConversationCacheInvalidationDuringBuildRetainsSessionLock(t *testing.T) {
	fx := newTestFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var builds atomic.Int32
	svc := NewService(Deps{Profiles: fx.loader, LLM: fx.llmReg, Store: fx.st,
		Bus: fx.bus, Logger: fx.logger, BuildModel: func(_ context.Context, _ llm.Provider, _ string) (kernel.Model, error) {
			if builds.Add(1) == 1 {
				close(started)
				<-release
			}
			return kernel.NewMockChatModel(), nil
		}})
	sess, err := svc.loadOrCreateSession("reload-user", "", "热更新")
	if err != nil {
		t.Fatal(err)
	}
	gate := svc.sessionLock(sess.ID)
	done := make(chan error, 1)
	go func() {
		_, err := svc.HandleMessage(context.Background(), "reload-user", sess.ID, "分析")
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(runtimeEventWait):
		t.Fatalf("首轮装配未在 %s 内开始", runtimeEventWait)
	}
	svc.InvalidateModelRuntimes()
	if svc.sessionLock(sess.ID) != gate {
		t.Fatal("缓存淘汰不得替换会话锁")
	}
	if _, err := svc.SetSessionExecutionPolicy("reload-user", sess.ID, store.ExecutionModeAuto, false); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("缓存淘汰后仍应保持装配 busy 边界: %v", err)
	}
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if builds.Load() != 2 {
		t.Fatalf("热重载前的在途装配不应重新入缓存: builds=%d", builds.Load())
	}
	if _, err := svc.HandleMessage(context.Background(), "reload-user", sess.ID, "下一轮"); err != nil || builds.Load() != 2 {
		t.Fatalf("新装配应能缓存复用: builds=%d err=%v", builds.Load(), err)
	}
}
