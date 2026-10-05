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
