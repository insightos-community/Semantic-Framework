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

package skill

import (
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// MaxResourcePreviewBytes 限制技能资源在线预览大小。资源文件仍保留在
	// Skill 目录中，超限只是不通过管理界面一次性读入内存和浏览器。
	MaxResourcePreviewBytes int64 = 1024 * 1024
)

var (
	// ErrInvalidResourcePath 表示资源路径不属于标准 Skill 资源目录。
	ErrInvalidResourcePath = errors.New("技能资源路径非法")
	// ErrResourceTooLarge 表示文件存在，但超过在线预览上限。
	ErrResourceTooLarge = errors.New("技能资源超过预览上限")
	// ErrResourceNotText 表示文件不是合法 UTF-8 文本，不能在代码预览区展示。
	ErrResourceNotText = errors.New("技能资源不是文本文件")
)

// ResourceInfo 是 scripts/references/assets 下单个资源的安全元数据。
type ResourceInfo struct {
	// Path 是相对 Skill 根目录的稳定斜杠路径。
	Path string `json:"path"`
	// Kind 是资源顶级目录：scripts、references 或 assets。
	Kind string `json:"kind"`
	// MediaType 是按扩展名推断的媒体类型，仅用于前端展示。
	MediaType string `json:"media_type"`
	// Size 是文件字节数。
	Size int64 `json:"size"`
}

var standardResourceRoots = map[string]struct{}{
	"scripts": {}, "references": {}, "assets": {},
}

// ListResources 列出标准 Skill 资源目录中的普通文件。符号链接和隐藏目录
// 不进入管理视图，避免通过 Skill 包中的链接意外暴露包外文件。
func ListResources(sk Skill) ([]ResourceInfo, error) {
	resources := make([]ResourceInfo, 0)
	for _, kind := range []string{"scripts", "references", "assets"} {
		root := filepath.Join(sk.Dir, kind)
		if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("读取技能资源目录 %s 失败: %w", kind, err)
		}
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path != root && entry.IsDir() && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(sk.Dir, path)
			if err != nil {
				return err
			}
			resources = append(resources, ResourceInfo{
				Path: filepath.ToSlash(rel), Kind: kind,
				MediaType: resourceMediaType(path), Size: info.Size(),
			})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("扫描技能资源目录 %s 失败: %w", kind, err)
		}
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].Path < resources[j].Path })
	return resources, nil
}

// ReadResource 按相对路径读取一个可预览的文本资源。路径既做词法校验，
// 也解析符号链接后的真实路径，确保请求不能越出当前 Skill 根目录。
func ReadResource(sk Skill, requested string) (ResourceInfo, string, error) {
	rel, kind, err := normalizeResourcePath(requested)
	if err != nil {
		return ResourceInfo{}, "", err
	}
	realRoot, err := filepath.EvalSymlinks(sk.Dir)
	if err != nil {
		return ResourceInfo{}, "", fmt.Errorf("解析技能根目录失败: %w", err)
	}
	target := filepath.Join(sk.Dir, filepath.FromSlash(rel))
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return ResourceInfo{}, "", err
	}
	inside, err := filepath.Rel(realRoot, realTarget)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return ResourceInfo{}, "", ErrInvalidResourcePath
	}
	info, err := os.Stat(realTarget)
	if err != nil {
		return ResourceInfo{}, "", err
	}
	if !info.Mode().IsRegular() {
		return ResourceInfo{}, "", ErrInvalidResourcePath
	}
	resource := ResourceInfo{Path: rel, Kind: kind, MediaType: resourceMediaType(realTarget), Size: info.Size()}
	if info.Size() > MaxResourcePreviewBytes {
		return resource, "", ErrResourceTooLarge
	}
	data, err := os.ReadFile(realTarget)
	if err != nil {
		return resource, "", err
	}
	if !utf8.Valid(data) {
		return resource, "", ErrResourceNotText
	}
	return resource, string(data), nil
}

func normalizeResourcePath(requested string) (string, string, error) {
	requested = strings.ReplaceAll(strings.TrimSpace(requested), "\\", "/")
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(requested)))
	if clean == "." || strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", ErrInvalidResourcePath
	}
	kind := strings.SplitN(clean, "/", 2)[0]
	if _, ok := standardResourceRoots[kind]; !ok || clean == kind {
		return "", "", ErrInvalidResourcePath
	}
	return clean, kind, nil
}

func resourceMediaType(path string) string {
	if mediaType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); mediaType != "" {
		return mediaType
	}
	return "text/plain; charset=utf-8"
}
