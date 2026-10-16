package pilot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestTwinUploadsSmallFreshStateWithoutMapOrAssets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	os.WriteFile(path, []byte("local-secret"), 0600)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/semantic/robot-state" || r.Header.Get("Authorization") != "Bearer local-secret" {
			t.Error("unexpected local access", r.URL.Path)
		}
		w.Write([]byte(`{"sample_age_s":0.1,"sequence":17}`))
	}))
	defer local.Close()
	calls := 0
	reject := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/pilot/v1/robot-state" || r.Header.Get("Authorization") != "Bearer pilot-secret" {
			t.Error("unexpected server access")
		}
		var v map[string]any
		json.NewDecoder(r.Body).Decode(&v)
		if v["sample_age_s"].(float64) < .1 || v["sequence"].(float64) != 17 {
			t.Error("frame changed")
		}
		if reject {
			w.WriteHeader(409)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	s := MapSynchronizer{Config: MapSyncConfig{ConsoleURL: local.URL, ConsoleTokenFile: path}, ServerURL: server.URL, Credential: "pilot-secret"}
	if err := s.SyncTwin(context.Background()); err != nil {
		t.Fatal(err)
	}
	reject = true
	if err := s.SyncTwin(context.Background()); err == nil {
		t.Fatal("server rejection ignored")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.SyncTwin(ctx); err == nil || calls != 2 {
		t.Fatal("cancelled request dispatched")
	}
}
