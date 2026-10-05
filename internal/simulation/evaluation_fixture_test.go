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
