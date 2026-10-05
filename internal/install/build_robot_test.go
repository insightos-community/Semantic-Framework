package install

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRobotSourceBuildUsesInstalledBundleRelativePaths(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"wheels/sdk.whl", "abilities/vla.zip", "robot/bundle.yaml", "requirements.lock"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0640); err != nil {
			t.Fatal(err)
		}
	}
	c := Component{BundleManifest: "robot/bundle.yaml", Python: &PythonEnvironment{Wheels: []string{"wheels/sdk.whl"}},
		Abilities: []AbilityComponent{{Package: "abilities/vla.zip"}}}
	if err := placeRobotBuildArtifacts(root, &c); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{c.Python.Wheels[0], c.Abilities[0].Package, "requirements.lock"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if c.Python.Wheels[0] != "robot/wheels/sdk.whl" || c.Abilities[0].Package != "robot/abilities/vla.zip" {
		t.Fatalf("路径未归一: %+v", c)
	}
}
