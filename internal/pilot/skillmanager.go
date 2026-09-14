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
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// InstalledSkillManager 管理设备侧 packages/environments/active 三个目录。
// staging 校验完成后才原子进入 packages，失败不会破坏已启用版本。
type InstalledSkillManager struct {
	BaseDirectory string
	Catalog       *SkillCatalog
	Installer     SkillInstaller
	InUse         func(name, version string) bool
}

// InstalledSkillSnapshot 是 Pilot 本地实际包目录的只读快照。Catalog 只包含
// active 目录中已经启用的版本，不能代表 packages 中“已安装但尚未启用”的包；
// 若注册时漏掉后者，Server 会在每次重连后重复下载安装同一个包。
type InstalledSkillSnapshot struct {
	Definition SkillDefinition
	Enabled    bool
}

func (m *InstalledSkillManager) ListInstalled() []InstalledSkillSnapshot {
	if m == nil || m.Catalog == nil || strings.TrimSpace(m.BaseDirectory) == "" {
		return nil
	}
	root := filepath.Join(m.BaseDirectory, "packages")
	names, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	result := make([]InstalledSkillSnapshot, 0)
	for _, nameEntry := range names {
		if !nameEntry.IsDir() {
			continue
		}
		versions, readErr := os.ReadDir(filepath.Join(root, nameEntry.Name()))
		if readErr != nil {
			continue
		}
		for _, versionEntry := range versions {
			if !versionEntry.IsDir() {
				continue
			}
			directory := filepath.Join(root, nameEntry.Name(), versionEntry.Name())
			definition, loadErr := LoadSkillDefinition(directory)
			if loadErr != nil {
				// 非活动目录中的残缺包不应阻止 Pilot 上线；当 Server 再次
				// 下发该精确版本时，正常安装流程会返回可见的失败状态。
				continue
			}
			_, enabledErr := m.Catalog.Resolve(definition.Name, definition.Version)
			result = append(result, InstalledSkillSnapshot{
				Definition: definition,
				Enabled:    enabledErr == nil,
			})
		}
	}
	return result
}

func (m *InstalledSkillManager) Install(ctx context.Context, archivePath, wantName, wantVersion string) (SkillDefinition, error) {
	if m.BaseDirectory == "" || m.Catalog == nil || m.Installer == nil {
		return SkillDefinition{}, errors.New("Skill 安装器未完整装配")
	}
	staging, err := os.MkdirTemp(filepath.Join(m.BaseDirectory, "staging"), "install-")
	if err != nil {
		if mkdirErr := os.MkdirAll(filepath.Join(m.BaseDirectory, "staging"), 0o750); mkdirErr != nil {
			return SkillDefinition{}, mkdirErr
		}
		staging, err = os.MkdirTemp(filepath.Join(m.BaseDirectory, "staging"), "install-")
	}
	if err != nil {
		return SkillDefinition{}, err
	}
	defer os.RemoveAll(staging)
	if err := extractPilotSkillArchive(archivePath, staging); err != nil {
		return SkillDefinition{}, err
	}
	manifest, err := findPilotSkillManifest(staging)
	if err != nil {
		return SkillDefinition{}, err
	}
	root := filepath.Dir(manifest)
	definition, err := LoadSkillDefinition(root)
	if err != nil {
		return SkillDefinition{}, err
	}
	if definition.Name != wantName || definition.Version != wantVersion {
		return SkillDefinition{}, errors.New("下载包的 name/version 与安装命令不一致")
	}
	if _, err := m.Installer.Prepare(ctx, definition); err != nil {
		return SkillDefinition{}, err
	}
	target := filepath.Join(m.BaseDirectory, "packages", safePilotSegment(wantName), safePilotSegment(wantVersion))
	if _, err := os.Stat(target); err == nil {
		return LoadSkillDefinition(target)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return SkillDefinition{}, err
	}
	if err := os.Rename(root, target); err != nil {
		return SkillDefinition{}, err
	}
	return LoadSkillDefinition(target)
}

func (m *InstalledSkillManager) Enable(name, version string) (SkillDefinition, error) {
	root := filepath.Join(m.BaseDirectory, "packages", safePilotSegment(name), safePilotSegment(version))
	definition, err := LoadSkillDefinition(root)
	if err != nil {
		return SkillDefinition{}, err
	}
	if err := activateSkill(m.BaseDirectory, name, version, root); err != nil {
		return SkillDefinition{}, err
	}
	m.Catalog.Install(definition)
	return definition, nil
}

func (m *InstalledSkillManager) Disable(name, version string) error {
	if m.InUse != nil && m.InUse(name, version) {
		return ErrRobotBusy
	}
	if err := m.Catalog.Remove(name, version); err != nil && !errors.Is(err, ErrSkillInvalid) {
		return err
	}
	return deactivateSkill(m.BaseDirectory, name, version)
}

func (m *InstalledSkillManager) Uninstall(name, version string) error {
	if m.InUse != nil && m.InUse(name, version) {
		return ErrRobotBusy
	}
	_ = m.Disable(name, version)
	target := filepath.Join(m.BaseDirectory, "packages", safePilotSegment(name), safePilotSegment(version))
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return os.RemoveAll(target)
}

func extractPilotSkillArchive(path, destination string) error {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer reader.Close()
	for _, entry := range reader.File {
		name := filepath.Clean(filepath.FromSlash(entry.Name))
		if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) || entry.Mode()&os.ModeSymlink != 0 {
			return ErrSkillInvalid
		}
		target := filepath.Join(destination, name)
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
			continue
		}
		if !entry.Mode().IsRegular() {
			return ErrSkillInvalid
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		source, err := entry.Open()
		if err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		if err != nil {
			source.Close()
			return err
		}
		_, copyErr := io.Copy(output, source)
		closeErr := output.Close()
		source.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func findPilotSkillManifest(root string) (string, error) {
	var manifest string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return ErrSkillInvalid
		}
		if !entry.IsDir() && entry.Name() == "SKILL.md" {
			if manifest != "" {
				return fmt.Errorf("%w: 包含多个 SKILL.md", ErrSkillInvalid)
			}
			manifest = path
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if manifest == "" {
		return "", fmt.Errorf("%w: 缺少 SKILL.md", ErrSkillInvalid)
	}
	return manifest, nil
}
