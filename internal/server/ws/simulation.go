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
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/simulation"
	"insightos.cn/semantic-framework/pkg/log"
)

// SimulationStreamAccess 校验 Project 归属。
type SimulationStreamAccess interface {
	ProjectOwnedByUser(string, string) error
}

// SimulationStreamGateway 把 Plugin 的Scene Pose/传感器二进制流转发给 Studio。
// Studio 不会得到 Runtime 地址，也不会直接连接 Plugin。
type SimulationStreamGateway struct {
	auth       *auth.Service
	access     SimulationStreamAccess
	simulation *simulation.Service
	logger     *log.Logger
}

func NewSimulationStreamGateway(
	authService *auth.Service,
	access SimulationStreamAccess,
	service *simulation.Service,
	logger *log.Logger,
) *SimulationStreamGateway {
	return &SimulationStreamGateway{
		auth: authService, access: access, simulation: service, logger: logger,
	}
}

func (g *SimulationStreamGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, echoProtocol := extractToken(r)
	userID, err := g.auth.ValidateToken(token)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "AUTH_INVALID", "登录状态无效")
		return
	}
	projectID := r.URL.Query().Get("project_id")
	if projectID == "" || g.access.ProjectOwnedByUser(userID, projectID) != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "Project 不存在")
		return
	}

	sourceURL, err := g.sourceURL(projectID, r)
	if err != nil || sourceURL == "" {
		writeError(w, http.StatusBadRequest, "SIMULATION_STREAM_INVALID", "仿真流参数无效")
		return
	}
	// 上游连接不能绑定到即将升级的 HTTP 请求 Context；部分 HTTP Server 会在
	// downstream 升级完成后结束该 Context，继而把已经建立的 Plugin 连接一并关闭。
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dialCancel()
	upstream, response, err := websocket.Dial(dialCtx, sourceURL, nil)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		writeError(w, http.StatusBadGateway, "SIMULATION_STREAM_OFFLINE", "仿真画面不可用")
		return
	}
	defer func() { _ = upstream.Close(websocket.StatusNormalClosure, "") }()

	options := &websocket.AcceptOptions{}
	// float32 Pose、RGB 和 Depth 帧会明显超过 coder/websocket 默认的 32 KiB。
	// 16 MiB 足以覆盖当前分辨率，同时避免无界读取占用 Server 内存。
	upstream.SetReadLimit(16 << 20)
	if echoProtocol != "" {
		options.Subprotocols = []string{echoProtocol}
	}
	downstream, err := websocket.Accept(w, r, options)
	if err != nil {
		return
	}
	defer func() { _ = downstream.Close(websocket.StatusNormalClosure, "") }()
	downstream.SetReadLimit(1 << 20)

	// WebSocket 升级完成后，HTTP 请求的 Context 可能被服务器结束。
	// 转发会话必须由两端连接的生命周期管理，否则会在第一帧到达前取消上游读取。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 画面流是单向的，但仍需读取 downstream 的关闭帧。否则场景暂停、上游暂时
	// 没有新帧时，浏览器关闭页面后代理无法及时取消 Plugin 连接。
	go func() {
		for {
			if _, _, readErr := downstream.Read(ctx); readErr != nil {
				cancel()
				return
			}
		}
	}()
	for {
		messageType, data, readErr := upstream.Read(ctx)
		if readErr != nil {
			if !errors.Is(readErr, context.Canceled) {
				g.logger.WithError(readErr).Debug("仿真上游流已结束")
			}
			return
		}
		writeCtx, writeCancel := context.WithTimeout(ctx, 5*time.Second)
		writeErr := downstream.Write(writeCtx, messageType, data)
		writeCancel()
		if writeErr != nil {
			return
		}
	}
}

func (g *SimulationStreamGateway) sourceURL(
	projectID string,
	r *http.Request,
) (string, error) {
	switch r.URL.Query().Get("kind") {
	case "pose":
		return g.simulation.ScenePoseURL(
			projectID, r.URL.Query().Get("instance_id"))
	case "sensor":
		return g.simulation.SensorFramesURL(
			projectID,
			r.URL.Query().Get("instance_id"),
			r.URL.Query().Get("robot_id"),
			r.URL.Query().Get("sensor_id"),
		)
	default:
		return "", errors.New("未知仿真流类型")
	}
}
