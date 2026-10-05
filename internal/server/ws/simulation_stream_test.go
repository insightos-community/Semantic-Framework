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

package ws

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/simulation"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/log"
)

type allowSimulationStreamAccess struct{}

func (allowSimulationStreamAccess) ProjectOwnedByUser(_, _ string) error { return nil }

type simulationStreamState struct {
	state simulation.ProjectRuntimeState
}

func (s *simulationStreamState) LoadRuntimeState(string) (simulation.ProjectRuntimeState, error) {
	return s.state, nil
}

func (s *simulationStreamState) SaveRuntimeState(state simulation.ProjectRuntimeState) error {
	s.state = state
	return nil
}

type simulationStreamClient struct {
	simulation.RuntimeClient
	poseURL string
}

func (c simulationStreamClient) ScenePoseURL(string) string { return c.poseURL }

func (c simulationStreamClient) ViewerScene(
	context.Context, string,
) (simulation.ViewerScene, error) {
	return simulation.ViewerScene{}, nil
}

func (c simulationStreamClient) ViewerSceneContent(
	context.Context, string,
) ([]byte, string, error) {
	return nil, "model/gltf-binary", nil
}

func TestSimulationStreamGatewayForwardsLargePoseFrame(t *testing.T) {
	payload := bytes.Repeat([]byte{0x5a}, 128<<10)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("Plugin WS 升级失败: %v", err)
			return
		}
		defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
		if err := conn.Write(context.Background(), websocket.MessageBinary, payload); err != nil {
			t.Errorf("Plugin 帧发送失败: %v", err)
		}
	}))
	defer upstream.Close()

	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
	st := storetest.OpenMigrated(t, logger)
	t.Setenv("SEMANTIC_ADMIN_PASSWORD", "s3cret")
	authService := auth.NewService(st, logger)
	if err := authService.SeedAdmin(); err != nil {
		t.Fatalf("创建测试用户失败: %v", err)
	}
	token, err := authService.Login("admin", "s3cret")
	if err != nil {
		t.Fatalf("登录测试用户失败: %v", err)
	}

	poseURL := "ws" + strings.TrimPrefix(upstream.URL, "http") + "/frames"
	state := &simulationStreamState{state: simulation.ProjectRuntimeState{
		ProjectID: "project-1", InstanceID: "instance-1", RuntimeProfileID: "native-mujoco",
	}}
	service := simulation.NewService(
		simulationStreamClient{poseURL: poseURL}, nil, state,
	)
	gateway := NewSimulationStreamGateway(
		authService, allowSimulationStreamAccess{}, service, logger,
	)
	server := httptest.NewServer(gateway)
	defer server.Close()

	streamURL := "ws" + strings.TrimPrefix(server.URL, "http") +
		"?kind=pose&project_id=project-1&instance_id=instance-1"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, streamURL, &websocket.DialOptions{
		Subprotocols: []string{"bearer." + token},
	})
	if err != nil {
		t.Fatalf("连接 Framework Scene Pose 流失败: %v", err)
	}
	defer func() { _ = client.Close(websocket.StatusNormalClosure, "") }()
	client.SetReadLimit(1 << 20)

	messageType, got, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("读取 Framework Viewer 帧失败: %v", err)
	}
	if messageType != websocket.MessageBinary || !bytes.Equal(got, payload) {
		t.Fatalf("Framework 未完整转发大帧: type=%v bytes=%d", messageType, len(got))
	}
}
