package pilot

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMapSyncAcknowledgesOnlyPersistedSnapshot(t *testing.T) {
	uploadCount := 0
	accepted := false
	token := filepath.Join(t.TempDir(), "console-token")
	_ = os.WriteFile(token, []byte("console-secret"), 0600)
	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer console-secret" {
			t.Error("missing console authentication")
		}
		if r.Header.Get("If-None-Match") == "\"snapshot\"" {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", "\"snapshot\"")
		_, _ = w.Write([]byte(`{"schema_version":"1.0.0"}`))
	}))
	defer console.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploadCount++
		if r.URL.Path != "/pilot/v1/map-sync" || r.Header.Get("Authorization") != "Bearer pilot-secret" {
			t.Error("wrong upload routing")
		}
		if !accepted {
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"map_id": "real_map", "revision": 1})
	}))
	defer server.Close()
	sync := MapSynchronizer{Config: MapSyncConfig{ConsoleURL: console.URL, ConsoleTokenFile: token, ProjectID: "project"}, ServerURL: server.URL, Credential: "pilot-secret"}
	etag, err := sync.Sync(context.Background(), "")
	if err == nil || etag != "" {
		t.Fatal("failed upload was acknowledged")
	}
	accepted = true
	// Source publication is independent of a project. Selection is Server-side.
	sync.Config.ProjectID = ""
	etag, err = sync.Sync(context.Background(), etag)
	if err != nil || etag != "\"snapshot\"" {
		t.Fatalf("%q %v", etag, err)
	}
	_, err = sync.Sync(context.Background(), etag)
	if err != nil || uploadCount != 2 {
		t.Fatalf("unchanged snapshot retransmitted: %d %v", uploadCount, err)
	}
}

func TestMapAssetsUploadBeforeSnapshotAndRemainCached(t *testing.T) {
	content := []byte("glTF test data long enough")
	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	token := filepath.Join(t.TempDir(), "token")
	os.WriteFile(token, []byte("local-secret"), 0600)
	reads, uploads, maps := 0, 0, 0
	cached, fail := false, true
	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-secret" {
			t.Error("local credentials missing")
		}
		if strings.HasPrefix(r.URL.Path, "/api/semantic/map-assets/") {
			reads++
			w.Write(content)
			return
		}
		w.Header().Set("ETag", "snapshot")
		json.NewEncoder(w).Encode(map[string]any{"visual_assets": []any{map[string]any{"sha256": hash, "bytes": len(content), "format": "glb"}}})
	}))
	defer console.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer remote-secret" {
			t.Error("remote credentials missing")
		}
		if r.Method == http.MethodHead {
			if !cached {
				w.WriteHeader(404)
			}
			return
		}
		if r.Method == http.MethodPut {
			uploads++
			if fail {
				w.WriteHeader(503)
				return
			}
			cached = true
			w.WriteHeader(204)
			return
		}
		maps++
		if !cached {
			t.Error("snapshot sent before asset")
		}
		json.NewEncoder(w).Encode(map[string]any{"map_id": "real_map", "revision": 1})
	}))
	defer server.Close()
	sync := MapSynchronizer{Config: MapSyncConfig{ConsoleURL: console.URL, ConsoleTokenFile: token}, ServerURL: server.URL, Credential: "remote-secret"}
	if etag, err := sync.Sync(context.Background(), ""); err == nil || etag != "" || maps != 0 {
		t.Fatal("failed asset acknowledged")
	}
	fail = false
	if _, err := sync.Sync(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := sync.Sync(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if reads != 2 || uploads != 2 || maps != 2 {
		t.Fatalf("assets retransferred: %d %d %d", reads, uploads, maps)
	}
}
