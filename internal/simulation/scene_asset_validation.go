package simulation

import "strings"

func validateAssetKinds(document SceneDocument) []ValidationIssue {
	issues := make([]ValidationIssue, 0)
	assets := make(map[string]SceneAsset, len(document.Assets))
	for _, asset := range document.Assets {
		assets[asset.ID] = asset
		if asset.Kind != "object" && asset.Kind != "robot" {
			issues = append(issues, ValidationIssue{
				Level: "error", Field: "assets." + asset.ID + ".kind",
				Message: "资产类型必须是 object 或 robot",
			})
		}
		for key := range asset.Metadata {
			if !map[string]bool{"model": true, "category": true}[key] {
				issues = append(issues, ValidationIssue{
					Level: "error", Field: "assets." + asset.ID + ".metadata." + key,
					Message: "不支持的资产元数据 " + key,
				})
			}
		}
		if model, ok := asset.Metadata["model"]; ok {
			if value, valid := model.(string); !valid || strings.TrimSpace(value) == "" {
				issues = append(issues, ValidationIssue{
					Level: "error", Field: "assets." + asset.ID + ".metadata.model",
					Message: "资产 model 必须是非空字符串",
				})
			}
		}
	}
	for _, node := range allSceneNodes(document) {
		if node.AssetID == "" {
			continue
		}
		asset, exists := assets[node.AssetID]
		if !exists {
			continue
		}
		switch node.Kind {
		case "robot":
			if asset.Kind != "robot" {
				issues = append(issues, ValidationIssue{
					Level: "error", NodeID: node.ID, Field: "asset_id",
					Message: "Robot 节点只能引用 robot 资产",
				})
			}
		case "object":
			if asset.Kind != "object" {
				issues = append(issues, ValidationIssue{
					Level: "error", NodeID: node.ID, Field: "asset_id",
					Message: "Object 节点只能引用 object 资产",
				})
			}
		default:
			issues = append(issues, ValidationIssue{
				Level: "error", NodeID: node.ID, Field: "asset_id",
				Message: node.Kind + " 节点不能直接引用资产",
			})
		}
	}
	return issues
}
