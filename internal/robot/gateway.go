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

package robot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/log"
)

const (
	pilotReadLimit    = 2 << 20
	pilotWriteTimeout = 10 * time.Second
)

// PilotGateway 是 Server 与设备端 Pilot 的单一控制通道。首条消息必须完成
// 注册；后续消息只传命令、状态和 Artifact 元数据，不传二进制内容。
type PilotGateway struct {
	service *Service
	logger  *log.Logger
}

func NewPilotGateway(service *Service, logger *log.Logger) *PilotGateway {
	return &PilotGateway{service: service, logger: logger}
}

type pilotUplink struct {
	Type       string                  `json:"type"`
	Pilot      store.RobotPilot        `json:"pilot,omitempty"`
	Skills     []store.RobotPilotSkill `json:"skills,omitempty"`
	EventType  string                  `json:"event_type,omitempty"`
	Sequence   int64                   `json:"sequence,omitempty"`
	Payload    map[string]any          `json:"payload,omitempty"`
	Executions []map[string]any        `json:"executions,omitempty"`
	CommandID  string                  `json:"command_id,omitempty"`
	OK         bool                    `json:"ok,omitempty"`
	Error      string                  `json:"error,omitempty"`
	Result     map[string]any          `json:"result,omitempty"`
}

func (g *PilotGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		http.Error(w, "Pilot 缺少访问令牌", http.StatusUnauthorized)
		return
	}
	credentialPilotID, err := g.service.ValidatePilotCredential(strings.TrimSpace(strings.TrimPrefix(header, prefix)))
	if err != nil {
		http.Error(w, "Pilot 访问令牌无效", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(pilotReadLimit)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var first pilotUplink
	if err := wsReadJSON(ctx, conn, &first); err != nil || first.Type != "register" {
		g.logger.WithError(err).Warn("Pilot 注册消息无效", "remote", r.RemoteAddr, "message_type", first.Type)
		_ = conn.Close(websocket.StatusPolicyViolation, "首条消息必须是 register")
		return
	}
	// credential 与 register identity 必须一致；这是设备接入唯一需要的身份绑定。
	if first.Pilot.PilotInstanceID != credentialPilotID {
		_ = conn.Close(websocket.StatusPolicyViolation, "Pilot credential 与 pilot_instance_id 不匹配")
		return
	}
	commands, disconnect, err := g.service.ConnectWithSkillSnapshot(first.Pilot, first.Skills)
	if err != nil {
		g.logger.WithError(err).Warn("Pilot 注册被拒绝", "pilot_instance_id", first.Pilot.PilotInstanceID,
			"robot_id", first.Pilot.RobotID)
		_ = conn.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}
	defer disconnect()
	pilotID := first.Pilot.PilotInstanceID

	writeErr := make(chan error, 1)
	go func() {
		for {
			select {
			case command := <-commands:
				writeCtx, writeCancel := context.WithTimeout(context.Background(), pilotWriteTimeout)
				err := wsWriteJSON(writeCtx, conn, command)
				writeCancel()
				if err != nil {
					writeErr <- err
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	_, _ = g.service.sendCommand(pilotID, "reconcile.request", map[string]any{"requested_at": time.Now().UTC()})

	for {
		select {
		case err := <-writeErr:
			g.logger.WithError(err).Warn("Pilot 下行连接中断", "pilot_instance_id", pilotID)
			return
		default:
		}
		var message pilotUplink
		if err := wsReadJSON(ctx, conn, &message); err != nil {
			g.logger.WithError(err).Warn("Pilot 上行连接中断", "pilot_instance_id", pilotID)
			return
		}
		switch message.Type {
		case "event":
			if err := g.service.HandlePilotEvent(pilotID, message.EventType, message.Sequence, message.Payload); err != nil {
				g.logger.WithError(err).Warn("Pilot 事件处理失败", "pilot_instance_id", pilotID, "event_type", message.EventType)
			}
		case "heartbeat":
			if err := g.service.HandlePilotEvent(pilotID, "heartbeat", 0, message.Payload); err != nil {
				g.logger.WithError(err).Warn("Pilot 心跳处理失败", "pilot_instance_id", pilotID)
				return
			}
		case "reconcile":
			snapshot, err := g.service.Reconcile(pilotID, message.Executions)
			if err != nil {
				g.logger.WithError(err).Warn("Pilot Execution 对账失败", "pilot_instance_id", pilotID)
				return
			}
			_, _ = g.service.sendCommand(pilotID, "reconcile.result", map[string]any{"executions": snapshot})
		case "command.ack":
			if err := g.service.HandleCommandAckResult(pilotID, message.CommandID,
				message.OK, message.Error, message.Result); err != nil {
				g.logger.WithError(err).Warn("Pilot 命令执行失败", "pilot_instance_id", pilotID,
					"command_id", message.CommandID)
			}
		default:
			g.logger.Warn("Pilot 发送未知消息", "pilot_instance_id", pilotID, "message_type", message.Type)
			_ = conn.Close(websocket.StatusUnsupportedData, "未知 Pilot 消息类型")
			return
		}
	}
}

func wsReadJSON(ctx context.Context, conn *websocket.Conn, target any) error {
	kind, data, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	if kind != websocket.MessageText {
		return errors.New("Pilot 只接受 JSON 文本消息")
	}
	return json.Unmarshal(data, target)
}
func wsWriteJSON(ctx context.Context, conn *websocket.Conn, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}
