package simulation

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSceneContentIsPackageLocalAndSharedAcrossTasks(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "content"), 0700); err != nil {
		t.Fatal(err)
	}
	entry := SceneCatalogEntry{SceneID: "tasks", CompatibleRuntimeProfile: "profile", ContentRoot: "content", Versions: []SceneCatalogVersion{{RuntimeSceneKey: "task:0"}, {RuntimeSceneKey: "task:1"}}}
	if err := resolveSceneContent(root, &entry); err != nil {
		t.Fatal(err)
	}
	catalog := &SceneCatalogService{items: map[string]SceneCatalogEntry{"tasks": entry}}
	for _, key := range []string{"task:0", "task:1"} {
		got, err := catalog.contentRoot("profile", key)
		if err != nil || got != filepath.Join(root, "content") {
			t.Fatalf("%s: %s %v", key, got, err)
		}
	}
	entry.ContentRoot = "../outside"
	if err := resolveSceneContent(root, &entry); err == nil {
		t.Fatal("允许了目录外内容")
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "external")); err != nil {
		t.Fatal(err)
	}
	entry.ContentRoot = "external"
	if err := resolveSceneContent(root, &entry); err == nil {
		t.Fatal("允许了符号链接逃逸")
	}
}
