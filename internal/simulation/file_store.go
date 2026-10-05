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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WorkspaceResolver 只返回 Framework 已分配的 Project 工作区。
type WorkspaceResolver interface {
	ProjectWorkspace(string) (string, error)
}

// FileStore 把仿真恢复状态和场景文档保存在对应 Project 工作区。
type FileStore struct {
	resolver WorkspaceResolver
}

func NewFileStore(resolver WorkspaceResolver) *FileStore {
	return &FileStore{resolver: resolver}
}

func (s *FileStore) LoadRuntimeState(projectID string) (ProjectRuntimeState, error) {
	path, err := s.runtimeStatePath(projectID)
	if err != nil {
		return ProjectRuntimeState{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ProjectRuntimeState{}, ErrNotFound
	}
	if err != nil {
		return ProjectRuntimeState{}, fmt.Errorf("读取仿真恢复状态失败: %w", err)
	}
	var result ProjectRuntimeState
	if err := json.Unmarshal(data, &result); err != nil {
		return ProjectRuntimeState{}, fmt.Errorf("解析仿真恢复状态失败: %w", err)
	}
	return result, nil
}

func (s *FileStore) SaveRuntimeState(state ProjectRuntimeState) error {
	path, err := s.runtimeStatePath(state.ProjectID)
	if err != nil {
		return err
	}
	return writeJSONAtomic(path, state)
}

func (s *FileStore) simulationRoot(projectID string) (string, error) {
	root, err := s.resolver.ProjectWorkspace(projectID)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, ".semantic", "simulation")
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", fmt.Errorf("创建 Project 仿真目录失败: %w", err)
	}
	return path, nil
}

func (s *FileStore) runtimeStatePath(projectID string) (string, error) {
	root, err := s.simulationRoot(projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "runtime-state.json"), nil
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("编码 JSON 文件失败: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".semantic-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(append(data, '\n')); err != nil {
		_ = temp.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("同步临时文件失败: %w", err)
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("替换 JSON 文件失败: %w", err)
	}
	return nil
}
