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

// SaveRuntimeBundle 把构建输入保存在 Project 工作区。目录仅供 Framework 恢复和审计，
// Runtime 只接收结构化内容，不会看到此路径。
func (s *FileStore) SaveRuntimeBundle(projectID string, bundle RuntimeBundle) error {
	root, err := s.simulationRoot(projectID)
	if err != nil {
		return err
	}
	path := filepath.Join(root, "runtime-bundles", bundle.RuntimeBundleID+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("创建 RuntimeBundle 目录失败: %w", err)
	}
	return writeJSONAtomic(path, bundle)
}

func (s *FileStore) LoadRuntimeBundle(projectID, bundleID string) (RuntimeBundle, error) {
	if !validBundleID(bundleID) {
		return RuntimeBundle{}, ErrNotFound
	}
	root, err := s.simulationRoot(projectID)
	if err != nil {
		return RuntimeBundle{}, err
	}
	data, err := os.ReadFile(filepath.Join(root, "runtime-bundles", bundleID+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return RuntimeBundle{}, ErrNotFound
	}
	if err != nil {
		return RuntimeBundle{}, err
	}
	var bundle RuntimeBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return RuntimeBundle{}, fmt.Errorf("解析 RuntimeBundle 失败: %w", err)
	}
	return bundle, nil
}

// ListRuntimeBundles 返回 Project 已由 Runtime 验证成功的全部不可变构建。
func (s *FileStore) ListRuntimeBundles(projectID string) ([]RuntimeBundle, error) {
	root, err := s.simulationRoot(projectID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(root, "runtime-bundles"))
	if errors.Is(err, os.ErrNotExist) {
		return []RuntimeBundle{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := make([]RuntimeBundle, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		bundleID := entry.Name()[:len(entry.Name())-len(filepath.Ext(entry.Name()))]
		bundle, loadErr := s.LoadRuntimeBundle(projectID, bundleID)
		if loadErr == nil {
			result = append(result, bundle)
		}
	}
	return result, nil
}

// FindRuntimeBundle 查找指定 revision 已成功保存的构建结果。
// 一个场景可以为多个 profile 构建；发布只要求至少有一个可运行构建。
func (s *FileStore) FindRuntimeBundle(
	projectID, documentID string, revision int64,
) (RuntimeBundle, error) {
	root, err := s.simulationRoot(projectID)
	if err != nil {
		return RuntimeBundle{}, err
	}
	entries, err := os.ReadDir(filepath.Join(root, "runtime-bundles"))
	if errors.Is(err, os.ErrNotExist) {
		return RuntimeBundle{}, ErrNotFound
	}
	if err != nil {
		return RuntimeBundle{}, err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		bundle, loadErr := s.LoadRuntimeBundle(
			projectID, entry.Name()[:len(entry.Name())-len(filepath.Ext(entry.Name()))],
		)
		if loadErr == nil && bundle.DocumentID == documentID && bundle.Revision == revision {
			return bundle, nil
		}
	}
	return RuntimeBundle{}, ErrNotFound
}

func validBundleID(value string) bool {
	const prefix = "runtime-bundle-"
	if len(value) <= len(prefix) || value[:len(prefix)] != prefix {
		return false
	}
	for _, character := range value {
		if !(character == '-' || character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' || character >= '0' && character <= '9') {
			return false
		}
	}
	return true
}
