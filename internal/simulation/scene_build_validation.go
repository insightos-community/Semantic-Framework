package simulation

// ValidateForBuild 在可持续保存的草稿检查之上，补充 Runtime 构建前置条件。
//
// Scene Editor 允许先保存只有 Group 或尚未挂资产的 Robot，便于多步编辑；
// 用户点击“校验/构建”时，Robot 与 Object 必须已经通过 AttachAsset 使用发布目录。
func (s *SceneAuthoringService) ValidateForBuild(document SceneDocument) ValidationResult {
	result := s.Validate(document)
	for _, node := range allSceneNodes(document) {
		if (node.Kind == "robot" || node.Kind == "object") && node.AssetID == "" {
			result.Issues = append(result.Issues, ValidationIssue{
				Level: "error", NodeID: node.ID, Field: "asset_id",
				Message: "Robot 和 Object 必须通过 AttachAsset 使用发布资产",
			})
		}
	}
	result.Valid = len(result.Issues) == 0
	return result
}
