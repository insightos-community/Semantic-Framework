package pilot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type MapSyncConfig struct {
	RobotStateEnabled  bool    `yaml:"robot_state_enabled"`
	RobotStateInterval float64 `yaml:"robot_state_interval_seconds"`
	Enabled            bool    `yaml:"enabled"`
	ProjectID          string  `yaml:"project_id"`
	ConsoleURL         string  `yaml:"console_url"`
	ConsoleTokenFile   string  `yaml:"console_token_file"`
	IntervalSeconds    int     `yaml:"interval_seconds"`
}

// MapSynchronizer runs inside Pilot. The console exposes an authenticated,
// read-only snapshot; ETag advances only after Server acknowledges persistence.
type MapSynchronizer struct {
	Config     MapSyncConfig
	ServerURL  string
	Credential string
	Client     *http.Client
	OnError    func(error)
}

func (s *MapSynchronizer) Sync(ctx context.Context, etag string) (string, error) {
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	secret, err := os.ReadFile(s.Config.ConsoleTokenFile)
	if err != nil {
		return etag, fmt.Errorf("read console credential: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.Config.ConsoleURL, "/")+"/api/semantic/map-snapshot", nil)
	if err != nil {
		return etag, err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(secret)))
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	response, err := client.Do(request)
	if err != nil {
		return etag, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		return etag, nil
	}
	if response.StatusCode != 200 {
		return etag, fmt.Errorf("console map snapshot HTTP %d", response.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil {
		return etag, err
	}
	if len(payload) > 8<<20 || !json.Valid(payload) {
		return etag, fmt.Errorf("invalid or oversized map snapshot")
	}
	if err := s.syncAssets(ctx, client, string(secret), payload); err != nil {
		return etag, err
	}
	body, err := json.Marshal(map[string]any{"project_id": s.Config.ProjectID, "snapshot": json.RawMessage(payload)})
	if err != nil {
		return etag, err
	}
	upload, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.ServerURL, "/")+"/pilot/v1/map-sync", bytes.NewReader(body))
	if err != nil {
		return etag, err
	}
	upload.Header.Set("Content-Type", "application/json")
	upload.Header.Set("Authorization", "Bearer "+s.Credential)
	ack, err := client.Do(upload)
	if err != nil {
		return etag, err
	}
	defer ack.Body.Close()
	if ack.StatusCode != 200 {
		return etag, fmt.Errorf("Server map sync HTTP %d", ack.StatusCode)
	}
	var result struct {
		MapID    string `json:"map_id"`
		Revision int64  `json:"revision"`
	}
	if err := json.NewDecoder(io.LimitReader(ack.Body, 4096)).Decode(&result); err != nil {
		return etag, err
	}
	if result.MapID != "real_map" || result.Revision <= 0 {
		return etag, fmt.Errorf("invalid map acknowledgement")
	}
	return response.Header.Get("ETag"), nil
}

func (s *MapSynchronizer) Run(ctx context.Context) {
	if s.Config.RobotStateEnabled {
		go s.RunTwin(ctx)
	}
	interval := time.Duration(s.Config.IntervalSeconds) * time.Second
	if interval < 5*time.Second {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	etag := "" // Reconnect reconciles Server state, including a restored Server DB.
	for {
		next, err := s.Sync(ctx, etag)
		if err != nil {
			if s.OnError != nil && ctx.Err() == nil {
				s.OnError(err)
			}
		} else {
			etag = next
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
