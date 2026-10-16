package pilot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
)

// A separate bounded loop keeps slow model/map uploads out of the 1 Hz stream.
// No queue or catch-up: reconnect samples current hardware, never replays motion.
func (s *MapSynchronizer) SyncTwin(ctx context.Context) error {
	started := time.Now()
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	secret, err := os.ReadFile(s.Config.ConsoleTokenFile)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.Config.ConsoleURL, "/")+"/api/semantic/robot-state", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(secret)))
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	response.Body.Close()
	if readErr != nil {
		return readErr
	}
	if response.StatusCode != 200 || len(body) > 64<<10 {
		return fmt.Errorf("robot state HTTP %d or oversized", response.StatusCode)
	}
	var value map[string]any
	if json.Unmarshal(body, &value) != nil {
		return fmt.Errorf("invalid robot state JSON")
	}
	age, ok := value["sample_age_s"].(float64)
	if !ok || age < 0 {
		return fmt.Errorf("invalid robot sample age")
	}
	value["sample_interval_s"] = twinInterval(s.Config.RobotStateInterval)
	value["sample_age_s"] = age + time.Since(started).Seconds()
	body, err = json.Marshal(value)
	if err != nil {
		return err
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.ServerURL, "/")+"/pilot/v1/robot-state", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.Credential)
	req.Header.Set("Content-Type", "application/json")
	response, err = client.Do(req)
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode != 204 {
		return fmt.Errorf("Server robot state HTTP %d", response.StatusCode)
	}
	return nil
}

func (s *MapSynchronizer) RunTwin(ctx context.Context) {
	seconds := twinInterval(s.Config.RobotStateInterval)
	ticker := time.NewTicker(time.Duration(seconds * float64(time.Second)))
	defer ticker.Stop()
	lastError := time.Time{}
	for {
		call, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := s.SyncTwin(call)
		cancel()
		if err != nil && s.OnError != nil && ctx.Err() == nil && time.Since(lastError) > 30*time.Second {
			s.OnError(err)
			lastError = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func twinInterval(seconds float64) float64 {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return 1
	}
	return math.Max(.1, math.Min(60, seconds))
}
