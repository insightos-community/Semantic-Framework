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

package robot

import (
	"archive/zip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

// TestRobotSkillPackageDetailReadsPublishedArchive 验证技能库读取的内容来自
// Server 已发布的包，而不是由前端根据 Registry 摘要补造。这样安装到 Pilot 的
// 版本和用户看到的 SKILL.md、脚本、参考资料始终是同一个不可变版本。
func TestRobotSkillPackageDetailReadsPublishedArchive(t *testing.T) {
	st := openRobotTestStore(t)
	archive := filepath.Join(t.TempDir(), "grasp-object.zip")
	writeRobotSkillArchive(t, archive, map[string]string{
		"grasp-object/SKILL.md": `---
name: grasp-object
description: 抓取指定物体并形成 HeldObjectState
category: robot_skill
when_to_use: Robot Task 需要抓取已经解析的目标物体时
version: 0.1.0
required_actions:
  - type: perception.locate_object
    schema_version: 1
stop_actions:
  - type: gripper.hold_object
    schema_version: 1
---
# 抓取物体

通过动态观测、候选选择和独立验证完成抓取。
`,
		"grasp-object/scripts/skill.py":       "async def run(ctx):\n    pass\n",
		"grasp-object/references/recovery.md": "# 恢复边界\n\n状态未知时不重放动作。\n",
		"grasp-object/requirements.lock":      "semantic-robot-skill-sdk==0.5.0\n",
	})
	if err := st.SaveRobotSkillPackage(store.RobotSkillPackage{
		Name: "grasp-object", Version: "0.1.0", Category: "robot_skill",
		PackagePath: archive, PublishedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	service := NewService(st, nil)
	detail, err := service.GetSkillPackageDetail("grasp-object", "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if detail.WhenToUse == "" || detail.Body == "" || len(detail.Resources) != 3 {
		t.Fatalf("Robot Skill 详情不完整: %#v", detail)
	}
	resource, content, err := service.ReadSkillPackageResource(
		"grasp-object", "0.1.0", "references/recovery.md",
	)
	if err != nil {
		t.Fatal(err)
	}
	if resource.Path != "references/recovery.md" || resource.Kind != "references" || content == "" {
		t.Fatalf("Robot Skill 资源不完整: resource=%#v content=%q", resource, content)
	}
	if _, _, err := service.ReadSkillPackageResource("grasp-object", "0.1.0", "../SKILL.md"); err != ErrSkillResourceInvalid {
		t.Fatalf("越界资源路径必须拒绝，实际 err=%v", err)
	}
}

func TestInstalledSkillPackageDetailUsesExactCurrentCatalog(t *testing.T) {
	st := openRobotTestStore(t)
	service := NewService(st, nil)
	now := time.Now().UTC()
	for _, pilot := range []store.RobotPilot{
		{PilotInstanceID: "pilot-old", RobotID: "robot-contract", LastSeenAt: now.Add(-time.Hour)},
		{PilotInstanceID: "pilot-current", RobotID: "robot-contract", LastSeenAt: now},
	} {
		if err := st.SaveRobotPilot(pilot); err != nil {
			t.Fatal(err)
		}
	}
	for _, version := range []string{"1.0.0", "2.0.0", "9.0.0"} {
		archive := filepath.Join(t.TempDir(), "skill.zip")
		writeRobotSkillArchive(t, archive, map[string]string{
			"exact-skill/SKILL.md": fmt.Sprintf(`---
name: exact-skill
description: 精确已安装契约
version: %s
runtime:
  input_model: scripts.models:ExactInput
  result_model: scripts.models:ExactResult
---
# Version %s

Input must contain target.object_ref.
`, version, version),
		})
		if err := st.SaveRobotSkillPackage(store.RobotSkillPackage{
			Name: "exact-skill", Version: version, PackagePath: archive, PublishedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, actual := range []store.RobotPilotSkill{
		{PilotInstanceID: "pilot-current", Name: "exact-skill", Version: "1.0.0", Enabled: true, Status: "installed"},
		{PilotInstanceID: "pilot-current", Name: "exact-skill", Version: "2.0.0", Enabled: false, Status: "installed"},
		{PilotInstanceID: "pilot-current", Name: "installing-skill", Version: "1.0.0", Enabled: true, Status: "installing"},
		{PilotInstanceID: "pilot-old", Name: "old-pilot-only", Version: "1.0.0", Enabled: true, Status: "installed"},
	} {
		if err := st.SaveRobotPilotSkill(actual); err != nil {
			t.Fatal(err)
		}
	}
	for _, version := range []string{"", "1.0.0"} {
		detail, err := service.GetInstalledSkillPackageDetail("robot-contract", "exact-skill", version)
		if err != nil {
			t.Fatal(err)
		}
		published, err := service.GetSkillPackageDetail("exact-skill", "1.0.0")
		if err != nil || !reflect.DeepEqual(detail, published) {
			t.Fatalf("契约必须复用同一精确发布包，不选择 Registry 9.0.0: %+v err=%v", detail, err)
		}
	}
	for _, test := range []struct{ name, version string }{
		{"missing", ""}, {"exact-skill", "9.0.0"}, {"exact-skill", "2.0.0"},
		{"exact-skill", "latest"}, {"installing-skill", "1.0.0"}, {"old-pilot-only", "1.0.0"},
	} {
		t.Run(test.name+"@"+test.version, func(t *testing.T) {
			if _, err := service.GetInstalledSkillPackageDetail("robot-contract", test.name, test.version); !errors.Is(err, ErrSkillUnavailable) {
				t.Fatalf("未安装启用的精确版本必须拒绝: %v", err)
			}
		})
	}
	if err := st.SaveRobotPilotSkill(store.RobotPilotSkill{PilotInstanceID: "pilot-current",
		Name: "exact-skill", Version: "2.0.0", Enabled: true, Status: "installed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetInstalledSkillPackageDetail("robot-contract", "exact-skill", ""); !errors.Is(err, ErrSkillVersionRequired) {
		t.Fatalf("两个启用版本不能替模型猜测版本: %v", err)
	}
	if detail, err := service.GetInstalledSkillPackageDetail("robot-contract", "exact-skill", "2.0.0"); err != nil || detail.Version != "2.0.0" {
		t.Fatalf("显式选择已安装版本应成功: %+v %v", detail, err)
	}
}

func writeRobotSkillArchive(t *testing.T, target string, files map[string]string) {
	t.Helper()
	output, err := os.Create(target)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(output)
	for name, content := range files {
		entry, createErr := writer.Create(name)
		if createErr != nil {
			t.Fatal(createErr)
		}
		if _, writeErr := entry.Write([]byte(content)); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
}
