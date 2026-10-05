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
	"math"
	"reflect"
	"strings"
)

const (
	SceneOperationCreateNode   = "CreateNode"
	SceneOperationDeleteNode   = "DeleteNode"
	SceneOperationMoveNode     = "MoveNode"
	SceneOperationSetTransform = "SetTransform"
	SceneOperationSetProperty  = "SetProperty"
	SceneOperationAttachAsset  = "AttachAsset"
	SceneOperationAddRobot     = "AddRobot"
	SceneOperationAddCamera    = "AddCamera"
	SceneOperationAddRegion    = "AddRegion"
)

// SceneOperation 是 Studio 拖拽编辑和 Agent 场景编辑共用的最小操作。
// Value 使用 JSON 是为了让不同节点保留各自属性，但操作名、目标和层级关系必须类型化。
type SceneOperation struct {
	Type      string          `json:"type"`
	Node      *SceneNode      `json:"node,omitempty"`
	NodeID    string          `json:"node_id,omitempty"`
	ParentID  string          `json:"parent_id,omitempty"`
	Transform *Transform      `json:"transform,omitempty"`
	Property  string          `json:"property,omitempty"`
	Value     json.RawMessage `json:"value,omitempty"`
	AssetID   string          `json:"asset_id,omitempty"`
	Asset     *SceneAsset     `json:"asset,omitempty"`
}

// ApplyOperations 在指定 revision 上原子应用一组编辑操作。
// 任一步失败都不会保存半成品，避免 Studio 刷新后得到部分修改的场景。
func (s *SceneAuthoringService) ApplyOperations(
	projectID, documentID string,
	expectedRevision int64,
	operations []SceneOperation,
) (SceneDocument, error) {
	if len(operations) == 0 {
		return SceneDocument{}, errors.New("operations 不能为空")
	}
	document, err := s.Get(projectID, documentID)
	if err != nil {
		return SceneDocument{}, err
	}
	if document.Revision != expectedRevision {
		return SceneDocument{}, fmt.Errorf("%w: 场景草稿 revision 已变化", ErrConflict)
	}
	if document.Status == "published" {
		return SceneDocument{}, fmt.Errorf("%w: 已发布版本不能直接修改", ErrConflict)
	}
	next := document
	next.Assets = append([]SceneAsset(nil), document.Assets...)
	next.Nodes = append([]SceneNode(nil), document.Nodes...)
	next.Regions = append([]SceneNode(nil), document.Regions...)
	for index, operation := range operations {
		if err := applySceneOperation(&next, operation); err != nil {
			return SceneDocument{}, fmt.Errorf("第 %d 个场景操作失败: %w", index+1, err)
		}
	}
	validation := s.Validate(next)
	if !validation.Valid {
		return SceneDocument{}, fmt.Errorf("%w: 场景操作产生无效文档: %s", ErrConflict, formatIssues(validation.Issues))
	}
	return s.Update(projectID, documentID, expectedRevision, next)
}

func applySceneOperation(document *SceneDocument, operation SceneOperation) error {
	switch operation.Type {
	case SceneOperationCreateNode, SceneOperationAddRobot, SceneOperationAddCamera, SceneOperationAddRegion:
		if operation.Node == nil {
			return errors.New("新增节点必须提供 node")
		}
		node := *operation.Node
		switch operation.Type {
		case SceneOperationAddRobot:
			node.Kind = "robot"
		case SceneOperationAddCamera:
			node.Kind = "camera"
		case SceneOperationAddRegion:
			node.Kind = "region"
		}
		if _, _, found := findSceneNode(document, node.ID); found {
			return fmt.Errorf("节点 id 已存在: %s", node.ID)
		}
		if node.ParentID != "" {
			if _, _, found := findSceneNode(document, node.ParentID); !found {
				return fmt.Errorf("父节点不存在: %s", node.ParentID)
			}
		}
		if node.Kind == "region" {
			document.Regions = append(document.Regions, node)
		} else {
			document.Nodes = append(document.Nodes, node)
		}
	case SceneOperationDeleteNode:
		if operation.NodeID == "" {
			return errors.New("DeleteNode 必须提供 node_id")
		}
		if _, _, found := findSceneNode(document, operation.NodeID); !found {
			return ErrNotFound
		}
		remove := map[string]bool{operation.NodeID: true}
		for changed := true; changed; {
			changed = false
			for _, node := range allSceneNodes(*document) {
				if remove[node.ParentID] && !remove[node.ID] {
					remove[node.ID] = true
					changed = true
				}
			}
		}
		document.Nodes = filterSceneNodes(document.Nodes, remove)
		document.Regions = filterSceneNodes(document.Regions, remove)
	case SceneOperationMoveNode:
		node, region, found := findSceneNode(document, operation.NodeID)
		if !found {
			return ErrNotFound
		}
		if operation.ParentID != "" {
			if _, _, parentFound := findSceneNode(document, operation.ParentID); !parentFound {
				return fmt.Errorf("父节点不存在: %s", operation.ParentID)
			}
		}
		node.ParentID = operation.ParentID
		setSceneNode(document, *node, region)
	case SceneOperationSetTransform:
		if operation.Transform == nil {
			return errors.New("SetTransform 必须提供 transform")
		}
		node, region, found := findSceneNode(document, operation.NodeID)
		if !found {
			return ErrNotFound
		}
		node.Transform = *operation.Transform
		setSceneNode(document, *node, region)
	case SceneOperationSetProperty:
		if strings.TrimSpace(operation.Property) == "" || len(operation.Value) == 0 {
			return errors.New("SetProperty 必须提供 property 和 value")
		}
		var value any
		if err := json.Unmarshal(operation.Value, &value); err != nil {
			return fmt.Errorf("property value 不是合法 JSON: %w", err)
		}
		if operation.NodeID == "" {
			if operation.Property != "physics" {
				// node_id 为空时，操作目标是当前 SceneDocument。首版只开放 physics，
				// 避免客户端借文档级 SetProperty 修改 revision、status 或发布版本等系统字段。
				return errors.New("没有 node_id 时 SetProperty 只允许修改 physics")
			}
			physics, ok := value.(map[string]any)
			if !ok {
				return errors.New("physics 必须是对象")
			}
			document.Physics = physics
			return nil
		}
		node, region, found := findSceneNode(document, operation.NodeID)
		if !found {
			return ErrNotFound
		}
		// name、properties 与 extensions 是 SceneNode 自身字段，不应被悄悄
		// 塞进 properties。Studio 和未来 Agent 编辑器都走这条分支，因此这里
		// 必须保持字段含义一致，并对类型错误给出明确结果。
		switch operation.Property {
		case "name":
			name, ok := value.(string)
			if !ok || strings.TrimSpace(name) == "" {
				return errors.New("name 必须是非空字符串")
			}
			node.Name = name
		case "properties":
			properties, ok := value.(map[string]any)
			if !ok {
				return errors.New("properties 必须是对象")
			}
			node.Properties = properties
		case "extensions":
			extensions, ok := value.(map[string]any)
			if !ok {
				return errors.New("extensions 必须是对象")
			}
			node.Extensions = extensions
		default:
			if node.Properties == nil {
				node.Properties = make(map[string]any)
			}
			node.Properties[operation.Property] = value
		}
		setSceneNode(document, *node, region)
	case SceneOperationAttachAsset:
		if operation.AssetID == "" {
			return errors.New("AttachAsset 必须提供 asset_id")
		}
		// AttachAsset 同时承担“把已发布资产目录项加入草稿”和“把资产挂到节点”
		// 两个动作。这样 Studio 与 Agent 都不需要先保存一条不完整引用；
		// 整组操作仍然在同一个 revision 上原子提交。
		if operation.Asset != nil {
			if operation.Asset.ID == "" || operation.Asset.ID != operation.AssetID {
				return errors.New("asset.id 必须与 asset_id 一致")
			}
			foundAsset := false
			for _, existing := range document.Assets {
				if existing.ID != operation.AssetID {
					continue
				}
				foundAsset = true
				if !reflect.DeepEqual(existing, *operation.Asset) {
					return fmt.Errorf("资产 %s 已存在且内容不同", operation.AssetID)
				}
			}
			if !foundAsset {
				document.Assets = append(document.Assets, *operation.Asset)
			}
		}
		node, region, found := findSceneNode(document, operation.NodeID)
		if !found {
			return ErrNotFound
		}
		if !sceneAssetExists(*document, operation.AssetID) {
			return fmt.Errorf("资产不存在: %s", operation.AssetID)
		}
		node.AssetID = operation.AssetID
		setSceneNode(document, *node, region)
	default:
		return fmt.Errorf("不支持的场景操作: %s", operation.Type)
	}
	return nil
}

func validateSceneGraph(document SceneDocument) []ValidationIssue {
	issues := make([]ValidationIssue, 0)
	allowed := map[string]bool{
		"group": true, "object": true, "robot": true,
		"camera": true, "light": true, "region": true,
	}
	parent := make(map[string]string)
	for _, node := range allSceneNodes(document) {
		if !allowed[node.Kind] {
			issues = append(issues, ValidationIssue{
				Level: "error", NodeID: node.ID, Field: "kind",
				Message: unsupportedNodeKindMessage(node.Kind),
			})
		}
		issues = append(issues, validateNodeProperties(node)...)
		parent[node.ID] = node.ParentID
		norm := quaternionNorm(node.Transform.QuaternionXYZW)
		if norm > 0 && math.Abs(norm-1) > 1e-6 {
			issues = append(issues, ValidationIssue{
				Level: "error", NodeID: node.ID, Field: "quaternion_xyzw",
				Message: "四元数必须归一化",
			})
		}
	}
	for id := range parent {
		seen := make(map[string]bool)
		for current := id; current != ""; current = parent[current] {
			if seen[current] {
				issues = append(issues, ValidationIssue{
					Level: "error", NodeID: id, Field: "parent_id",
					Message: "节点层级存在循环引用",
				})
				break
			}
			seen[current] = true
		}
	}
	issues = append(issues, validateRegionCollections(document)...)
	return issues
}

func quaternionNorm(value [4]float64) float64 {
	return math.Sqrt(value[0]*value[0] + value[1]*value[1] + value[2]*value[2] + value[3]*value[3])
}

func allSceneNodes(document SceneDocument) []SceneNode {
	result := make([]SceneNode, 0, len(document.Nodes)+len(document.Regions))
	result = append(result, document.Nodes...)
	result = append(result, document.Regions...)
	return result
}

func findSceneNode(document *SceneDocument, id string) (*SceneNode, bool, bool) {
	for index := range document.Nodes {
		if document.Nodes[index].ID == id {
			node := document.Nodes[index]
			return &node, false, true
		}
	}
	for index := range document.Regions {
		if document.Regions[index].ID == id {
			node := document.Regions[index]
			return &node, true, true
		}
	}
	return nil, false, false
}

func setSceneNode(document *SceneDocument, node SceneNode, region bool) {
	target := &document.Nodes
	if region {
		target = &document.Regions
	}
	for index := range *target {
		if (*target)[index].ID == node.ID {
			(*target)[index] = node
			return
		}
	}
}

func filterSceneNodes(nodes []SceneNode, remove map[string]bool) []SceneNode {
	result := make([]SceneNode, 0, len(nodes))
	for _, node := range nodes {
		if !remove[node.ID] {
			result = append(result, node)
		}
	}
	return result
}

func sceneAssetExists(document SceneDocument, assetID string) bool {
	for _, asset := range document.Assets {
		if asset.ID == assetID {
			return true
		}
	}
	return false
}

func formatIssues(issues []ValidationIssue) string {
	if len(issues) == 0 {
		return ""
	}
	result := make([]string, 0, len(issues))
	for _, issue := range issues {
		result = append(result, issue.Message)
	}
	return strings.Join(result, "; ")
}
