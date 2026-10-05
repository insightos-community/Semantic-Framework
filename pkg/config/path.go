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

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ConfigPathEnv 是配置文件路径环境变量；仅在未提供 -c 时生效。
const ConfigPathEnv = "SEMANTIC_CONFIG"

// DefaultPath 返回 semantic init 为当前用户安装的默认配置文件路径。
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("定位用户目录失败: %w", err)
	}
	return filepath.Join(home, ".semantic", "configs", "semantic-server.yaml"), nil
}

// ResolvePath 按“显式 -c > SEMANTIC_CONFIG > 默认安装路径”的优先级解析
// 配置文件，并返回清理后的绝对路径。该返回值是启动加载、热重载与设置 API
// 共用的唯一读写目标，避免前端把设置误写回仓库中的只读模板。
func ResolvePath(explicit string) (string, error) {
	path := strings.TrimSpace(explicit)
	if path == "" {
		path = strings.TrimSpace(os.Getenv(ConfigPathEnv))
	}
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return "", err
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("解析配置文件路径 %s 失败: %w", path, err)
	}
	return filepath.Clean(abs), nil
}
