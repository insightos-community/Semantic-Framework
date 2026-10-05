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
	"encoding/base64"
	"fmt"
	"html"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const layoutPreviewGeneratorVersion = "semantic-layout-svg/v1"

// refreshLayoutPreview 根据场景对象的 XY 投影生成确定性 SVG。它不启动 Runtime、
// 不调用 Agent，也不读取 MuJoCo 内部标识，因此保存草稿时可以快速稳定地更新预览。
func refreshLayoutPreview(document *SceneDocument) {
	if document == nil {
		return
	}
	document.Preview = "data:image/svg+xml;base64," +
		base64.StdEncoding.EncodeToString([]byte(renderLayoutPreview(*document)))
}

// persistLayoutPreview 把 SVG 保存到 Project 工作区。相同 Layout/revision
// 总是得到同一路径和同一内容；保存失败会阻止场景文档更新，避免文档引用不存在
// 的预览。真实 Runtime 截图以后可以替换展示图，但不会覆盖该 revision 的 SVG 证据。
func (s *SceneAuthoringService) persistLayoutPreview(document *SceneDocument) error {
	if document == nil || document.ProjectID == "" || document.LayoutID == "" ||
		document.Revision < 1 {
		return fmt.Errorf("Layout 预览缺少 project、layout 或 revision")
	}
	if strings.ContainsAny(document.LayoutID, "/\\") || document.LayoutID == "." ||
		document.LayoutID == ".." {
		return fmt.Errorf("Layout ID 不能用于预览路径")
	}
	root, err := s.store.simulationRoot(document.ProjectID)
	if err != nil {
		return err
	}
	relative := filepath.ToSlash(filepath.Join(
		"layout-previews", document.LayoutID, fmt.Sprintf("r%d.svg", document.Revision),
	))
	target := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("创建 Layout 预览目录失败: %w", err)
	}
	svg := []byte(renderLayoutPreview(*document))
	if err := writeBytesAtomic(target, svg); err != nil {
		return fmt.Errorf("保存 Layout 预览失败: %w", err)
	}
	document.Preview = "data:image/svg+xml;base64," +
		base64.StdEncoding.EncodeToString(svg)
	runtimeProfile := "native-mujoco"
	if document.Authoring != nil && document.Authoring.RuntimeProfileID != "" {
		runtimeProfile = document.Authoring.RuntimeProfileID
	}
	document.PreviewArtifact = &LayoutPreviewArtifact{
		LayoutID: document.LayoutID, Revision: document.Revision,
		RuntimeProfile: runtimeProfile, Generator: layoutPreviewGeneratorVersion,
		Path: relative, MediaType: "image/svg+xml",
	}
	return nil
}

func writeBytesAtomic(path string, content []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".layout-preview-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func renderLayoutPreview(document SceneDocument) string {
	nodes := append([]SceneNode{}, document.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	minX, maxX, minY, maxY := -2.5, 2.5, -2.0, 2.0
	for _, node := range nodes {
		minX = math.Min(minX, node.Transform.Position[0]-0.5)
		maxX = math.Max(maxX, node.Transform.Position[0]+0.5)
		minY = math.Min(minY, node.Transform.Position[1]-0.5)
		maxY = math.Max(maxY, node.Transform.Position[1]+0.5)
	}
	project := func(x, y float64) (float64, float64) {
		return 32 + (x-minX)/(maxX-minX)*576, 62 + (maxY-y)/(maxY-minY)*250
	}
	var content strings.Builder
	content.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" width="640" height="360" viewBox="0 0 640 360">`)
	content.WriteString(`<rect width="640" height="360" rx="18" fill="#122437"/>`)
	content.WriteString(`<path d="M0 60H640M0 120H640M0 180H640M0 240H640M0 300H640M80 0V360M160 0V360M240 0V360M320 0V360M400 0V360M480 0V360M560 0V360" stroke="#29445b" stroke-width="1"/>`)
	content.WriteString(fmt.Sprintf(`<metadata>{"layout_id":%q,"revision":%d,"generator":%q}</metadata>`, document.LayoutID, document.Revision, layoutPreviewGeneratorVersion))
	content.WriteString(fmt.Sprintf(`<text x="24" y="34" fill="#f5f7fb" font-size="20" font-family="sans-serif" font-weight="700">%s</text>`, html.EscapeString(document.LayoutName)))
	for _, node := range nodes {
		if node.Kind == "group" || node.Kind == "light" || node.Kind == "camera" {
			continue
		}
		x, y := project(node.Transform.Position[0], node.Transform.Position[1])
		color, width, height, radius := "#f3f6f8", 34.0, 34.0, 4.0
		category, _ := node.Properties["category"].(string)
		switch {
		case node.Kind == "robot":
			color, width, height, radius = "#cfd9dd", 44, 54, 18
		case category == "pallet":
			color, width, height, radius = "#3852b4", 94, 48, 6
		case category == "target":
			color, width, height, radius = "#58c69b", 86, 42, 20
		case category == "box":
			color, width, height, radius = "#f0f3f5", 34, 34, 3
		}
		content.WriteString(fmt.Sprintf(`<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="%.1f" fill="%s" stroke="#0b1722" stroke-width="2"><title>%s</title></rect>`, x-width/2, y-height/2, width, height, radius, color, html.EscapeString(node.Name)))
	}
	content.WriteString(`</svg>`)
	return content.String()
}
