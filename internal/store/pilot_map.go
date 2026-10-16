package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"time"
)

// PilotMapSnapshot is an immutable, metric projection of a robot's current map.
// Identity is separate from Semantic's generation/revision counters.
type PilotMapSnapshot struct {
	VisualAssets    []PilotMapAsset   `json:"visual_assets,omitempty"`
	SchemaVersion   string            `json:"schema_version"`
	FrameID         string            `json:"frame_id"`
	Units           string            `json:"units"`
	MapHash         string            `json:"map_hash"`
	MappingRevision string            `json:"mapping_revision"`
	Mapping         map[string]any    `json:"mapping"`
	Points          [][3]float64      `json:"points_m"`
	Entities        []MapSourceEntity `json:"entities"`
}

func (s *Store) PilotEnrollmentOwner(pilotID string) (string, error) {
	var owner string
	err := s.db.QueryRow(`SELECT created_by FROM pilot_enrollments WHERE pilot_instance_id=? AND status='claimed' ORDER BY claimed_at DESC LIMIT 1`, pilotID).Scan(&owner)
	return owner, err
}

func (s *Store) ApplyPilotMap(projectID, robotID string, incoming PilotMapSnapshot, now time.Time) (MapSnapshot, error) {
	return s.applyPilotMap(projectID, robotID, incoming, now, nil)
}

func ValidatePilotMap(incoming PilotMapSnapshot) error {
	if (incoming.SchemaVersion != "1.0.0" && incoming.SchemaVersion != "1.1.0") || incoming.FrameID != "map" || incoming.Units != "m" || incoming.MapHash == "" || incoming.MappingRevision == "" || len(incoming.Entities) > 1000 || len(incoming.Points) > 60000 {
		return fmt.Errorf("invalid Pilot map contract: %w", ErrInvalidState)
	}
	if err := validateMapVisuals(incoming); err != nil {
		return err
	}
	seen := map[string]bool{"@map": true}
	for _, row := range incoming.Entities {
		if strings.TrimSpace(row.Entity.Name) == "" || strings.TrimSpace(row.Entity.Type) == "" || row.SourceID == "" || seen[row.SourceID] || row.Entity.FrameID != incoming.FrameID || row.Entity.ID != "" || len(row.Entity.ManualFields) != 0 {
			return fmt.Errorf("invalid or duplicate source entity: %w", ErrInvalidState)
		}
		if err := validatePose(row.Entity.Pose); err != nil {
			return err
		}
		if err := validateBounds(row.Entity.Bounds); err != nil {
			return err
		}
		seen[row.SourceID] = true
	}
	for _, p := range incoming.Points {
		if !finite(p[0]) || !finite(p[1]) || !finite(p[2]) {
			return ErrInvalidState
		}
	}
	return nil
}

func (s *Store) applyPilotMap(projectID, robotID string, incoming PilotMapSnapshot, now time.Time, binding *PilotMapBinding) (MapSnapshot, error) {
	if err := ValidatePilotMap(incoming); err != nil {
		return MapSnapshot{}, err
	}
	encoded, err := json.Marshal(incoming)
	if err != nil {
		return MapSnapshot{}, err
	}
	digest := sha256.Sum256(encoded)
	hash := hex.EncodeToString(digest[:])
	current, err := s.GetMapSnapshot(projectID, MapSlotReal, 0)
	if err != nil {
		return MapSnapshot{}, err
	}
	// A project real_map has one coordinate space. Another robot/map source
	// cannot silently cohabit it unless aligned explicitly in a future revision.
	own := map[string]MapEntity{}
	unchanged, replacement := false, false
	for _, e := range current.Entities {
		if e.Status == EntityStatusRemoved {
			continue
		}
		if e.Properties["pilot_robot_id"] == robotID {
			sourceID, _ := e.Properties["pilot_source_id"].(string)
			own[sourceID] = e
			if binding != nil && sourceID == "@map" && e.Properties["pilot_instance_id"] != nil && e.Properties["pilot_instance_id"] != binding.PilotID {
				replacement = true
			}
			if sourceID == "@map" && e.Properties["snapshot_sha256"] == hash {
				unchanged = true
			}
			if sourceID == "@map" && (e.Properties["map_hash"] != incoming.MapHash || e.Properties["mapping_revision"] != incoming.MappingRevision) {
				replacement = true
			}
		} else if e.FrameID != incoming.FrameID || e.Properties["pilot_robot_id"] != nil {
			if binding == nil {
				return MapSnapshot{}, fmt.Errorf("real_map contains another coordinate source: %w", ErrInvalidState)
			}
			replacement = true
		}
	}
	if unchanged && !replacement {
		if binding != nil {
			return current, s.SavePilotMapBinding(*binding)
		}
		return current, nil
	}
	rows := make([]MapSourceEntity, len(incoming.Entities), len(incoming.Entities)+1)
	copy(rows, incoming.Entities)
	seen := map[string]bool{"@map": true}
	for _, row := range rows {
		seen[row.SourceID] = true
	}
	rows = append(rows, MapSourceEntity{SourceID: "@map", Entity: MapEntity{
		Type: "map_layer", Name: "Robot map", FrameID: incoming.FrameID, Status: EntityStatusActive,
		Pose: Pose{Orientation: Quaternion{W: 1}}, Bounds: Bounds{Kind: "point"},
		Properties: map[string]any{"map_hash": incoming.MapHash, "mapping_revision": incoming.MappingRevision,
			"mapping": incoming.Mapping, "points_m": incoming.Points, "snapshot_sha256": hash},
	}})
	update := MapUpdate{Generation: current.Map.Generation, ExpectedRevision: current.Map.Revision, Source: "observation", FrameID: incoming.FrameID, pilotBinding: binding}
	if replacement {
		update.NewGenerationReason = "robot_map_replaced"
		own = map[string]MapEntity{}
	}
	for _, row := range rows {
		properties := map[string]any{}
		for key, value := range row.Entity.Properties {
			properties[key] = value
		}
		// Clone descriptors: project-specific URLs must not mutate the cached source.
		if descriptor, ok := properties["visual_descriptor"].(map[string]any); ok {
			visual := map[string]any{}
			for k, v := range descriptor {
				visual[k] = v
			}
			source := descriptor["map_visual"].(map[string]any)
			content := map[string]any{}
			for k, v := range source {
				content[k] = v
			}
			content["content_url"] = "/api/v1/projects/" + url.PathEscape(projectID) + "/maps/visual-assets/" + source["sha256"].(string) + ".glb"
			visual["map_visual"] = content
			properties["visual_descriptor"] = visual
		}
		row.Entity.Properties = properties
		row.Entity.Properties["pilot_robot_id"] = robotID
		if binding != nil {
			row.Entity.Properties["pilot_instance_id"] = binding.PilotID
		}
		row.Entity.Properties["pilot_source_id"] = row.SourceID
		row.Entity.Status = EntityStatusActive
		existing, exists := own[row.SourceID]
		// Compare JSON values so the first decode (float64) matches typed geometry.
		if exists && samePilotEntity(existing, row.Entity) {
			continue
		}
		row.SourceID = robotID + ":" + row.SourceID
		update.SourceEntities = append(update.SourceEntities, row)
	}
	for sourceID, e := range own {
		if !seen[sourceID] {
			update.RemoveEntityIDs = append(update.RemoveEntityIDs, e.ID)
		}
	}
	return s.ApplyMapUpdate(projectID, MapSlotReal, update, now)
}

func samePilotEntity(a, b MapEntity) bool {
	b.ID = a.ID
	b.MapID = a.MapID
	b.Generation = a.Generation
	b.Revision = a.Revision
	b.UpdatedAt = a.UpdatedAt
	b.Source = a.Source
	preserveManual(&b, a)
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	var ax, by any
	_ = json.Unmarshal(x, &ax)
	_ = json.Unmarshal(y, &by)
	return reflect.DeepEqual(ax, by)
}
