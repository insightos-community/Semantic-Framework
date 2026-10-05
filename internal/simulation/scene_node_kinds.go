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

func unsupportedNodeKindMessage(kind string) string {
	if map[string]bool{
		"asset": true, "joint": true, "actuator": true,
		"sensor": true, "material": true, "collision": true,
	}[kind] {
		return "当前 native 场景不支持独立 " + kind +
			" 节点；请通过资产、Robot、Camera 或对象属性配置"
	}
	return "不支持的节点类型: " + kind
}

func validateRegionCollections(document SceneDocument) []ValidationIssue {
	issues := make([]ValidationIssue, 0)
	for _, node := range document.Nodes {
		if node.Kind == "region" {
			issues = append(issues, ValidationIssue{
				Level: "error", NodeID: node.ID, Field: "kind",
				Message: "Region 必须保存在 regions 集合中",
			})
		}
	}
	for _, node := range document.Regions {
		if node.Kind != "region" {
			issues = append(issues, ValidationIssue{
				Level: "error", NodeID: node.ID, Field: "kind",
				Message: "regions 集合只能包含 Region",
			})
		}
	}
	return issues
}
