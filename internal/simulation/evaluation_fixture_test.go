package simulation

import "testing"

// TestSceneEvaluationFixture 固定 Profile Runtime 与 Framework 共用的评测字段。
// 评测是原生环境证据，不是 Semantic Task 或 Robot Skill 的业务终态。
func TestSceneEvaluationFixture(t *testing.T) {
	var evaluation SceneEvaluation
	readFixture(t, "scene-evaluation-profile.json", &evaluation)
	if evaluation.SceneKey != "libero_spatial:0" ||
		evaluation.InstanceID == "" ||
		evaluation.Generation != 1 ||
		evaluation.RuntimeProfileID != "libero-robosuite-1.4" ||
		evaluation.Language == "" ||
		evaluation.Metrics["suite"] != "libero_spatial" ||
		evaluation.Metrics["task_id"] != float64(0) ||
		evaluation.ObservedAt.IsZero() {
		t.Fatalf("SceneEvaluation 公共样例无效: %+v", evaluation)
	}
}
