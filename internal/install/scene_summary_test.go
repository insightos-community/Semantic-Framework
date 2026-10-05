package install

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScenePackageInspectionListsNativeTasksAndInitialStates(t *testing.T) {
	data, err := packFiles(map[string][]byte{
		"semantic-component.yaml": []byte("schema_version: 1\nkind: scene_catalog\nname: example-scenes\nversion: 1.0.0\nscene_catalog: catalog/catalog.yaml\n"),
		"catalog/catalog.yaml":    []byte("entries:\n  - scene_id: any-task\n    name: upstream task language\n    compatible_runtime_profile: test-runtime\n    versions:\n      - version: 1.0.0\n        variants:\n          - variant_id: init-0\n          - variant_id: init-9\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "scenes.zip")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	pkg, err := InspectArchive(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Scenes) != 1 || pkg.Scenes[0].InitialStates != 2 || pkg.Scenes[0].SceneID != "any-task" {
		t.Fatalf("%+v", pkg.Scenes)
	}
}
