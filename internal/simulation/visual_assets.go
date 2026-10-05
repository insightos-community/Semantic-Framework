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
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

const maxVisualAssetBytes = 64 << 20

// RuntimeVisualAssetClient 是可选的浏览器视觉内容能力。
// 它不进入 RuntimeClient 主接口，旧 Fake Runtime 与不提供编辑视觉的 Profile
// 可以继续工作；请求视觉内容时才明确返回“不支持”。
type RuntimeVisualAssetClient interface {
	VisualAsset(context.Context, string, string) ([]byte, string, error)
}

func validVisualToken(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') ||
			(char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') ||
			char == '-' || char == '_' || char == '.' {
			continue
		}
		return false
	}
	return true
}

// VisualAsset 读取 Runtime Pack 已解析的视觉内容，不接受路径或任意 URL。
func (c *HTTPRuntimeClient) VisualAsset(
	ctx context.Context, visualID, version string,
) ([]byte, string, error) {
	if !validVisualToken(visualID) || !validVisualToken(version) {
		return nil, "", fmt.Errorf("%w: visual_id 或 version 非法", ErrConflict)
	}
	path := "/api/v1/visual-assets/" + url.PathEscape(visualID) +
		"/" + url.PathEscape(version) + ".glb"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+path, nil)
	if err != nil {
		return nil, "", fmt.Errorf("创建 Runtime 视觉资源请求失败: %w", err)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if response.StatusCode == http.StatusNotFound {
			return nil, "", fmt.Errorf("%w: %s", ErrNotFound, string(data))
		}
		return nil, "", fmt.Errorf("Runtime 视觉资源请求失败 %s: %s",
			response.Status, string(data))
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "model/gltf-binary" {
		return nil, "", fmt.Errorf("Runtime 视觉资源 Content-Type 无效: %q",
			response.Header.Get("Content-Type"))
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, maxVisualAssetBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("读取 Runtime 视觉资源失败: %w", err)
	}
	if len(content) > maxVisualAssetBytes {
		return nil, "", fmt.Errorf("Runtime 视觉资源超过 %d bytes", maxVisualAssetBytes)
	}
	if len(content) < 12 || !bytes.Equal(content[:4], []byte("glTF")) {
		return nil, "", fmt.Errorf("Runtime 返回的视觉资源不是 GLB")
	}
	return content, mediaType, nil
}

// ProjectVisualAsset 从当前 Project 偏好或唯一兼容 Installation 获取内容。
// Ensure 仅保证 Runtime 已连接，不会启动 Scene Instance。
func (s *Service) ProjectVisualAsset(
	ctx context.Context, projectID, visualID, version string,
) ([]byte, string, error) {
	if !validVisualToken(visualID) || !validVisualToken(version) {
		return nil, "", fmt.Errorf("%w: visual_id 或 version 非法", ErrConflict)
	}
	installation, err := s.SelectProjectRuntimeInstallation(projectID, "", "")
	if err != nil {
		return nil, "", err
	}
	if _, err := s.EnsureRuntimeInstallation(ctx, installation.InstallationID); err != nil {
		return nil, "", err
	}
	binding, err := s.registry.Binding(installation.InstallationID)
	if err != nil {
		return nil, "", err
	}
	client, ok := binding.Client.(RuntimeVisualAssetClient)
	if !ok {
		return nil, "", fmt.Errorf("%w: Runtime %s 不提供浏览器视觉资源",
			ErrNotFound, installation.InstallationID)
	}
	content, mediaType, err := client.VisualAsset(ctx, strings.TrimSpace(visualID),
		strings.TrimSpace(version))
	if err != nil {
		return nil, "", err
	}
	return content, mediaType, nil
}
