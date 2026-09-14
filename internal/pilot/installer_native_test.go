// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0

package pilot

import (
	"archive/zip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeOfflineSkillInstallation(t *testing.T) {
	python := os.Getenv("SEMANTIC_TEST_PYTHON")
	if python == "" {
		t.Skip("set SEMANTIC_TEST_PYTHON to a real Python with venv/ensurepip")
	}
	root := filepath.Join(t.TempDir(), "中文 Skill workspace")
	wheels := filepath.Join(root, "wheels")
	definitionDir := filepath.Join(root, "definition")
	for _, directory := range []string{wheels, definitionDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	wheel := func(name string) string {
		path := filepath.Join(wheels, name+"-1.0-py3-none-any.whl")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		writer := zip.NewWriter(file)
		dist := name + "-1.0.dist-info/"
		files := map[string]string{
			name + "/__init__.py": "VALUE = 'offline-verified'\n",
			dist + "METADATA":     "Metadata-Version: 2.1\nName: " + strings.ReplaceAll(name, "_", "-") + "\nVersion: 1.0\n",
			dist + "WHEEL":        "Wheel-Version: 1.0\nGenerator: semantic-native-test\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
		}
		record := dist + "RECORD,,\n"
		for name, body := range files {
			member, err := writer.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := member.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
			record += name + ",,\n"
		}
		member, err := writer.Create(dist + "RECORD")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := member.Write([]byte(record)); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	sdk := wheel("semantic_native_fixture_sdk")
	wheel("semantic_native_fixture_dependency")
	if err := os.WriteFile(filepath.Join(definitionDir, "requirements.lock"), []byte("semantic-native-fixture-dependency==1.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	installer := VenvSkillInstaller{BaseDirectory: filepath.Join(root, "environments"), PythonExecutable: python, SDKSource: sdk, Wheelhouse: wheels}
	definition := SkillDefinition{Name: "native-probe", Version: "1.0", Directory: definitionDir}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	prepared, err := installer.Prepare(ctx, definition)
	if err != nil {
		t.Fatal(err)
	}
	code := "import semantic_native_fixture_sdk as sdk, semantic_native_fixture_dependency as dep; assert sdk.VALUE == dep.VALUE == 'offline-verified'"
	if output, err := exec.CommandContext(ctx, prepared.PythonExecutable, "-I", "-B", "-c", code).CombinedOutput(); err != nil {
		t.Fatalf("installed SDK/dependency import: %v: %s", err, output)
	}
	installer.PythonExecutable = filepath.Join(root, "missing-python")
	repeated, err := installer.Prepare(ctx, definition)
	if err != nil || repeated.PythonExecutable != prepared.PythonExecutable {
		t.Fatalf("ready environment must be reusable: %+v, %v", repeated, err)
	}
}
