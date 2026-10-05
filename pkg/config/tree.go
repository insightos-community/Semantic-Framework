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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// tree.go 提供配置快照的"通用键值树"视图：settings REST 的 GET 序列化、
// PATCH 的 merge patch 计算与 base_hash 乐观锁都建立在这棵树上。
// 树以 yaml 键名为路径（与配置文件一致），Duration 以 "10s" 风格字符串表示。

// Tree 将配置快照导出为通用键值树（map 键即 yaml 键名）。
// 经 yaml 序列化/反序列化往返生成，保证与配置文件的键结构一一对应。
func Tree(cfg *Config) (map[string]any, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("序列化配置快照失败: %w", err)
	}
	var tree map[string]any
	if err := yaml.Unmarshal(data, &tree); err != nil {
		return nil, fmt.Errorf("构建配置树失败: %w", err)
	}
	return tree, nil
}

// TreeHash 计算配置树的 base_hash：对规范化 JSON（map 键排序，
// encoding/json 保证）取 SHA-256。同一快照任意时刻计算结果一致，
// 供 PATCH 的乐观锁比对（客户端先 GET 拿 hash，PATCH 时回传）。
func TreeHash(tree map[string]any) (string, error) {
	data, err := json.Marshal(tree)
	if err != nil {
		return "", fmt.Errorf("规范化配置树失败: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// DecodeTree 将键值树还原为配置：先经 ValidateYAML 做 fail-closed schema
// 校验（未知键/类型错误聚合为 ValidationError），再解码到独立的 Config
// 实例。树必须是完整配置树（由 Tree 导出后合并得到），不叠加默认值。
func DecodeTree(tree map[string]any) (*Config, error) {
	data, err := yaml.Marshal(tree)
	if err != nil {
		return nil, fmt.Errorf("序列化合并结果失败: %w", err)
	}
	if err := validateYAML(data); err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解码合并结果失败: %w", err)
	}
	return cfg, nil
}

// ValidateYAML 对 yaml 原文做 fail-closed schema 校验（未知键/类型错误
// 聚合为 ValidationError）。是 validateYAML 的导出入口，供 settings
// PATCH 在写回前复用与启动加载一致的校验规则。
func ValidateYAML(data []byte) error {
	return validateYAML(data)
}

// MergeTree 将 JSON merge patch（RFC 7386 语义）合并到 base 树：
// patch 值为 nil 表示删除该键；两端同为映射时递归合并，其余直接覆盖。
// 返回新树，不修改 base（调用方持有的快照树保持只读）。
func MergeTree(base, patch map[string]any) map[string]any {
	merged := make(map[string]any, len(base))
	for k, v := range base {
		merged[k] = v
	}
	for k, pv := range patch {
		if pv == nil {
			delete(merged, k)
			continue
		}
		patchMap, pvIsMap := pv.(map[string]any)
		baseMap, bvIsMap := merged[k].(map[string]any)
		if pvIsMap && bvIsMap {
			merged[k] = MergeTree(baseMap, patchMap)
			continue
		}
		merged[k] = pv
	}
	return merged
}

// PatchPaths 展开 merge patch 的叶子键路径（如 ["llm.default"]），
// 按字典序返回，供审计记录"变更键清单"（只记路径，不记值）。
// 值为 nil 的删除操作同样记为一个变更路径。
func PatchPaths(patch map[string]any) []string {
	var paths []string
	collectPatchPaths(patch, "", &paths)
	sort.Strings(paths)
	return paths
}

// collectPatchPaths 递归收集 patch 树的叶子路径。
func collectPatchPaths(node map[string]any, prefix string, paths *[]string) {
	for k, v := range node {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if child, ok := v.(map[string]any); ok {
			collectPatchPaths(child, path, paths)
			continue
		}
		*paths = append(*paths, path)
	}
}
