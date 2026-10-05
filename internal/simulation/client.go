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

package simulation

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
	"time"
)

var (
	ErrRuntimeUnavailable = errors.New("仿真 Runtime 不可用")
	ErrConflict           = errors.New("仿真状态冲突")
	ErrNotFound           = errors.New("仿真资源不存在")
)

// RuntimeClient 是 Framework 使用的外部 Runtime 公共接口。
// 所有 Robot 调试操作都使用明确方法，禁止拼接任意路径透传请求。
type RuntimeClient interface {
	Health(context.Context) error
	Runtime(context.Context) (RuntimeInfo, error)
	RuntimeProfiles(context.Context) ([]RuntimeProfile, error)
	ListScenes(context.Context) ([]SceneDescriptor, error)
	RegisterRuntimeBundle(context.Context, RuntimeBundle) (RuntimeBundleResult, error)
	StartScene(context.Context, string, SceneStartRequest) (SceneInstance, error)
	Scene(context.Context, string) (SceneInstance, error)
	SceneOperation(context.Context, string, string, any) (SceneInstance, error)
	SceneSnapshot(context.Context, string) (SceneSnapshot, error)
	SceneEvaluation(context.Context, string) (SceneEvaluation, error)
	Robots(context.Context, string) ([]VirtualRobotDescriptor, error)
	SensorFramesURL(string, string) string
	RobotState(context.Context, string) (RobotStateSnapshot, error)
	RobotSensors(context.Context, string) ([]SensorDescriptor, error)
	SubmitRobotCommand(context.Context, string, RobotDebugCommand) (RobotCommandRecord, error)
	RobotCommand(context.Context, string, string) (RobotCommandRecord, error)
	StopRobotCommand(context.Context, string, string) (RobotCommandRecord, error)
	HoldRobot(context.Context, string, int64) (RobotCommandRecord, error)
}

// HTTPRuntimeClient 通过 Plugin HTTP 接口管理场景、视觉内容和受限 Robot 调试操作。
type HTTPRuntimeClient struct {
	endpoint        string
	client          *http.Client
	lifecycleClient *http.Client
}

func NewHTTPRuntimeClient(endpoint string, client *http.Client) *HTTPRuntimeClient {
	lifecycleClient := client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
		// reset/stop 包含原生场景资源清理，预算独立于快速状态读取。
		// 创建仍返回 starting，长时间的就绪等待由 Profile 预算和状态轮询管理。
		lifecycleClient = &http.Client{Timeout: 90 * time.Second}
	}
	return &HTTPRuntimeClient{endpoint: strings.TrimRight(endpoint, "/"), client: client, lifecycleClient: lifecycleClient}
}

func (c *HTTPRuntimeClient) Health(ctx context.Context) error {
	var reply map[string]any
	return c.do(ctx, http.MethodGet, "/healthz", nil, &reply)
}

func (c *HTTPRuntimeClient) Runtime(ctx context.Context) (RuntimeInfo, error) {
	var result RuntimeInfo
	err := c.do(ctx, http.MethodGet, "/api/v1/runtime", nil, &result)
	result.Endpoint = c.endpoint
	return result, err
}

// RuntimeProfiles 读取进程实际提供的环境能力，供 Framework 验证部署配置，
// 不能仅凭启动参数假设 Runtime 内已安装对应 Loader、Robot 或 evaluator。
func (c *HTTPRuntimeClient) RuntimeProfiles(ctx context.Context) ([]RuntimeProfile, error) {
	var result []RuntimeProfile
	return result, c.do(ctx, http.MethodGet, "/api/v1/runtime-profiles", nil, &result)
}

func (c *HTTPRuntimeClient) ListScenes(ctx context.Context) ([]SceneDescriptor, error) {
	var result []SceneDescriptor
	return result, c.do(ctx, http.MethodGet, "/api/v1/scenes", nil, &result)
}

func (c *HTTPRuntimeClient) RegisterRuntimeBundle(
	ctx context.Context, bundle RuntimeBundle,
) (RuntimeBundleResult, error) {
	var result RuntimeBundleResult
	return result, c.do(ctx, http.MethodPost, "/api/v1/runtime-bundles", bundle, &result)
}

func (c *HTTPRuntimeClient) StartScene(
	ctx context.Context, sceneKey string, request SceneStartRequest,
) (SceneInstance, error) {
	var result SceneInstance
	path := "/api/v1/scenes/" + url.PathEscape(sceneKey) + "/instances"
	// runtime_installation_id 只用于 Framework 在多个本地或远程安装之间选择
	// 连接目标，不属于 Runtime 的公共 SceneStartRequest。这里必须在出站前剥离，
	// 否则采用严格请求模型的 Runtime 会把正确的产品启动请求拒绝为未知字段。
	request.RuntimeInstallationID = ""
	if strings.TrimSpace(request.RenderBackend) == "" {
		request.RenderBackend = "auto"
	}
	return result, c.do(ctx, http.MethodPost, path, request, &result)
}

func (c *HTTPRuntimeClient) Scene(ctx context.Context, instanceID string) (SceneInstance, error) {
	var result SceneInstance
	return result, c.do(ctx, http.MethodGet,
		"/api/v1/scene-instances/"+url.PathEscape(instanceID), nil, &result)
}

func (c *HTTPRuntimeClient) SceneOperation(
	ctx context.Context, instanceID, operation string, body any,
) (SceneInstance, error) {
	var result SceneInstance
	path := "/api/v1/scene-instances/" + url.PathEscape(instanceID) + "/" + operation
	if operation == "reset" || operation == "stop" {
		// 使用请求局部副本，不能修改共享 client 的 Timeout，影响并发查询。
		lifecycle := *c
		lifecycle.client = c.lifecycleClient
		return result, lifecycle.do(ctx, http.MethodPost, path, body, &result)
	}
	return result, c.do(ctx, http.MethodPost, path, body, &result)
}

func (c *HTTPRuntimeClient) SceneSnapshot(ctx context.Context, instanceID string) (SceneSnapshot, error) {
	var result SceneSnapshot
	path := "/api/v1/scene-instances/" + url.PathEscape(instanceID) + "/snapshot"
	return result, c.do(ctx, http.MethodGet, path, nil, &result)
}

func (c *HTTPRuntimeClient) SceneEvaluation(ctx context.Context, instanceID string) (SceneEvaluation, error) {
	var result SceneEvaluation
	path := "/api/v1/scene-instances/" + url.PathEscape(instanceID) + "/evaluation"
	return result, c.do(ctx, http.MethodGet, path, nil, &result)
}

func (c *HTTPRuntimeClient) Robots(ctx context.Context, instanceID string) ([]VirtualRobotDescriptor, error) {
	var result []VirtualRobotDescriptor
	path := "/api/v1/scene-instances/" + url.PathEscape(instanceID) + "/robots"
	return result, c.do(ctx, http.MethodGet, path, nil, &result)
}

func (c *HTTPRuntimeClient) SensorFramesURL(robotID, sensorID string) string {
	return c.streamURL("/api/v1/robots/" + url.PathEscape(robotID) +
		"/sensors/" + url.PathEscape(sensorID) + "/stream")
}

func (c *HTTPRuntimeClient) RobotState(ctx context.Context, robotID string) (RobotStateSnapshot, error) {
	var result RobotStateSnapshot
	path := "/api/v1/robots/" + url.PathEscape(robotID) + "/state"
	return result, c.do(ctx, http.MethodGet, path, nil, &result)
}

func (c *HTTPRuntimeClient) RobotSensors(ctx context.Context, robotID string) ([]SensorDescriptor, error) {
	var result []SensorDescriptor
	path := "/api/v1/robots/" + url.PathEscape(robotID) + "/sensors"
	return result, c.do(ctx, http.MethodGet, path, nil, &result)
}

func (c *HTTPRuntimeClient) SubmitRobotCommand(
	ctx context.Context, robotID string, command RobotDebugCommand,
) (RobotCommandRecord, error) {
	var result RobotCommandRecord
	path := "/api/v1/robots/" + url.PathEscape(robotID) + "/commands"
	return result, c.do(ctx, http.MethodPost, path, command, &result)
}

func (c *HTTPRuntimeClient) RobotCommand(
	ctx context.Context, robotID, commandID string,
) (RobotCommandRecord, error) {
	var result RobotCommandRecord
	path := "/api/v1/robots/" + url.PathEscape(robotID) +
		"/commands/" + url.PathEscape(commandID)
	return result, c.do(ctx, http.MethodGet, path, nil, &result)
}

func (c *HTTPRuntimeClient) StopRobotCommand(
	ctx context.Context, robotID, commandID string,
) (RobotCommandRecord, error) {
	var result RobotCommandRecord
	path := "/api/v1/robots/" + url.PathEscape(robotID) +
		"/commands/" + url.PathEscape(commandID) + "/stop"
	return result, c.do(ctx, http.MethodPost, path, map[string]any{}, &result)
}

func (c *HTTPRuntimeClient) HoldRobot(
	ctx context.Context, robotID string, generation int64,
) (RobotCommandRecord, error) {
	var result RobotCommandRecord
	path := "/api/v1/robots/" + url.PathEscape(robotID) + "/hold"
	body := map[string]int64{"scene_generation": generation}
	return result, c.do(ctx, http.MethodPost, path, body, &result)
}

func (c *HTTPRuntimeClient) streamURL(path string) string {
	parsed, err := url.Parse(c.endpoint)
	if err != nil {
		return ""
	}
	if parsed.Scheme == "https" {
		parsed.Scheme = "wss"
	} else {
		parsed.Scheme = "ws"
	}
	parsed.Path = path
	return parsed.String()
}

func (c *HTTPRuntimeClient) do(ctx context.Context, method, path string, body, result any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("编码 Runtime 请求失败: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, reader)
	if err != nil {
		return fmt.Errorf("创建 Runtime 请求失败: %w", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		switch response.StatusCode {
		case http.StatusNotFound:
			return fmt.Errorf("%w: %s", ErrNotFound, string(data))
		case http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity:
			return fmt.Errorf("%w: %s", ErrConflict, string(data))
		default:
			return fmt.Errorf("Runtime 请求失败 %s: %s", response.Status, string(data))
		}
	}
	if result == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(result); err != nil {
		return fmt.Errorf("解析 Runtime 响应失败: %w", err)
	}
	return nil
}
