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
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

const (
	scenePackageSchemaVersion = 1
	maxScenePackageFiles      = 256
	maxScenePackageBytes      = 64 << 20
)

type scenePackageManifest struct {
	SchemaVersion            int      `yaml:"schema_version"`
	SceneID                  string   `yaml:"scene_id"`
	Name                     string   `yaml:"name"`
	Description              string   `yaml:"description,omitempty"`
	Category                 string   `yaml:"category,omitempty"`
	Tags                     []string `yaml:"tags,omitempty"`
	CompatibleRuntimeProfile string   `yaml:"compatible_runtime_profile"`
	Layouts                  []string `yaml:"layouts"`
	ExportedAt               string   `yaml:"exported_at"`
}

type scenePackageMetadata struct {
	SceneID     string   `json:"scene_id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Category    string   `json:"category,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Preview     string   `json:"preview,omitempty"`
}

// ExportScenePackage 导出同一逻辑场景每个 Layout 的最新已发布版本。包中只保存
// 可移植的场景 JSON 和资产引用，不包含 Runtime 本机路径、环境变量或密钥。
func (s *SceneAuthoringService) ExportScenePackage(
	projectID, sceneID string,
) ([]byte, string, error) {
	documents, err := s.ListLayouts(projectID, sceneID)
	if err != nil {
		return nil, "", err
	}
	latest := latestPublishedLayouts(documents)
	if len(latest) == 0 {
		return nil, "", fmt.Errorf("%w: 场景没有可导出的已发布 Layout", ErrConflict)
	}
	bundles, err := s.store.ListRuntimeBundles(projectID)
	if err != nil {
		return nil, "", err
	}
	bundleByDocument := make(map[string]RuntimeBundle)
	for _, bundle := range bundles {
		key := fmt.Sprintf("%s:%d", bundle.DocumentID, bundle.Revision)
		bundleByDocument[key] = bundle
	}
	profileID := ""
	for _, document := range latest {
		key := fmt.Sprintf("%s:%d", document.ID, document.Revision-1)
		bundle, ok := bundleByDocument[key]
		if !ok {
			return nil, "", fmt.Errorf("%w: Layout %s 缺少已构建 RuntimeBundle",
				ErrConflict, document.LayoutName)
		}
		if profileID == "" {
			profileID = bundle.RuntimeProfileID
		} else if profileID != bundle.RuntimeProfileID {
			return nil, "", fmt.Errorf("%w: 同一 Scene Package 不能混用 Runtime Profile", ErrConflict)
		}
	}
	first := latest[0]
	manifest := scenePackageManifest{
		SchemaVersion: scenePackageSchemaVersion, SceneID: first.SceneID,
		Name: first.Name, Description: first.Description, Category: first.Category,
		Tags: append([]string{}, first.Tags...), CompatibleRuntimeProfile: profileID,
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
	}
	for _, document := range latest {
		manifest.Layouts = append(manifest.Layouts, document.LayoutID)
	}
	metadata := scenePackageMetadata{
		SceneID: first.SceneID, Name: first.Name, Description: first.Description,
		Category: first.Category, Tags: append([]string{}, first.Tags...), Preview: first.Preview,
	}
	assets := collectPackageAssets(latest)
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	manifestData, _ := yaml.Marshal(manifest)
	metadataData, _ := json.MarshalIndent(metadata, "", "  ")
	assetData, _ := json.MarshalIndent(assets, "", "  ")
	if err := writeScenePackageFile(writer, "semantic-scene.yaml", manifestData); err != nil {
		return nil, "", err
	}
	if err := writeScenePackageFile(writer, "scene.json", metadataData); err != nil {
		return nil, "", err
	}
	for _, document := range latest {
		data, marshalErr := json.MarshalIndent(document, "", "  ")
		if marshalErr != nil {
			return nil, "", marshalErr
		}
		if err := writeScenePackageFile(writer,
			"layouts/"+document.LayoutID+".json", data); err != nil {
			return nil, "", err
		}
	}
	if err := writeScenePackageFile(writer, "asset-references.json", assetData); err != nil {
		return nil, "", err
	}
	if err := writeScenePackageFile(writer, "previews/README.txt",
		[]byte("Preview files are referenced by scene.json and are not copied implicitly.\n")); err != nil {
		return nil, "", err
	}
	if err := writeScenePackageFile(writer, "licenses/README.txt",
		[]byte("Asset licenses remain attached to the referenced versioned asset catalog.\n")); err != nil {
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return output.Bytes(), safePackageFilename(first.Name) + ".semantic-scene.zip", nil
}

// ImportScenePackage 把包内 Layout 作为当前 Project 的新草稿导入。任何 Python、
// 绝对路径、路径穿越、未知文件或不兼容 Runtime Profile 都会整体拒绝。
func (s *SceneAuthoringService) ImportScenePackage(
	projectID, expectedProfileID string, data []byte,
) ([]SceneDocument, error) {
	if len(data) == 0 || len(data) > maxScenePackageBytes {
		return nil, errors.New("Scene Package 为空或超过 64 MiB")
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("Scene Package 不是合法 ZIP: %w", err)
	}
	if len(reader.File) == 0 || len(reader.File) > maxScenePackageFiles {
		return nil, errors.New("Scene Package 文件数量非法")
	}
	files := make(map[string][]byte)
	var total uint64
	for _, file := range reader.File {
		name := file.Name
		if !validScenePackagePath(name) {
			return nil, fmt.Errorf("Scene Package 包含非法路径: %s", name)
		}
		if file.FileInfo().IsDir() {
			continue
		}
		if !allowedScenePackageFile(name) {
			return nil, fmt.Errorf("Scene Package 包含不支持的文件: %s", name)
		}
		total += file.UncompressedSize64
		if total > maxScenePackageBytes {
			return nil, errors.New("Scene Package 解压后超过 64 MiB")
		}
		stream, openErr := file.Open()
		if openErr != nil {
			return nil, openErr
		}
		payload, readErr := io.ReadAll(io.LimitReader(stream, maxScenePackageBytes+1))
		_ = stream.Close()
		if readErr != nil || len(payload) > maxScenePackageBytes {
			return nil, errors.New("读取 Scene Package 文件失败")
		}
		files[name] = payload
	}
	var manifest scenePackageManifest
	decoder := yaml.NewDecoder(bytes.NewReader(files["semantic-scene.yaml"]))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("解析 semantic-scene.yaml 失败: %w", err)
	}
	if manifest.SchemaVersion != scenePackageSchemaVersion || manifest.Name == "" ||
		len(manifest.Layouts) == 0 || manifest.CompatibleRuntimeProfile == "" {
		return nil, errors.New("semantic-scene.yaml 缺少必要字段")
	}
	if expectedProfileID == "" || manifest.CompatibleRuntimeProfile != expectedProfileID {
		return nil, fmt.Errorf("%w: Scene Package 需要 %s，Project Runtime 是 %s",
			ErrConflict, manifest.CompatibleRuntimeProfile, expectedProfileID)
	}
	newSceneID := "scene-" + uuid.NewString()
	now := time.Now().UTC()
	result := make([]SceneDocument, 0, len(manifest.Layouts))
	layoutIDs := make(map[string]bool)
	for _, sourceLayoutID := range manifest.Layouts {
		if layoutIDs[sourceLayoutID] {
			return nil, fmt.Errorf("Scene Package layout_id 重复: %s", sourceLayoutID)
		}
		layoutIDs[sourceLayoutID] = true
		payload, ok := files["layouts/"+sourceLayoutID+".json"]
		if !ok {
			return nil, fmt.Errorf("Scene Package 缺少 Layout: %s", sourceLayoutID)
		}
		var source SceneDocument
		jsonDecoder := json.NewDecoder(bytes.NewReader(payload))
		jsonDecoder.DisallowUnknownFields()
		if err := jsonDecoder.Decode(&source); err != nil {
			return nil, fmt.Errorf("解析 Layout %s 失败: %w", sourceLayoutID, err)
		}
		normalizeSceneIdentity(&source)
		source.ID = "scene-doc-" + uuid.NewString()
		source.ProjectID = projectID
		source.SceneID = newSceneID
		source.LayoutID = "layout-" + uuid.NewString()
		source.Name = manifest.Name
		source.Description = manifest.Description
		source.Category = manifest.Category
		source.Tags = append([]string{}, manifest.Tags...)
		source.Status = "draft"
		source.Version = 0
		source.Revision = 1
		source.CreatedAt, source.UpdatedAt = now, now
		validation := s.Validate(source)
		if !validation.Valid {
			return nil, fmt.Errorf("%w: 导入 Layout %s 无效: %s",
				ErrConflict, source.LayoutName, formatIssues(validation.Issues))
		}
		result = append(result, source)
	}
	for _, document := range result {
		if err := s.save(&document); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func latestPublishedLayouts(documents []SceneDocument) []SceneDocument {
	latest := map[string]SceneDocument{}
	for _, document := range documents {
		if document.Status != "published" {
			continue
		}
		current, ok := latest[document.LayoutID]
		if !ok || document.Version > current.Version ||
			(document.Version == current.Version && document.UpdatedAt.After(current.UpdatedAt)) {
			latest[document.LayoutID] = document
		}
	}
	result := make([]SceneDocument, 0, len(latest))
	for _, document := range latest {
		result = append(result, document)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].LayoutName < result[j].LayoutName })
	return result
}

func collectPackageAssets(documents []SceneDocument) []SceneAsset {
	assets := map[string]SceneAsset{}
	for _, document := range documents {
		for _, asset := range document.Assets {
			assets[asset.ID] = asset
		}
	}
	result := make([]SceneAsset, 0, len(assets))
	for _, asset := range assets {
		result = append(result, asset)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func writeScenePackageFile(writer *zip.Writer, name string, data []byte) error {
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	header.SetMode(0o640)
	entry, err := writer.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = entry.Write(data)
	return err
}

func validScenePackagePath(name string) bool {
	if name == "" || strings.Contains(name, "\\") || strings.Contains(name, ":") ||
		strings.HasPrefix(name, "/") || path.Clean(name) != name {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." || part == "" {
			return false
		}
	}
	return true
}

func allowedScenePackageFile(name string) bool {
	switch name {
	case "semantic-scene.yaml", "scene.json", "asset-references.json",
		"previews/README.txt", "licenses/README.txt":
		return true
	}
	return strings.HasPrefix(name, "layouts/") && strings.HasSuffix(name, ".json")
}

func safePackageFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "scene"
	}
	var result strings.Builder
	for _, char := range name {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' {
			result.WriteRune(char)
		} else if result.Len() > 0 {
			result.WriteByte('-')
		}
	}
	if result.Len() == 0 {
		return "scene"
	}
	return strings.Trim(result.String(), "-")
}
