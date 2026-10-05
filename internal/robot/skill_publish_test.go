package robot

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishSkillArchiveVersionImmutable(t *testing.T) {
	st := openRobotTestStore(t)
	service := NewService(st, nil)
	path := filepath.Join(t.TempDir(), "package.zip")
	files := map[string]string{"SKILL.md": `---
name: publish-test
description: 发布不可变测试
category: robot_skill
version: 0.1.0
required_actions:
  - type: robot.state
    schema_version: 1
stop_actions:
  - type: robot.hold
    schema_version: 1
---
# 发布测试
`, "scripts/skill.py": "# original"}
	writeRobotSkillArchive(t, path, files)
	archive, _ := os.ReadFile(path)
	first, err := service.PublishSkillArchive(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	writeRobotSkillArchive(t, path, files)
	repacked, _ := os.ReadFile(path)
	second, err := service.PublishSkillArchive(bytes.NewReader(repacked))
	if err != nil || second.PublishedAt != first.PublishedAt {
		t.Fatalf("重复发布失败: %+v %v", second, err)
	}
	files["scripts/skill.py"] = "# changed"
	writeRobotSkillArchive(t, path, files)
	changed, _ := os.ReadFile(path)
	if _, err := service.PublishSkillArchive(bytes.NewReader(changed)); err == nil {
		t.Fatal("同版本不同内容应拒绝")
	}
	saved, _ := os.ReadFile(first.PackagePath)
	if !bytes.Equal(saved, archive) {
		t.Fatal("已发布包被覆盖")
	}
}
