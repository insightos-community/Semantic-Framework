package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

const MaxPilotMapAssetBytes = 32 << 20

type PilotMapAsset struct {
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
	Format string `json:"format"`
}

func migrateV36(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE pilot_map_assets (sha256 TEXT PRIMARY KEY, content BLOB NOT NULL);
 CREATE TABLE pilot_map_asset_owners (pilot_id TEXT NOT NULL, sha256 TEXT NOT NULL REFERENCES pilot_map_assets(sha256), PRIMARY KEY(pilot_id,sha256));`)
	return err
}

func ValidMapAssetHash(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == value
}

// Only self-contained GLB 2.0 is accepted; a synced model cannot make the
// browser fetch URLs from the robot or third parties.
func ValidatePilotMapAsset(hash string, content []byte) error {
	sum := sha256.Sum256(content)
	if !ValidMapAssetHash(hash) || hex.EncodeToString(sum[:]) != hash || len(content) < 20 || len(content) > MaxPilotMapAssetBytes || string(content[:4]) != "glTF" || binary.LittleEndian.Uint32(content[4:8]) != 2 || int(binary.LittleEndian.Uint32(content[8:12])) != len(content) {
		return ErrInvalidState
	}
	var noURI func(any) bool
	noURI = func(v any) bool {
		switch x := v.(type) {
		case map[string]any:
			for k, value := range x {
				if k == "uri" || !noURI(value) {
					return false
				}
			}
		case []any:
			for _, value := range x {
				if !noURI(value) {
					return false
				}
			}
		}
		return true
	}
	for offset := 12; offset < len(content); {
		if offset+8 > len(content) {
			return ErrInvalidState
		}
		size := int(binary.LittleEndian.Uint32(content[offset : offset+4]))
		end := offset + 8 + size
		if size%4 != 0 || end > len(content) {
			return ErrInvalidState
		}
		if offset == 12 {
			var document map[string]any
			if string(content[offset+4:offset+8]) != "JSON" || json.Unmarshal(content[offset+8:end], &document) != nil || !noURI(document) {
				return ErrInvalidState
			}
		}
		offset = end
	}
	return nil
}

func (s *Store) PutPilotMapAsset(pilotID, hash string, content []byte) error {
	if err := ValidatePilotMapAsset(hash, content); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT OR IGNORE INTO pilot_map_assets(sha256,content) VALUES(?,?)`, hash, content); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO pilot_map_asset_owners(pilot_id,sha256) VALUES(?,?)`, pilotID, hash); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PilotMapAssetSize(pilotID, hash string) (int, error) {
	var size int
	err := s.db.QueryRow(`SELECT length(content) FROM pilot_map_assets a JOIN pilot_map_asset_owners o ON a.sha256=o.sha256 WHERE o.pilot_id=? AND a.sha256=?`, pilotID, hash).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return size, err
}

// References in retained generations keep assets readable after a source switch.
func (s *Store) ProjectMapAsset(projectID, hash string) ([]byte, error) {
	m, err := s.GetSemanticMap(projectID, MapSlotReal)
	if err != nil {
		return nil, err
	}
	var content []byte
	err = s.db.QueryRow(`SELECT content FROM pilot_map_assets a WHERE a.sha256=? AND EXISTS
 (SELECT 1 FROM map_entities e WHERE e.map_id=? AND (json_extract(e.properties_json,'$.visual_descriptor.map_visual.sha256')=a.sha256 OR json_extract(e.properties_json,'$.mapping.robot_visual.map_visual.sha256')=a.sha256))`, hash, m.ID).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return content, err
}

func validateMapVisuals(incoming PilotMapSnapshot) error {
	assets := map[string]bool{}
	if len(incoming.VisualAssets) > 64 {
		return ErrInvalidState
	}
	for _, asset := range incoming.VisualAssets {
		if !ValidMapAssetHash(asset.SHA256) || asset.Format != "glb" || asset.Bytes < 20 || asset.Bytes > MaxPilotMapAssetBytes || assets[asset.SHA256] {
			return ErrInvalidState
		}
		assets[asset.SHA256] = true
	}
	visuals := []map[string]any{}
	for _, row := range incoming.Entities {
		visuals = append(visuals, row.Entity.Properties)
	}
	if v, ok := incoming.Mapping["robot_visual"]; ok {
		visuals = append(visuals, map[string]any{"visual_descriptor": v})
	}
	for _, props := range visuals {
		value, exists := props["visual_descriptor"]
		if !exists {
			continue
		}
		visual, ok := value.(map[string]any)
		if !ok {
			return ErrInvalidState
		}
		content, ok := visual["map_visual"].(map[string]any)
		if !ok {
			return ErrInvalidState
		}
		hash, _ := content["sha256"].(string)
		if !assets[hash] || content["format"] != "glb" || content["content_url"] != nil || content["url"] != nil {
			return fmt.Errorf("unlisted or external map visual: %w", ErrInvalidState)
		}
		if heading, exists := visual["heading_offset_rad"]; exists {
			h, ok := heading.(float64)
			if !ok || !finite(h) {
				return ErrInvalidState
			}
		}
	}
	return nil
}
