package pilot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The snapshot lists content hashes, never arbitrary fetch URLs. Models travel
// once over authenticated HTTP before publishing the referencing snapshot.
func (s *MapSynchronizer) syncAssets(ctx context.Context, client *http.Client, secret string, payload []byte) error {
	var snapshot struct {
		Assets []struct {
			SHA256 string `json:"sha256"`
			Bytes  int    `json:"bytes"`
			Format string `json:"format"`
		} `json:"visual_assets"`
	}
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return err
	}
	if len(snapshot.Assets) > 64 {
		return fmt.Errorf("too many map assets")
	}
	for _, asset := range snapshot.Assets {
		decoded, err := hex.DecodeString(asset.SHA256)
		if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != asset.SHA256 || asset.Bytes < 20 || asset.Bytes > 32<<20 || asset.Format != "glb" {
			return fmt.Errorf("invalid map asset descriptor")
		}
		destination := strings.TrimRight(s.ServerURL, "/") + "/pilot/v1/map-assets/" + asset.SHA256
		check, err := http.NewRequestWithContext(ctx, http.MethodHead, destination, nil)
		if err != nil {
			return err
		}
		check.Header.Set("Authorization", "Bearer "+s.Credential)
		response, err := client.Do(check)
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode == 200 {
			continue
		}
		if response.StatusCode != 404 {
			return fmt.Errorf("check map asset HTTP %d", response.StatusCode)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.Config.ConsoleURL, "/")+"/api/semantic/map-assets/"+asset.SHA256+".glb", nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(secret))
		response, err = client.Do(request)
		if err != nil {
			return err
		}
		content, readErr := io.ReadAll(io.LimitReader(response.Body, (32<<20)+1))
		response.Body.Close()
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(content)
		if response.StatusCode != 200 || len(content) != asset.Bytes || hex.EncodeToString(sum[:]) != asset.SHA256 {
			return fmt.Errorf("invalid map asset content HTTP %d", response.StatusCode)
		}
		upload, err := http.NewRequestWithContext(ctx, http.MethodPut, destination, bytes.NewReader(content))
		if err != nil {
			return err
		}
		upload.Header.Set("Authorization", "Bearer "+s.Credential)
		upload.Header.Set("Content-Type", "model/gltf-binary")
		response, err = client.Do(upload)
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode != 204 {
			return fmt.Errorf("upload map asset HTTP %d", response.StatusCode)
		}
	}
	return nil
}
