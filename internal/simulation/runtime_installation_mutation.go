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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// SetEnabled 只修改管理员清单中的 enabled 字段，不执行命令、不安装依赖，也不
// 允许浏览器传入路径。运行期 Registry 保持启动时快照，因此调用方必须提示重启
// Server 后再启动新启用的 Runtime；停用则可先停止现有受管进程后立即阻止新租约。
func (c *RuntimeInstallationCatalog) SetEnabled(
	installationID string, enabled bool,
) (RuntimeInstallationView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.items[installationID]
	if !ok {
		return RuntimeInstallationView{}, fmt.Errorf(
			"%w: Runtime installation 不存在: %s", ErrNotFound, installationID,
		)
	}
	if current.Enabled == enabled {
		return current.Public(), nil
	}
	if strings.TrimSpace(current.SourceFile) == "" {
		return RuntimeInstallationView{}, errors.New(
			"内置 Runtime 清单是只读模板；请先运行 semantic init 后修改安装目录中的清单",
		)
	}
	data, err := os.ReadFile(current.SourceFile)
	if err != nil {
		return RuntimeInstallationView{}, fmt.Errorf("读取 Runtime 安装清单失败: %w", err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return RuntimeInstallationView{}, fmt.Errorf("解析 Runtime 安装清单失败: %w", err)
	}
	if err := setRuntimeManifestEnabled(&document, enabled); err != nil {
		return RuntimeInstallationView{}, err
	}
	encoded, err := yaml.Marshal(&document)
	if err != nil {
		return RuntimeInstallationView{}, fmt.Errorf("编码 Runtime 安装清单失败: %w", err)
	}
	if err := writeRuntimeManifestAtomic(current.SourceFile, encoded); err != nil {
		return RuntimeInstallationView{}, err
	}
	updated, err := LoadRuntimeInstallationFile(current.SourceFile)
	if err != nil {
		return RuntimeInstallationView{}, err
	}
	c.items[installationID] = updated
	delete(c.observed, installationID)
	return updated.Public(), nil
}

func setRuntimeManifestEnabled(document *yaml.Node, enabled bool) error {
	if document == nil || len(document.Content) != 1 {
		return fmt.Errorf("Runtime 安装清单必须只有一个 YAML 文档")
	}
	mapping := document.Content[0]
	if mapping.Kind != yaml.MappingNode {
		return fmt.Errorf("Runtime 安装清单根节点必须是对象")
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value != "enabled" {
			continue
		}
		value := mapping.Content[index+1]
		value.Kind = yaml.ScalarNode
		value.Tag = "!!bool"
		value.Value = strconv.FormatBool(enabled)
		return nil
	}
	return fmt.Errorf("Runtime 安装清单缺少 enabled 字段")
}

func writeRuntimeManifestAtomic(path string, data []byte) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("Runtime 安装清单必须是普通文件")
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".runtime-installation-*.tmp")
	if err != nil {
		return fmt.Errorf("创建 Runtime 安装清单临时文件失败: %w", err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	mode := info.Mode().Perm()
	if mode == 0 {
		mode = 0o600
	}
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("写入 Runtime 安装清单失败: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("替换 Runtime 安装清单失败: %w", err)
	}
	return nil
}
