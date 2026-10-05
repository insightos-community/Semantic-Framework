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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// AbilityTaskCatalog 来自 Ability Manifest/实例目录。Pilot 始终按精确
// instance UUID 和 Manifest taskType 调用，不按 Ability 名称寻找端口。
type AbilityTaskCatalog struct {
	InstanceID string
	TaskTypes  map[string]int
}

// AbilityStartRejectedError 表示 AbilityFramework 在登记 Task 之前明确拒绝了请求。
// 这类错误与网络断连不同：前者可以确定没有产生物理动作，应当收敛为 failed
// 并释放 Robot；后者无法确认请求是否已经被执行，必须保留 interrupted 和资源锁。
type AbilityStartRejectedError struct {
	StatusCode int
	Message    string
}

func (e *AbilityStartRejectedError) Error() string {
	return fmt.Sprintf("AbilityFramework HTTP %d: %s", e.StatusCode, e.Message)
}

// AbilityFrameworkClient 只连接本 Robot 对应的 AbilityFramework。Framework
// 的实例代理负责转发到 Ability 进程；Pilot 不读取 Ability 端口。
type AbilityFrameworkClient struct {
	BaseURL      string
	HTTPClient   *http.Client
	PollInterval time.Duration

	mu        sync.RWMutex
	instances map[string]map[string]int
}

func NewAbilityFrameworkClient(baseURL string, catalogs []AbilityTaskCatalog) *AbilityFrameworkClient {
	client := &AbilityFrameworkClient{BaseURL: strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 30 * time.Second}, PollInterval: 20 * time.Millisecond,
		instances: make(map[string]map[string]int)}
	for _, catalog := range catalogs {
		client.SetCatalog(catalog)
	}
	return client
}

func (c *AbilityFrameworkClient) SetCatalog(catalog AbilityTaskCatalog) {
	copyTypes := make(map[string]int, len(catalog.TaskTypes))
	for name, taskType := range catalog.TaskTypes {
		copyTypes[name] = taskType
	}
	c.mu.Lock()
	c.instances[catalog.InstanceID] = copyTypes
	c.mu.Unlock()
}

// ReplaceCatalogs 只在完整的 heartbeat/CR 轮询成功后切换目录。旧实例不会
// 因为本轮缺失而继续被调用，正在运行的 invocation 仍通过其精确 instance ID
// 对账，发现器会把所有仍有 heartbeat 的实例放进 catalogs。
func (c *AbilityFrameworkClient) ReplaceCatalogs(catalogs []AbilityTaskCatalog) {
	next := make(map[string]map[string]int, len(catalogs))
	for _, catalog := range catalogs {
		tasks := make(map[string]int, len(catalog.TaskTypes))
		for name, taskType := range catalog.TaskTypes {
			tasks[name] = taskType
		}
		next[catalog.InstanceID] = tasks
	}
	c.mu.Lock()
	c.instances = next
	c.mu.Unlock()
}

func (c *AbilityFrameworkClient) StartTask(ctx context.Context, instanceID, taskName string, input map[string]any) (AbilityTask, error) {
	taskID, err := c.start(ctx, instanceID, taskName, input)
	if err != nil {
		return AbilityTask{}, err
	}
	// ability_py.start_task 先返回 Task ID，再在线程中调用 Ability Task。
	// 这里等待的是“invocation 已登记”这一步，不是等待 Robot 物理动作结束；
	// 也不会再次发送 start。否则 Worker 可能在 invocation 写入 Execution Store
	// 前调用 GetExecution，并把一个正常的异步启动误判为中断。
	payload, err := c.waitTask(ctx, instanceID, taskID)
	if err != nil {
		return AbilityTask{TaskID: taskID}, err
	}
	if rejected, ok := payload["request_rejected"].(map[string]any); ok {
		message, _ := rejected["message"].(string)
		if message == "" {
			message = "Ability 拒绝了 Task 输入"
		}
		return AbilityTask{TaskID: taskID}, &AbilityStartRejectedError{
			StatusCode: http.StatusUnprocessableEntity,
			Message:    message,
		}
	}
	return AbilityTask{TaskID: taskID}, nil
}

func (c *AbilityFrameworkClient) GetExecution(ctx context.Context, instanceID, invocationID string, afterSequence int64) (AbilityExecution, error) {
	payload, err := c.invokeAndWait(ctx, instanceID, "GetExecution", map[string]any{
		"invocation_id": invocationID, "after_feedback_sequence": afterSequence})
	if err != nil {
		return AbilityExecution{}, err
	}
	return decodeAbilityExecution(payload)
}

func (c *AbilityFrameworkClient) StopExecution(ctx context.Context, instanceID, invocationID, reason string) (AbilityExecution, error) {
	payload, err := c.invokeAndWait(ctx, instanceID, "StopExecution", map[string]any{
		"invocation_id": invocationID, "reason": reason})
	if err != nil {
		return AbilityExecution{}, err
	}
	return decodeAbilityExecution(payload)
}

func (c *AbilityFrameworkClient) start(ctx context.Context, instanceID, taskName string, input map[string]any) (string, error) {
	taskType, err := c.taskType(instanceID, taskName)
	if err != nil {
		return "", err
	}
	var response struct {
		TaskID string `json:"task_id"`
	}
	// http.Client 不装配重试 Transport。请求结果未知时上层将 invocation 标为
	// interrupted，只能通过 GetExecution 对账，不能再次 start 物理 Task。
	if err := c.postStart(ctx, instanceID, map[string]any{"task_type": taskType, "input": input}, &response); err != nil {
		return "", err
	}
	if response.TaskID == "" {
		return "", errors.New("AbilityFramework start 响应缺少 task_id")
	}
	return response.TaskID, nil
}

func (c *AbilityFrameworkClient) postStart(ctx context.Context, instanceID string, request, response any) error {
	err := c.post(ctx, instanceID, "/api/task/start", request, response)
	var httpErr *abilityHTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode >= 400 && httpErr.StatusCode < 500 {
		return &AbilityStartRejectedError{StatusCode: httpErr.StatusCode, Message: httpErr.Message}
	}
	return err
}

type abilityHTTPError struct {
	StatusCode int
	Message    string
}

func (e *abilityHTTPError) Error() string {
	return fmt.Sprintf("AbilityFramework HTTP %d: %s", e.StatusCode, e.Message)
}

func (c *AbilityFrameworkClient) invokeAndWait(ctx context.Context, instanceID, taskName string, input map[string]any) (map[string]any, error) {
	taskID, err := c.start(ctx, instanceID, taskName, input)
	if err != nil {
		return nil, err
	}
	return c.waitTask(ctx, instanceID, taskID)
}

func (c *AbilityFrameworkClient) waitTask(ctx context.Context, instanceID, taskID string) (map[string]any, error) {
	interval := c.PollInterval
	if interval <= 0 {
		interval = 20 * time.Millisecond
	}
	for {
		var status struct {
			Status  string         `json:"status"`
			Payload map[string]any `json:"payload"`
			Message string         `json:"message"`
		}
		if err := c.post(ctx, instanceID, "/api/task/status", map[string]any{"task_id": taskID}, &status); err != nil {
			return nil, err
		}
		switch status.Status {
		case "completed":
			if status.Payload == nil {
				return nil, errors.New("Ability Task completed 但缺少 payload")
			}
			return status.Payload, nil
		case "failed", "cancelled":
			if status.Message == "" {
				status.Message = "Ability Task " + status.Status
			}
			return nil, errors.New(status.Message)
		case "running":
		default:
			return nil, fmt.Errorf("未知 Ability Task 状态 %q", status.Status)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *AbilityFrameworkClient) taskType(instanceID, taskName string) (int, error) {
	c.mu.RLock()
	tasks, ok := c.instances[instanceID]
	taskType, found := tasks[taskName]
	c.mu.RUnlock()
	if !ok {
		return 0, fmt.Errorf("Ability 实例 %s 不在当前 Robot 目录", instanceID)
	}
	if !found {
		return 0, fmt.Errorf("Ability 实例 %s 的 Manifest 未声明 Task %s", instanceID, taskName)
	}
	return taskType, nil
}

func (c *AbilityFrameworkClient) post(ctx context.Context, instanceID, subpath string, request any, response any) error {
	if _, err := url.ParseRequestURI(c.BaseURL); err != nil {
		return fmt.Errorf("AbilityFramework endpoint 无效: %w", err)
	}
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	endpoint := c.BaseURL + "/api/ability/" + url.PathEscape(instanceID) + subpath
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpResponse, err := c.HTTPClient.Do(httpRequest)
	if err != nil {
		return err
	}
	defer httpResponse.Body.Close()
	limited, err := io.ReadAll(io.LimitReader(httpResponse.Body, 4<<20))
	if err != nil {
		return err
	}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		return &abilityHTTPError{StatusCode: httpResponse.StatusCode, Message: strings.TrimSpace(string(limited))}
	}
	if err := json.Unmarshal(limited, response); err != nil {
		return fmt.Errorf("解析 AbilityFramework 响应失败: %w", err)
	}
	return nil
}

func decodeAbilityExecution(value any) (AbilityExecution, error) {
	if object, ok := value.(map[string]any); ok {
		if nested, exists := object["execution"]; exists {
			value = nested
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return AbilityExecution{}, err
	}
	var result AbilityExecution
	if err := json.Unmarshal(encoded, &result); err != nil {
		return AbilityExecution{}, err
	}
	if result.Status == "" {
		return AbilityExecution{}, fmt.Errorf("ability execution status is empty")
	}
	return result, nil
}
