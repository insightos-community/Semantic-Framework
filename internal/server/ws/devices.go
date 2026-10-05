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
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"

	robotdomain "insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/pkg/log"
)

// DeviceGateway 向浏览器提供 Server 视角的全局 Pilot、Robot、Skill、
// Ability 与 Robot Execution 增量。浏览器始终只连接 Server。
type DeviceGateway struct {
	service *robotdomain.Service
	auth    *auth.Service
	logger  *log.Logger
}

func NewDeviceGateway(service *robotdomain.Service, authService *auth.Service, logger *log.Logger) *DeviceGateway {
	return &DeviceGateway{service: service, auth: authService, logger: logger}
}

func (g *DeviceGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, echoProtocol := extractToken(r)
	if _, err := g.auth.ValidateToken(token); err != nil {
		writeError(w, http.StatusUnauthorized, "AUTH_TOKEN_INVALID", "访问令牌无效")
		return
	}
	options := &websocket.AcceptOptions{}
	if echoProtocol != "" {
		options.Subprotocols = []string{echoProtocol}
	}
	conn, err := websocket.Accept(w, r, options)
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(64 << 10)

	// 前端连接后必须先发送游标。Server 当前只缓存实时增量；游标存在
	// 缺口时明确要求重新读取 Snapshot，不用不完整事件拼装状态。
	readCtx, cancelRead := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancelRead()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		return
	}
	var request struct {
		Type          string `json:"type"`
		AfterSequence int64  `json:"after_sequence"`
	}
	if err := json.Unmarshal(data, &request); err != nil || request.Type != "sync" {
		_ = writeDeviceMessage(r.Context(), conn, map[string]any{"type": "error", "code": "BAD_MESSAGE", "message": "首条消息必须是 sync"})
		return
	}
	events, unsubscribe, err := g.service.SubscribeDevices(request.AfterSequence)
	if err != nil {
		_ = writeDeviceMessage(r.Context(), conn, map[string]any{"type": "error", "code": "SYNC_FAILED", "message": err.Error()})
		return
	}
	defer unsubscribe()

	disconnected := make(chan error, 1)
	go func() {
		for {
			_, _, readErr := conn.Read(r.Context())
			if readErr != nil {
				disconnected <- readErr
				return
			}
		}
	}()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}
			if err := writeDeviceMessage(r.Context(), conn, event); err != nil {
				return
			}
		case <-disconnected:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func writeDeviceMessage(ctx context.Context, conn *websocket.Conn, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, data)
}
