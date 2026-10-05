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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
)

const assetCatalogSchemaVersion = "1"

var (
	assetCatalogVersionPattern = regexp.MustCompile(
		`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`,
	)
	assetCatalogIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	previewColorPattern   = regexp.MustCompile(`^#[0-9A-Fa-f]{6}([0-9A-Fa-f]{2})?$`)
)

// LoadSceneAssetCatalog 从版本管理的 JSON 文件加载资产目录。
//
// 解码使用严格模式：拼错字段、追加第二个 JSON 值或超过大小限制都会让 Server
// 在启动阶段失败，避免配置看似生效、实际却退回内建目录。
func LoadSceneAssetCatalog(filePath string) (SceneAssetCatalog, error) {
	filePath = strings.TrimSpace(filePath)
	if filePath == "" {
		return SceneAssetCatalog{}, errors.New("asset catalog 文件路径不能为空")
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return SceneAssetCatalog{}, fmt.Errorf("读取 asset catalog %q 失败: %w", filePath, err)
	}
	const maxCatalogBytes = 4 << 20
	if len(data) > maxCatalogBytes {
		return SceneAssetCatalog{}, fmt.Errorf(
			"asset catalog %q 超过 %d 字节限制", filePath, maxCatalogBytes,
		)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var catalog SceneAssetCatalog
	if err := decoder.Decode(&catalog); err != nil {
		return SceneAssetCatalog{}, fmt.Errorf("解析 asset catalog %q 失败: %w", filePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return SceneAssetCatalog{}, fmt.Errorf("asset catalog %q 只能包含一个 JSON 对象", filePath)
		}
		return SceneAssetCatalog{}, fmt.Errorf("解析 asset catalog %q 尾部失败: %w", filePath, err)
	}
	if err := catalog.Validate(); err != nil {
		return SceneAssetCatalog{}, fmt.Errorf("asset catalog %q 无效: %w", filePath, err)
	}
	return cloneSceneAssetCatalog(catalog)
}

// Validate 检查目录版本、条目标识、Runtime 资产路径、预览与分发信息。
// Framework 只接受可移植的正斜杠相对路径；资产是否实际存在仍由 Runtime 在构建时检查。
func (c SceneAssetCatalog) Validate() error {
	problems := make([]string, 0)
	if c.SchemaVersion != assetCatalogSchemaVersion {
		problems = append(problems, fmt.Sprintf(
			"schema_version 必须是 %q，当前为 %q", assetCatalogSchemaVersion, c.SchemaVersion,
		))
	}
	if !assetCatalogVersionPattern.MatchString(c.CatalogVersion) {
		problems = append(problems, "catalog_version 必须是 SemVer 版本")
	}
	if len(c.Entries) == 0 {
		problems = append(problems, "entries 不能为空")
	}

	catalogIDs := make(map[string]struct{}, len(c.Entries))
	assetIDs := make(map[string]struct{}, len(c.Entries))
	for index, entry := range c.Entries {
		field := fmt.Sprintf("entries[%d]", index)
		if !assetCatalogIDPattern.MatchString(entry.CatalogID) {
			problems = append(problems, field+".catalog_id 必须是稳定的小写标识")
		} else if _, exists := catalogIDs[entry.CatalogID]; exists {
			problems = append(problems, field+".catalog_id 重复: "+entry.CatalogID)
		}
		catalogIDs[entry.CatalogID] = struct{}{}
		if strings.TrimSpace(entry.Label) == "" {
			problems = append(problems, field+".label 不能为空")
		}
		if entry.NodeKind != "robot" && entry.NodeKind != "object" {
			problems = append(problems, field+".node_kind 必须是 robot 或 object")
		}
		for _, tag := range entry.Tags {
			if !assetCatalogIDPattern.MatchString(tag) {
				problems = append(problems, field+".tags 必须是稳定的小写标识")
			}
		}
		if entry.Asset.ID == "" {
			problems = append(problems, field+".asset.id 不能为空")
		} else if _, exists := assetIDs[entry.Asset.ID]; exists {
			problems = append(problems, field+".asset.id 重复: "+entry.Asset.ID)
		}
		assetIDs[entry.Asset.ID] = struct{}{}
		if entry.Asset.Kind != entry.NodeKind {
			problems = append(problems, field+".asset.kind 必须与 node_kind 一致")
		}
		if err := validateAssetCatalogKey(entry.Asset.AssetKey); err != nil {
			problems = append(problems, field+".asset.asset_key "+err.Error())
		}
		model, modelIsString := entry.Asset.Metadata["model"].(string)
		if !modelIsString || strings.TrimSpace(model) == "" {
			problems = append(problems,
				field+".asset.metadata.model 必须是非空字符串")
		}
		problems = append(problems, validateCatalogPreview(field, entry.Preview)...)
		problems = append(problems, validateCatalogVisual(field, entry.Visual)...)
		if strings.TrimSpace(entry.Source) == "" {
			problems = append(problems, field+".source 不能为空，未知来源请明确写 unknown")
		}
		if strings.TrimSpace(entry.License) == "" {
			problems = append(problems, field+".license 不能为空，未确认请明确写 pending")
		}
		switch entry.DistributionStatus {
		case "redistributable", "internal-only", "pending":
		default:
			problems = append(problems,
				field+".distribution_status 必须是 redistributable、internal-only 或 pending")
		}
		if entry.DistributionStatus == "redistributable" &&
			(entry.License == "pending" || entry.License == "unknown") {
			problems = append(problems,
				field+".license 未确认时不能标记为 redistributable")
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	if _, err := json.Marshal(c); err != nil {
		return fmt.Errorf("目录包含不能序列化的属性: %w", err)
	}
	return nil
}

func validateAssetCatalogKey(assetKey string) error {
	if assetKey == "" {
		return errors.New("不能为空")
	}
	if assetKey != strings.TrimSpace(assetKey) || strings.ContainsAny(assetKey, "\\:") ||
		strings.HasPrefix(assetKey, "/") || path.Clean(assetKey) != assetKey ||
		assetKey == "." || strings.HasPrefix(assetKey, "../") {
		return errors.New("必须是使用正斜杠的规范相对路径")
	}
	return nil
}

func validateCatalogPreview(field string, preview SceneAssetPreview) []string {
	problems := make([]string, 0)
	switch preview.Shape {
	case "box", "sphere", "cylinder", "capsule":
	default:
		problems = append(problems, field+".preview.shape 不受支持")
	}
	for _, value := range preview.Size {
		if value <= 0 {
			problems = append(problems, field+".preview.size 三个分量必须大于零")
			break
		}
	}
	if !previewColorPattern.MatchString(preview.Color) {
		problems = append(problems, field+".preview.color 必须是 #RRGGBB 或 #RRGGBBAA")
	}
	if !preview.Placeholder {
		problems = append(problems,
			field+".preview.placeholder 必须为 true；当前 Studio 只提供几何占位预览")
	}
	return problems
}

func cloneSceneAssetCatalog(catalog SceneAssetCatalog) (SceneAssetCatalog, error) {
	data, err := json.Marshal(catalog)
	if err != nil {
		return SceneAssetCatalog{}, fmt.Errorf("复制 asset catalog 失败: %w", err)
	}
	var cloned SceneAssetCatalog
	if err := json.Unmarshal(data, &cloned); err != nil {
		return SceneAssetCatalog{}, fmt.Errorf("复制 asset catalog 失败: %w", err)
	}
	return cloned, nil
}

// Catalog 返回目录的完整版本信息与条目深拷贝。
func (s *SceneAuthoringService) Catalog() SceneAssetCatalog {
	cloned, err := cloneSceneAssetCatalog(s.catalog)
	if err != nil {
		// catalog 只在构造时写入且已经通过序列化校验；这里失败表示编程错误。
		panic(err)
	}
	return cloned
}

// NewSceneAuthoringServiceWithCatalog 注入产品部署使用的版本化目录。
func NewSceneAuthoringServiceWithCatalog(
	store *FileStore,
	catalog SceneAssetCatalog,
) (*SceneAuthoringService, error) {
	if store == nil {
		return nil, errors.New("SceneAuthoringService store 不能为空")
	}
	if err := catalog.Validate(); err != nil {
		return nil, fmt.Errorf("初始化 SceneAuthoringService 的 asset catalog 失败: %w", err)
	}
	cloned, err := cloneSceneAssetCatalog(catalog)
	if err != nil {
		return nil, err
	}
	return &SceneAuthoringService{store: store, catalog: cloned}, nil
}
