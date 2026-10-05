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

package tool

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ResolveWorkspacePath 把 Project 内相对路径解析为真实宿主路径，并在解析
// 符号链接后再次检查边界。目标必须已经存在；需要创建目录的执行工具应先
// 完成自己的目录创建，再调用本函数校验。
func ResolveWorkspacePath(workspaceRoot, requested string) (absolutePath, relativePath string, err error) {
	requested = filepath.Clean(strings.TrimSpace(requested))
	if requested == "." || filepath.IsAbs(requested) || requested == ".." ||
		strings.HasPrefix(requested, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("路径必须是 Project workspace 内的相对路径")
	}
	realRoot, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return "", "", fmt.Errorf("Project workspace 无法解析")
	}
	realPath, err := filepath.EvalSymlinks(filepath.Join(workspaceRoot, requested))
	if err != nil {
		return "", "", fmt.Errorf("工作区路径不存在")
	}
	rel, err := filepath.Rel(realRoot, realPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("路径不能通过符号链接越出 Project workspace")
	}
	return realPath, filepath.ToSlash(rel), nil
}
