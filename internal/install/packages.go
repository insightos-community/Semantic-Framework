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

// Package install 提供项目包的统一导入入口。上传和目录投递都先保存不可变的
// 内容快照，再调用已有组件导入器；这里不执行包内脚本，也不启动机器人。
package install

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
	"insightos.cn/semantic-framework/internal/skill"
)

const MaxPackageBytes = 100 << 20

type Package struct {
	Scenes         []ScenePackageSummary `json:"scenes,omitempty"`
	Components     []PackageEntry        `json:"components,omitempty"`
	SourceRevision string                `json:"source_revision,omitempty"`
	Kind           string                `json:"kind"`
	Name           string                `json:"name"`
	Version        string                `json:"version,omitempty"`
}

type ScenePackageSummary struct {
	SceneID       string `yaml:"scene_id" json:"scene_id"`
	Name          string `yaml:"name" json:"name"`
	Runtime       string `yaml:"compatible_runtime_profile" json:"runtime_profile"`
	InitialStates int    `json:"initial_states"`
}

func Digest(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// Inspect 使用现有包的根清单识别类型。归档内容仅在内存中读取，路径与体积
// 校验覆盖整个包，避免将符号链接、路径穿越或压缩炸弹留给后续安装器。
func Inspect(data []byte) (Package, error) {
	if len(data) > MaxPackageBytes {
		return Package{}, errors.New("安装包超过 100 MiB")
	}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Package{}, fmt.Errorf("读取 ZIP 安装包: %w", err)
	}
	if len(z.File) > 4096 {
		return Package{}, errors.New("安装包文件过多")
	}
	var total uint64
	seen := map[string]bool{}
	var manifest *zip.File
	for _, f := range z.File {
		name := strings.TrimSuffix(f.Name, "/")
		if name == "" || path.Clean(name) != name || strings.HasPrefix(name, "/") || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\:") || seen[name] {
			return Package{}, fmt.Errorf("安装包路径无效或重复: %s", f.Name)
		}
		seen[name] = true
		if !f.Mode().IsRegular() && !f.FileInfo().IsDir() {
			return Package{}, fmt.Errorf("安装包包含非普通文件: %s", name)
		}
		if f.UncompressedSize64 > MaxPackageBytes || total > MaxPackageBytes-f.UncompressedSize64 {
			return Package{}, errors.New("安装包解压内容超过 100 MiB")
		}
		total += f.UncompressedSize64
		if name == "SKILL.md" || (strings.Count(name, "/") == 1 && strings.HasSuffix(name, "/SKILL.md")) || name == "semantic-scene.yaml" {
			if manifest != nil {
				return Package{}, errors.New("安装包包含多种根清单，请分别导入组件")
			}
			manifest = f
		}
	}
	if manifest == nil {
		return Package{}, errors.New("当前导入入口支持根目录含 SKILL.md 的 Robot Skill 或 semantic-scene.yaml 的 Scene Package")
	}
	if manifest.UncompressedSize64 > 1<<20 {
		return Package{}, errors.New("包清单超过 1 MiB")
	}
	r, err := manifest.Open()
	if err != nil {
		return Package{}, err
	}
	defer r.Close()
	body, err := io.ReadAll(io.LimitReader(r, (1<<20)+1))
	if err != nil {
		return Package{}, err
	}
	if path.Base(manifest.Name) == "SKILL.md" {
		s, err := skill.Parse(body, "")
		if err != nil {
			return Package{}, err
		}
		if s.Category != "robot_skill" {
			return Package{}, errors.New("当前 Skill 导入支持 category: robot_skill")
		}
		v, _ := s.Extensions["version"].(string)
		if v == "" {
			return Package{}, errors.New("Robot Skill 缺少 version")
		}
		return Package{Kind: "robot_skill", Name: s.Name, Version: v}, nil
	}
	var scene struct {
		SchemaVersion int    `yaml:"schema_version"`
		Name          string `yaml:"name"`
	}
	if err := yaml.Unmarshal(body, &scene); err != nil {
		return Package{}, err
	}
	if scene.SchemaVersion != 1 || scene.Name == "" {
		return Package{}, errors.New("场景包缺少 schema_version=1 或 name")
	}
	return Package{Kind: "scene", Name: scene.Name}, nil
}
