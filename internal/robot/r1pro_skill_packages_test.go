package robot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestR1ProExistingSkillsPublishWithCancelStrategy(t *testing.T) {
	root := os.Getenv("R1PRO_SKILL_ARTIFACTS")
	if root == "" {
		t.Skip("set R1PRO_SKILL_ARTIFACTS to validate experimental packages")
	}
	paths, err := filepath.Glob(filepath.Join(root, "r1pro-*-0.2.0-alpha.1.zip"))
	if err != nil || len(paths) != 9 {
		t.Fatalf("expected 9 skills: %v %v", paths, err)
	}
	service := NewService(openRobotTestStore(t), nil)
	for _, path := range paths {
		stream, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		pkg, err := service.PublishSkillArchive(stream)
		stream.Close()
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(pkg.RequiredActions) == 0 || len(pkg.StopActions) != 0 {
			t.Fatalf("wrong executable catalog: %+v", pkg)
		}
	}
}
