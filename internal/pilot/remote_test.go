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

package pilot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"insightos.cn/semantic-framework/internal/store"
)

func TestRemoteClientAcceptsLargeServerCommand(t *testing.T) {
	acknowledged := make(chan map[string]any, 1)
	serverErrors := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(response, request, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, _, err := conn.Read(ctx); err != nil {
			serverErrors <- err
			return
		}
		body, err := json.Marshal(map[string]any{
			"type":       "reconcile.result",
			"command_id": "command-large",
			"payload":    map[string]any{"checkpoint": strings.Repeat("x", 64<<10)},
		})
		if err != nil {
			serverErrors <- err
			return
		}
		if err := conn.Write(ctx, websocket.MessageText, body); err != nil {
			serverErrors <- err
			return
		}
		_, responseBody, err := conn.Read(ctx)
		if err != nil {
			serverErrors <- err
			return
		}
		var ack map[string]any
		if err := json.Unmarshal(responseBody, &ack); err != nil {
			serverErrors <- err
			return
		}
		acknowledged <- ack
	}))
	defer server.Close()

	client := NewRemoteClient(RemoteClientConfig{
		ServerWebSocketURL: "ws" + strings.TrimPrefix(server.URL, "http"),
		AccessToken:        "pilot-test",
		Pilot: store.RobotPilot{
			PilotInstanceID: "pilot-large", RobotID: "robot-large",
		},
	}, nil, NewMemorySkillExecutionStore(), nil, nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.runConnection(ctx) }()

	select {
	case err := <-serverErrors:
		t.Fatal(err)
	case ack := <-acknowledged:
		if ack["type"] != "command.ack" || ack["command_id"] != "command-large" || ack["ok"] != true {
			t.Fatalf("unexpected ack: %#v", ack)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Pilot did not acknowledge a valid command larger than 32 KiB")
	}
}
