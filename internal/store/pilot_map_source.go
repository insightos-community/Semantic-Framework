package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// Each Pilot publishes one current map. Projects independently subscribe to it.
func migrateV35(tx *sql.Tx) error {
	for _, query := range []string{
		`CREATE TABLE pilot_map_sources (pilot_id TEXT PRIMARY KEY, robot_id TEXT NOT NULL,
		 revision INTEGER NOT NULL, digest TEXT NOT NULL, received_at TIMESTAMP NOT NULL, snapshot BLOB NOT NULL)`,
		`CREATE TABLE project_pilot_maps (project_id TEXT PRIMARY KEY REFERENCES projects(id),
		 pilot_id TEXT NOT NULL, source_revision INTEGER NOT NULL DEFAULT 0,
		 synced_at TIMESTAMP NOT NULL, last_error TEXT NOT NULL DEFAULT '')`,
		`CREATE INDEX idx_project_pilot_maps_source ON project_pilot_maps(pilot_id)`,
	} {
		if _, err := tx.Exec(query); err != nil {
			return err
		}
	}
	return nil
}

type PilotMapSource struct {
	PilotID    string           `json:"pilot_id"`
	RobotID    string           `json:"robot_id"`
	Revision   int64            `json:"revision"`
	ReceivedAt time.Time        `json:"received_at"`
	Snapshot   PilotMapSnapshot `json:"-"`
}

type PilotMapBinding struct {
	ProjectID      string    `json:"project_id"`
	PilotID        string    `json:"pilot_id"`
	SourceRevision int64     `json:"source_revision"`
	SyncedAt       time.Time `json:"synced_at"`
	LastError      string    `json:"last_error,omitempty"`
}

func (s *Store) SavePilotMapSource(pilotID, robotID string, snapshot PilotMapSnapshot, now time.Time) (PilotMapSource, error) {
	if err := ValidatePilotMap(snapshot); err != nil {
		return PilotMapSource{}, err
	}
	for _, asset := range snapshot.VisualAssets {
		size, err := s.PilotMapAssetSize(pilotID, asset.SHA256)
		if err != nil || size != asset.Bytes {
			return PilotMapSource{}, ErrInvalidState
		}
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		return PilotMapSource{}, err
	}
	hash := sha256.Sum256(body)
	_, err = s.db.Exec(`INSERT INTO pilot_map_sources(pilot_id,robot_id,revision,digest,received_at,snapshot)
	 VALUES(?,?,1,?,?,?) ON CONFLICT(pilot_id) DO UPDATE SET robot_id=excluded.robot_id,
	 revision=pilot_map_sources.revision + CASE WHEN digest=excluded.digest THEN 0 ELSE 1 END,
	 digest=excluded.digest,received_at=excluded.received_at,snapshot=excluded.snapshot`,
		pilotID, robotID, hex.EncodeToString(hash[:]), now, body)
	if err != nil {
		return PilotMapSource{}, err
	}
	return s.GetPilotMapSource(pilotID)
}

// Lightweight status for the selector; never decode point clouds on polling.
func (s *Store) GetPilotMapSourceInfo(pilotID string) (PilotMapSource, error) {
	var row PilotMapSource
	err := s.db.QueryRow(`SELECT pilot_id,robot_id,revision,received_at FROM pilot_map_sources WHERE pilot_id=?`, pilotID).Scan(&row.PilotID, &row.RobotID, &row.Revision, &row.ReceivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return row, err
}

func (s *Store) GetPilotMapSource(pilotID string) (PilotMapSource, error) {
	var row PilotMapSource
	var body []byte
	err := s.db.QueryRow(`SELECT pilot_id,robot_id,revision,received_at,snapshot FROM pilot_map_sources WHERE pilot_id=?`, pilotID).
		Scan(&row.PilotID, &row.RobotID, &row.Revision, &row.ReceivedAt, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return row, ErrNotFound
	}
	if err == nil {
		err = json.Unmarshal(body, &row.Snapshot)
	}
	return row, err
}

func (s *Store) GetPilotMapBinding(projectID string) (PilotMapBinding, error) {
	var row PilotMapBinding
	err := s.db.QueryRow(`SELECT project_id,pilot_id,source_revision,synced_at,last_error FROM project_pilot_maps WHERE project_id=?`, projectID).
		Scan(&row.ProjectID, &row.PilotID, &row.SourceRevision, &row.SyncedAt, &row.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return row, err
}

func (s *Store) PilotMapProjects(pilotID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT project_id FROM project_pilot_maps WHERE pilot_id=? ORDER BY project_id`, pilotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

const savePilotMapBindingSQL = `INSERT INTO project_pilot_maps(project_id,pilot_id,source_revision,synced_at,last_error)
 VALUES(?,?,?,?,?) ON CONFLICT(project_id) DO UPDATE SET pilot_id=excluded.pilot_id,
 source_revision=excluded.source_revision,synced_at=excluded.synced_at,last_error=excluded.last_error`

func savePilotMapBindingTx(tx *sql.Tx, b PilotMapBinding) error {
	_, err := tx.Exec(savePilotMapBindingSQL, b.ProjectID, b.PilotID, b.SourceRevision, b.SyncedAt, b.LastError)
	return err
}

func (s *Store) SavePilotMapBinding(b PilotMapBinding) error {
	_, err := s.db.Exec(savePilotMapBindingSQL, b.ProjectID, b.PilotID, b.SourceRevision, b.SyncedAt, b.LastError)
	return err
}

// Binding and its projected map become visible in the same transaction.
func (s *Store) ApplyPilotMapSource(projectID string, source PilotMapSource, now time.Time) (MapSnapshot, error) {
	b := PilotMapBinding{ProjectID: projectID, PilotID: source.PilotID, SourceRevision: source.Revision, SyncedAt: now}
	return s.applyPilotMap(projectID, source.RobotID, source.Snapshot, now, &b)
}

// ProjectRealScene is the default scene configuration of a project connected
// to a Pilot. The persisted map binding is authoritative; there is no second
// scene record that can drift from it or be duplicated on page refresh.
type ProjectRealScene struct {
	SchemaVersion string `json:"schema_version"`
	SceneID       string `json:"scene_id"`
	ProjectID     string `json:"project_id"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	MapID         string `json:"map_id"`
	PilotID       string `json:"pilot_id"`
	RobotID       string `json:"robot_id"`
	RobotName     string `json:"robot_name"`
}

func (s *Store) GetProjectRealScene(projectID string) (*ProjectRealScene, error) {
	binding, err := s.GetPilotMapBinding(projectID)
	if errors.Is(err, ErrNotFound) || (err == nil && binding.PilotID == "") {
		return s.projectRobotRealScene(projectID)
	}
	if err != nil {
		return nil, err
	}
	pilot, err := s.GetRobotPilot(binding.PilotID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if pilot.Backend != "real" {
		return nil, nil
	}
	name := pilot.DisplayName
	if name == "" {
		name = pilot.RobotID
	}
	return &ProjectRealScene{SchemaVersion: "1.0.0", SceneID: "real-scene:" + projectID, ProjectID: projectID,
		Name: "实机场景 · " + name, Kind: "real", MapID: MapSlotReal, PilotID: binding.PilotID, RobotID: pilot.RobotID, RobotName: name}, nil
}

// A real robot can be added before it has published a map. Simulation-only
// projects keep the existing scene/Runtime onboarding path.
func (s *Store) projectRobotRealScene(projectID string) (*ProjectRealScene, error) {
	ids, err := s.ListProjectRobotIDs(projectID)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		pilot, err := s.GetActiveRobotPilot(id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if pilot.Backend != "real" {
			continue
		}
		name := pilot.DisplayName
		if name == "" {
			name = id
		}
		return &ProjectRealScene{SchemaVersion: "1.0.0", SceneID: "real-scene:" + projectID, ProjectID: projectID,
			Name: "实机场景 · " + name, Kind: "real", MapID: MapSlotReal, RobotID: id, RobotName: name}, nil
	}
	return nil, nil
}

// Lightweight identity read: telemetry never decodes or retransmits point clouds.
func (s *Store) PilotTwinIdentity(pilotID string) (string, string, string, string, error) {
	var hash, revision, model, urdf string
	err := s.db.QueryRow(`SELECT json_extract(snapshot,'$.map_hash'),json_extract(snapshot,'$.mapping_revision'),COALESCE(json_extract(snapshot,'$.mapping.robot_visual.map_visual.sha256'),''),COALESCE(json_extract(snapshot,'$.mapping.robot_visual.urdf_sha256'),'') FROM pilot_map_sources WHERE pilot_id=?`, pilotID).Scan(&hash, &revision, &model, &urdf)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return hash, revision, model, urdf, err
}
