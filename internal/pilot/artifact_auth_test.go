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

package pilot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtifactStoreAuthenticatesServerTransfers(t *testing.T) {
	const token = "pilot-transfer-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/download":
			_, _ = w.Write([]byte("server-artifact"))
		case "/upload":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"server_ref": "artifact://server-1", "sync_status": "synced",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	store := NewArtifactStore(t.TempDir(), server.URL)
	store.AccessToken = token
	store.HTTPClient = server.Client()
	execution := SkillExecution{ID: "execution-auth"}
	store.Authorize(execution.ID, []map[string]any{{
		"ref": "artifact://input-1", "url": "/download", "media_type": "text/plain",
	}})
	resolved, err := store.Resolve(context.Background(), execution, "artifact://input-1")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(resolved["local_path"].(string))
	if err != nil || string(body) != "server-artifact" {
		t.Fatalf("下载内容错误: body=%q err=%v", body, err)
	}

	workspace, err := store.Workspace(execution)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace, "evidence.txt")
	if err := os.WriteFile(path, []byte("pilot-artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	published, err := store.Publish(context.Background(), execution, path, "text/plain", "evidence")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(published["local_ref"].(string), "/")
	localID := parts[len(parts)-1]
	uploaded, err := store.Upload(context.Background(), localID, "/upload")
	if err != nil {
		t.Fatal(err)
	}
	if uploaded["server_ref"] != "artifact://server-1" || uploaded["sync_status"] != "synced" {
		t.Fatalf("上传结果错误: %#v", uploaded)
	}

	unauthorized := NewArtifactStore(t.TempDir(), server.URL)
	unauthorized.HTTPClient = server.Client()
	unauthorized.Authorize(execution.ID, []map[string]any{{"ref": "artifact://input-2", "url": "/download"}})
	if _, err := unauthorized.Resolve(context.Background(), execution, "artifact://input-2"); err == nil ||
		!strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("无令牌下载必须拒绝: %v", err)
	}
}

func TestArtifactStoreImportsManagedAbilityEvidenceOnce(t *testing.T) {
	exchange := t.TempDir()
	if err := os.MkdirAll(filepath.Join(exchange, "captures", "invocation"), 0o750); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(exchange, "captures", "invocation", "rgb.jpg")
	if err := os.WriteFile(source, []byte("rgb-frame"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewArtifactStore(t.TempDir(), "")
	store.PilotInstanceID = "pilot-1"
	store.AbilityExchangeDirectory = exchange
	announcements := 0
	store.Announce = func(SkillExecution, localArtifact) error {
		announcements++
		return nil
	}
	execution := SkillExecution{ID: "execution-evidence"}
	first, err := store.ImportAbilityArtifact(
		context.Background(),
		execution,
		"captures/invocation/rgb.jpg",
		"image/jpeg",
		"抓取前 RGB",
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.ImportAbilityArtifact(
		context.Background(),
		execution,
		"captures/invocation/rgb.jpg",
		"image/jpeg",
		"抓取前 RGB",
	)
	if err != nil {
		t.Fatal(err)
	}
	if first["local_ref"] != second["local_ref"] || announcements != 1 {
		t.Fatalf("重复导入必须复用同一 Artifact: first=%v second=%v announcements=%d", first, second, announcements)
	}
	journal := NewMemoryActionJournal()
	action := ActionExecution{
		ID: "action-capture",
		Result: map[string]any{"artifact_candidates": []any{map[string]any{
			"candidate_id":  "rgb-1",
			"exchange_path": "captures/invocation/rgb.jpg",
			"media_type":    "image/jpeg",
		}}},
		Observations: []map[string]any{{"kind": "sensor.frame"}},
	}
	if err := journal.SaveAction(action); err != nil {
		t.Fatal(err)
	}
	runtime := &SkillRuntime{artifacts: store, runner: &Runner{journal: journal}}
	translated := runtime.importAbilityArtifacts(context.Background(), execution, action)
	refs, ok := translated.Result["artifact_refs"].([]any)
	if !ok || len(refs) != 1 || refs[0] != first["local_ref"] {
		t.Fatalf("Action Result 未写回 Pilot ArtifactRef: %#v", translated.Result)
	}
	observationRefs, ok := translated.Observations[0]["evidence_refs"].([]any)
	if !ok || len(observationRefs) != 1 || observationRefs[0] != first["local_ref"] {
		t.Fatalf("Observation 未关联真实证据: %#v", translated.Observations)
	}
	if _, err := store.ImportAbilityArtifact(
		context.Background(),
		execution,
		source,
		"image/jpeg",
		"越界",
	); !errors.Is(err, ErrArtifactPathOutsideWorkspace) {
		t.Fatalf("绝对路径必须拒绝: %v", err)
	}
}

func TestArtifactStoreImportsLegacyInstanceNestedAbilityEvidence(t *testing.T) {
	exchange := t.TempDir()
	instanceRoot := filepath.Join(exchange, "c28f3bf4-fd25-465c-afda-5cad3ef83205")
	if err := os.MkdirAll(filepath.Join(instanceRoot, "captures", "invocation"), 0o750); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(instanceRoot, "captures", "invocation", "rgb.jpg")
	if err := os.WriteFile(source, []byte("legacy-rgb-frame"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.jpg")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(instanceRoot, "captures", "invocation", "escape.jpg")); err != nil {
		t.Fatal(err)
	}

	store := NewArtifactStore(t.TempDir(), "")
	store.PilotInstanceID = "pilot-1"
	store.AbilityExchangeDirectory = exchange
	store.Announce = func(SkillExecution, localArtifact) error { return nil }
	execution := SkillExecution{ID: "execution-legacy-evidence"}

	published, err := store.ImportAbilityArtifact(
		context.Background(),
		execution,
		"captures/invocation/rgb.jpg",
		"image/jpeg",
		"旧 Ability 套层后的 RGB",
	)
	if err != nil {
		t.Fatal(err)
	}
	if published["local_ref"] == "" {
		t.Fatalf("套层证据必须导入为 Pilot Artifact: %#v", published)
	}

	if _, err := store.ImportAbilityArtifact(
		context.Background(),
		execution,
		"../secret.jpg",
		"image/jpeg",
		"越界",
	); !errors.Is(err, ErrArtifactPathOutsideWorkspace) {
		t.Fatalf("相对越界路径必须拒绝: %v", err)
	}
	if _, err := store.ImportAbilityArtifact(
		context.Background(),
		execution,
		"captures/invocation/escape.jpg",
		"image/jpeg",
		"符号链接越界",
	); !errors.Is(err, ErrArtifactPathOutsideWorkspace) {
		t.Fatalf("指向交换根外的符号链接必须拒绝: %v", err)
	}

	duplicate := filepath.Join(exchange, "50c3452f-f455-496d-a6f6-6ed4da9409fa", "captures", "invocation")
	if err := os.MkdirAll(duplicate, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(duplicate, "rgb.jpg"), []byte("other-frame"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportAbilityArtifact(
		context.Background(),
		SkillExecution{ID: "execution-ambiguous"},
		"captures/invocation/rgb.jpg",
		"image/jpeg",
		"歧义",
	); err == nil || !strings.Contains(err.Error(), "多个同名证据") {
		t.Fatalf("多个套层副本必须拒绝猜测: %v", err)
	}
}
