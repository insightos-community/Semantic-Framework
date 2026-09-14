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

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRuntimePackStrictIntegrityAndContentRequirements(t *testing.T) {
	root := t.TempDir()
	files := map[string][]byte{
		"locks/requirements.lock":   []byte("fastapi==0.115.12\n"),
		"wheels/runtime.whl":        []byte("runtime-wheel"),
		"wheelhouse/fastapi.whl":    []byte("dependency-wheel"),
		"catalog/catalog.yaml":      []byte("schema_version: 1\ncatalog_version: 0.4.0\nentries: []\n"),
		"smoke/request.json":        []byte(`{"request_id":"install-smoke","layout":"layout001","headless":true}`),
		"licenses/LICENSE":          []byte("license"),
		"verification/version.json": []byte(`{"source_commit":"0123456789abcdef"}`),
	}
	for path, data := range files {
		absolute := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	file := func(path string) RuntimePackFile {
		hash := sha256.Sum256(files[path])
		return RuntimePackFile{Path: path, SHA256: hex.EncodeToString(hash[:])}
	}
	manifest := RuntimePackManifest{
		SchemaVersion: 1, PackID: "native-mujoco", PackVersion: "0.4.0",
		Profile: RuntimeProfile{RuntimeProfileID: "native-mujoco", Name: "Native MuJoCo",
			Engine: "mujoco", Loader: "native", APIVersion: "v1"},
		Runner: "native-mujoco", PythonVersion: "3.10.16", Endpoint: "http://127.0.0.1:8090",
		RequirementsLock:  file("locks/requirements.lock"),
		Wheels:            []RuntimePackFile{file("wheels/runtime.whl")},
		Wheelhouse:        []RuntimePackFile{file("wheelhouse/fastapi.whl")},
		SceneCatalog:      file("catalog/catalog.yaml"),
		Licenses:          []RuntimePackFile{file("licenses/LICENSE")},
		VerificationFiles: []RuntimePackFile{file("verification/version.json")},
		SmokeSceneKey:     "palletizing_depalletizing_001", SmokeRequest: file("smoke/request.json"),
		ContentRequirements: map[string]RuntimeContentRequirement{
			"mujoco_assets": {Required: true},
		},
	}
	encoded, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "runtime-pack.yaml"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadRuntimePack(root)
	if err != nil {
		t.Fatalf("有效 Pack 无法加载: %v", err)
	}
	loaded.ContentRequirements["PATH"] = RuntimeContentRequirement{Required: true}
	if err := loaded.Validate(); err == nil {
		t.Fatal("Pack must reject unknown content requirements")
	}
	delete(loaded.ContentRequirements, "PATH")
	if loaded.Runner != "native-mujoco" || len(loaded.Files()) != 7 {
		t.Fatalf("manifest=%+v", loaded)
	}

	if err := os.WriteFile(filepath.Join(root, "wheels/runtime.whl"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuntimePack(root); err == nil {
		t.Fatal("被替换的 Wheel 必须被摘要检查拒绝")
	}
}

func TestRuntimePackRejectsUnknownContentAndArbitraryRunner(t *testing.T) {
	manifest := RuntimePackManifest{SchemaVersion: 1, PackID: "bad", PackVersion: "0.4.0",
		Profile: RuntimeProfile{RuntimeProfileID: "bad", Engine: "mujoco", Loader: "native"},
		Runner:  "sh -c malware", PythonVersion: "3.10", Wheels: []RuntimePackFile{{Path: "w", SHA256: string(make([]byte, 64))}},
		Wheelhouse:       []RuntimePackFile{{Path: "d", SHA256: string(make([]byte, 64))}},
		RequirementsLock: RuntimePackFile{Path: "r", SHA256: string(make([]byte, 64))},
		SceneCatalog:     RuntimePackFile{Path: "c", SHA256: string(make([]byte, 64))},
		SmokeRequest:     RuntimePackFile{Path: "s", SHA256: string(make([]byte, 64))},
		Licenses:         []RuntimePackFile{{Path: "l", SHA256: string(make([]byte, 64))}}, SmokeSceneKey: "scene"}
	if err := manifest.Validate(); err == nil {
		t.Fatal("Pack 不得提供任意 runner")
	}
	if _, err := RuntimeContentEnvironment(map[string]string{"PATH": "/tmp"}); err == nil {
		t.Fatal("未知 content_ref 不得转换为子进程环境")
	}
}

func TestRuntimeContentEnvironmentNativePaths(t *testing.T) {
	absolute := filepath.Join(t.TempDir(), "语义 assets")
	env, err := RuntimeContentEnvironment(map[string]string{"mujoco_assets": absolute})
	if err != nil || !slices.Contains(env, "MUJOCO_ASSET_ROOT="+absolute) {
		t.Fatalf("native absolute content path: env=%v err=%v", env, err)
	}
	for _, value := range []string{"", "assets", filepath.Join("..", "assets")} {
		if _, err := RuntimeContentEnvironment(map[string]string{"mujoco_assets": value}); err == nil {
			t.Fatalf("accepted non-absolute content path %q", value)
		}
	}
}
