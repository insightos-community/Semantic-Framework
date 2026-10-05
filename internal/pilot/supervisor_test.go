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
	"strings"
	"testing"
	"time"
)

func TestLargeWorkerCheckpointDoesNotInterruptFollowingResponse(t *testing.T) {
	payload := strings.Repeat("trajectory-data", 150000)
	checkpoint, err := json.Marshal(rpcMessage{JSONRPC: "2.0", Method: "checkpoint", Params: map[string]any{"state": payload}})
	if err != nil {
		t.Fatal(err)
	}
	response := make(chan rpcMessage, 1)
	process := &WorkerProcess{pending: map[string]chan rpcMessage{"pilot-1": response}, requests: make(chan rpcMessage, 1)}
	process.readStdout(strings.NewReader(string(checkpoint) + "\n" + `{"jsonrpc":"2.0","id":"pilot-1","result":{"ok":true}}` + "\n"))
	select {
	case message := <-process.requests:
		if message.Method != "checkpoint" || message.Params["state"] != payload {
			t.Fatal("large checkpoint was truncated or changed")
		}
	default:
		t.Fatal("large checkpoint was not delivered")
	}
	select {
	case message := <-response:
		if message.Error != nil || message.Result.(map[string]any)["ok"] != true {
			t.Fatalf("following response failed: %#v", message)
		}
	default:
		t.Fatal("following response was not delivered")
	}
}

func TestOversizedWorkerMessageStillFailsPendingCall(t *testing.T) {
	response := make(chan rpcMessage, 1)
	process := &WorkerProcess{pending: map[string]chan rpcMessage{"pilot-1": response}, requests: make(chan rpcMessage, 1)}
	process.readStdout(strings.NewReader(`{"jsonrpc":"2.0","method":"checkpoint","params":{"state":"` + strings.Repeat("x", 9*1024*1024) + `"}}` + "\n"))
	select {
	case message := <-response:
		if !strings.Contains(stringValue(message.Error["message"]), "token too long") {
			t.Fatalf("unexpected error: %#v", message.Error)
		}
	default:
		t.Fatal("oversized message did not fail the pending call")
	}
}

func TestWorkerStdoutEOFFailsPendingCall(t *testing.T) {
	response := make(chan rpcMessage, 1)
	process := &WorkerProcess{
		pending:  map[string]chan rpcMessage{"pilot-1": response},
		requests: make(chan rpcMessage, 1),
	}

	process.readStdout(strings.NewReader(""))

	select {
	case message := <-response:
		if !strings.Contains(stringValue(message.Error["message"]), "stdout closed") {
			t.Fatalf("unexpected EOF error: %#v", message.Error)
		}
	case <-time.After(time.Second):
		t.Fatal("stdout EOF did not wake the pending JSON-RPC call")
	}
}

func TestWorkerResponseWaitsForPreviouslyReadRequests(t *testing.T) {
	process := &WorkerProcess{
		done:            make(chan struct{}),
		requestProgress: make(chan struct{}, 1),
	}
	process.requestQueued.Store(2)

	finished := make(chan error, 1)
	go func() {
		finished <- process.waitRequestsHandled(context.Background(), 2)
	}()

	process.markRequestHandled(1)
	select {
	case err := <-finished:
		t.Fatalf("barrier returned before all requests were handled: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	process.markRequestHandled(2)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("barrier returned an error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("barrier did not unblock after the final request")
	}
}

func TestWorkerStdoutEOFWithoutPendingCallIsNormal(t *testing.T) {
	process := &WorkerProcess{
		pending:  make(map[string]chan rpcMessage),
		requests: make(chan rpcMessage, 1),
	}

	process.readStdout(strings.NewReader(""))

	process.waitErrMu.RLock()
	err := process.waitErr
	process.waitErrMu.RUnlock()
	if err != nil {
		t.Fatalf("normal stdout EOF was recorded as worker failure: %v", err)
	}
}
