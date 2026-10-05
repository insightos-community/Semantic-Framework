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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAbilityFrameworkClientUsesExactInstanceAndManifestTasks(t *testing.T) {
	const instanceID = "11111111-2222-3333-4444-555555555555"
	var mu sync.Mutex
	starts := make([]map[string]any, 0)
	statuses := map[string]map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ability/"+instanceID+"/api/task/start" && r.URL.Path != "/api/ability/"+instanceID+"/api/task/status" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path[len(r.URL.Path)-5:] == "start" {
			mu.Lock()
			starts = append(starts, body)
			number := len(starts)
			mu.Unlock()
			taskID := "task-" + string(rune('0'+number))
			input, _ := body["input"].(map[string]any)
			taskType := int(body["task_type"].(float64))
			payload := map[string]any{"status": "running"}
			if taskType == 3 {
				payload = map[string]any{"status": "succeeded", "feedback": []any{}, "observations": []any{}, "result": map[string]any{"invocation_id": input["invocation_id"]}}
			} else if taskType == 4 {
				payload = map[string]any{"status": "stopped", "feedback": []any{}, "observations": []any{}, "result": map[string]any{"stop_evidence": true}}
			}
			statuses[taskID] = map[string]any{"status": "completed", "payload": payload}
			_ = json.NewEncoder(w).Encode(map[string]any{"task_id": taskID})
			return
		}
		taskID, _ := body["task_id"].(string)
		_ = json.NewEncoder(w).Encode(statuses[taskID])
	}))
	defer server.Close()

	client := NewAbilityFrameworkClient(server.URL, []AbilityTaskCatalog{{InstanceID: instanceID, TaskTypes: map[string]int{
		"FollowRoute": 1, "GetExecution": 3, "StopExecution": 4}}})
	client.PollInterval = time.Millisecond
	started, err := client.StartTask(context.Background(), instanceID, "FollowRoute", map[string]any{"invocation_id": "inv-1"})
	if err != nil || started.TaskID == "" {
		t.Fatalf("StartTask: %#v %v", started, err)
	}
	execution, err := client.GetExecution(context.Background(), instanceID, "inv-1", 7)
	if err != nil || execution.Status != "succeeded" {
		t.Fatalf("GetExecution: %#v %v", execution, err)
	}
	stopped, err := client.StopExecution(context.Background(), instanceID, "inv-1", "user stop")
	if err != nil || stopped.Status != "stopped" {
		t.Fatalf("StopExecution: %#v %v", stopped, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 3 {
		t.Fatalf("start 调用次数=%d", len(starts))
	}
	if int(starts[1]["task_type"].(float64)) != 3 || int(starts[2]["task_type"].(float64)) != 4 {
		t.Fatalf("Get/Stop 未按 Manifest Task 调用: %#v", starts)
	}
}

func TestAbilityFrameworkClientDoesNotRetryUnconfirmedStart(t *testing.T) {
	const instanceID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	var count atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("httptest writer cannot hijack")
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}))
	defer server.Close()
	client := NewAbilityFrameworkClient(server.URL, []AbilityTaskCatalog{{InstanceID: instanceID, TaskTypes: map[string]int{"FollowRoute": 1}}})
	_, err := client.StartTask(context.Background(), instanceID, "FollowRoute", map[string]any{"invocation_id": "inv-physical"})
	if err == nil {
		t.Fatal("连接中断必须返回未确认错误")
	}
	if count.Load() != 1 {
		t.Fatalf("物理 start 不得自动重试，实际 %d 次", count.Load())
	}
}

func TestAbilityFrameworkClientRejectsUncataloguedTask(t *testing.T) {
	client := NewAbilityFrameworkClient("http://127.0.0.1:1", []AbilityTaskCatalog{{InstanceID: "instance-a", TaskTypes: map[string]int{"GetExecution": 1}}})
	if _, err := client.StartTask(context.Background(), "instance-a", "FollowRoute", nil); err == nil {
		t.Fatal("Manifest 未声明的 Task 必须在发请求前拒绝")
	}
	if _, err := client.StartTask(context.Background(), "instance-b", "GetExecution", nil); err == nil {
		t.Fatal("未登记实例必须拒绝")
	}
}

func TestAbilityFrameworkClientClassifiesStartValidationRejection(t *testing.T) {
	const instanceID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ability/"+instanceID+"/api/task/start" {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "invalid MoveEndEffector input", http.StatusUnprocessableEntity)
	}))
	defer server.Close()

	client := NewAbilityFrameworkClient(server.URL, []AbilityTaskCatalog{{
		InstanceID: instanceID, TaskTypes: map[string]int{"MoveEndEffector": 1},
	}})
	_, err := client.StartTask(context.Background(), instanceID, "MoveEndEffector", map[string]any{"purpose": "unknown"})
	rejected, ok := err.(*AbilityStartRejectedError)
	if !ok || rejected.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("4xx start 必须返回明确拒绝: %#v", err)
	}
}

func TestAbilityFrameworkClientClassifiesAsyncInputRejection(t *testing.T) {
	const instanceID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/ability/" + instanceID + "/api/task/start":
			_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "task-invalid"})
		case "/api/ability/" + instanceID + "/api/task/status":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "completed",
				"payload": map[string]any{
					"request_rejected": map[string]any{
						"code": "INVALID_TASK_INPUT", "message": "coordination 无效",
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewAbilityFrameworkClient(server.URL, []AbilityTaskCatalog{{
		InstanceID: instanceID, TaskTypes: map[string]int{"MoveEndEffector": 1},
	}})
	client.PollInterval = time.Millisecond
	_, err := client.StartTask(context.Background(), instanceID, "MoveEndEffector", map[string]any{"coordination": "independent"})
	rejected, ok := err.(*AbilityStartRejectedError)
	if !ok || rejected.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("异步参数校验必须返回明确拒绝: %#v", err)
	}
}
