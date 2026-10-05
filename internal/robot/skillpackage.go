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

package robot

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"mime"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"insightos.cn/semantic-framework/internal/skill"
	"insightos.cn/semantic-framework/internal/store"
)

var (
	ErrSkillResourceInvalid  = errors.New("robot skill resource path is invalid")
	ErrSkillResourceTooLarge = errors.New("robot skill resource is too large")
	ErrSkillResourceNotText  = errors.New("robot skill resource is not UTF-8 text")
	ErrSkillVersionRequired  = errors.New("Robot Skill 有多个已安装启用版本，请指定精确 skill_version")
)

// SkillResource 是 Robot Skill 发布包中的可查看资源。
// 路径始终相对于 SKILL.md 所在目录，浏览器不会接触 Server 的归档文件路径。
type SkillResource struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
}

// SkillPackageDetail 在发布记录之外补充 SKILL.md 正文与资源目录。
// 发布包仍是唯一来源，避免为了页面展示再维护一份容易漂移的技能说明。
type SkillPackageDetail struct {
	store.RobotSkillPackage
	WhenToUse  string          `json:"when_to_use,omitempty"`
	Body       string          `json:"body"`
	Extensions map[string]any  `json:"extensions,omitempty"`
	Resources  []SkillResource `json:"resources"`
}

func (s *Service) GetSkillPackageDetail(name, version string) (SkillPackageDetail, error) {
	pkg, err := s.st.GetRobotSkillPackage(name, version)
	if err != nil {
		return SkillPackageDetail{}, err
	}
	return LoadSkillPackageDetail(pkg)
}

// GetInstalledSkillPackageDetail resolves only the current Pilot's installed,
// enabled catalog. An omitted version is safe only when that catalog contains
// exactly one version; a Registry publication never implies Robot availability.
// Like TaskExecution, documentation comes from the exact immutable package.
func (s *Service) GetInstalledSkillPackageDetail(robotID, name, version string) (SkillPackageDetail, error) {
	name, version = strings.TrimSpace(name), strings.TrimSpace(version)
	_, installed, err := s.GetRobot(robotID)
	if err != nil {
		return SkillPackageDetail{}, err
	}
	versions := make(map[string]struct{})
	for _, actual := range installed {
		if actual.Name == name && actual.Enabled && strings.EqualFold(actual.Status, "installed") &&
			actual.Version != "" && (version == "" || actual.Version == version) {
			versions[actual.Version] = struct{}{}
		}
	}
	if len(versions) == 0 {
		return SkillPackageDetail{}, fmt.Errorf("%s@%s: %w", name, version, ErrSkillUnavailable)
	}
	if len(versions) != 1 {
		return SkillPackageDetail{}, fmt.Errorf("%s: %w", name, ErrSkillVersionRequired)
	}
	for resolved := range versions {
		version = resolved
	}
	return s.GetSkillPackageDetail(name, version)
}

// LoadSkillPackageDetail 从 Registry 已持久化的发布记录读取 SKILL.md 语义。
// HTTP 详情页和 Robot Task Planning 共用这一入口，避免页面能看到的 Skill
// 与模型用于生成 SubTask 的 Skill 目录来自两套解析逻辑。
func LoadSkillPackageDetail(pkg store.RobotSkillPackage) (SkillPackageDetail, error) {
	reader, err := zip.OpenReader(pkg.PackagePath)
	if err != nil {
		return SkillPackageDetail{}, fmt.Errorf("打开 Robot Skill 发布包: %w", err)
	}
	defer reader.Close()

	manifest, err := robotSkillManifestEntry(reader.File)
	if err != nil {
		return SkillPackageDetail{}, err
	}
	content, err := readSkillZipText(manifest)
	if err != nil {
		return SkillPackageDetail{}, err
	}
	parsed, err := skill.Parse([]byte(content), ".")
	if err != nil {
		return SkillPackageDetail{}, fmt.Errorf("解析发布包 SKILL.md: %w", err)
	}

	return SkillPackageDetail{
		RobotSkillPackage: pkg,
		WhenToUse:         parsed.WhenToUse,
		Body:              parsed.Body,
		Extensions:        parsed.Extensions,
		Resources:         listSkillResources(reader.File, path.Dir(manifest.Name)),
	}, nil
}

func (s *Service) ReadSkillPackageResource(name, version, resourcePath string) (SkillResource, string, error) {
	pkg, err := s.st.GetRobotSkillPackage(name, version)
	if err != nil {
		return SkillResource{}, "", err
	}
	clean := path.Clean(strings.TrimSpace(resourcePath))
	if clean == "." || clean == "" || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return SkillResource{}, "", ErrSkillResourceInvalid
	}

	reader, err := zip.OpenReader(pkg.PackagePath)
	if err != nil {
		return SkillResource{}, "", fmt.Errorf("打开 Robot Skill 发布包: %w", err)
	}
	defer reader.Close()
	manifest, err := robotSkillManifestEntry(reader.File)
	if err != nil {
		return SkillResource{}, "", err
	}
	root := path.Dir(manifest.Name)
	target := clean
	if root != "." {
		target = path.Join(root, clean)
	}
	for _, file := range reader.File {
		if file.Name == target && !file.FileInfo().IsDir() {
			content, readErr := readSkillZipText(file)
			if readErr != nil {
				return SkillResource{}, "", readErr
			}
			mediaType := mime.TypeByExtension(path.Ext(clean))
			if mediaType == "" {
				mediaType = "text/plain"
			}
			return SkillResource{
				Path: clean, Kind: robotSkillResourceKind(clean), MediaType: mediaType,
				Size: int64(file.UncompressedSize64),
			}, content, nil
		}
	}
	return SkillResource{}, "", store.ErrNotFound
}

func robotSkillManifestEntry(files []*zip.File) (*zip.File, error) {
	var found *zip.File
	for _, file := range files {
		if file.FileInfo().IsDir() || path.Base(file.Name) != "SKILL.md" {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("Robot Skill 发布包包含多个 SKILL.md")
		}
		found = file
	}
	if found == nil {
		return nil, fmt.Errorf("Robot Skill 发布包缺少 SKILL.md")
	}
	return found, nil
}

func readSkillZipText(file *zip.File) (string, error) {
	if file.UncompressedSize64 > uint64(skill.MaxResourcePreviewBytes) {
		return "", ErrSkillResourceTooLarge
	}
	stream, err := file.Open()
	if err != nil {
		return "", err
	}
	defer stream.Close()
	data, err := io.ReadAll(io.LimitReader(stream, skill.MaxResourcePreviewBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > skill.MaxResourcePreviewBytes {
		return "", ErrSkillResourceTooLarge
	}
	if !utf8.Valid(data) {
		return "", ErrSkillResourceNotText
	}
	return string(data), nil
}

func listSkillResources(files []*zip.File, root string) []SkillResource {
	resources := make([]SkillResource, 0)
	prefix := ""
	if root != "." {
		prefix = root + "/"
	}
	for _, file := range files {
		if file.FileInfo().IsDir() || file.Mode()&0o120000 != 0 {
			continue
		}
		rel := strings.TrimPrefix(file.Name, prefix)
		if rel == file.Name && prefix != "" || rel == "SKILL.md" || rel == "" {
			continue
		}
		kind := robotSkillResourceKind(rel)
		if kind == "" {
			continue
		}
		mediaType := mime.TypeByExtension(path.Ext(rel))
		if mediaType == "" {
			mediaType = "text/plain"
		}
		resources = append(resources, SkillResource{
			Path: rel, Kind: kind, MediaType: mediaType, Size: int64(file.UncompressedSize64),
		})
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].Path < resources[j].Path })
	return resources
}

func robotSkillResourceKind(name string) string {
	first := strings.SplitN(name, "/", 2)[0]
	switch first {
	case "scripts", "references", "assets", "tests":
		return first
	case "requirements.lock":
		return "runtime"
	default:
		return ""
	}
}
