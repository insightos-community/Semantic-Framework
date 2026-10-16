package robot

import (
	"fmt"
	"insightos.cn/semantic-framework/internal/store"
	"math"
	"time"
)

// Live display state is ephemeral. It must never advance a map revision or
// become control/stop evidence. Source clocks are descriptive, not freshness clocks.
type PilotTwinState struct {
	SampleInterval  float64            `json:"sample_interval_s,omitempty"`
	SchemaVersion   string             `json:"schema_version"`
	StreamID        string             `json:"stream_id"`
	Sequence        int64              `json:"sequence"`
	CollectedAt     float64            `json:"collected_at"`
	SampleAge       float64            `json:"sample_age_s"`
	Frame           string             `json:"frame"`
	Units           string             `json:"units"`
	MapHash         string             `json:"map_hash"`
	MappingRevision string             `json:"mapping_revision"`
	ModelSHA256     string             `json:"model_sha256"`
	URDFSHA256      string             `json:"urdf_sha256"`
	Pose            []float64          `json:"pose"`
	PoseAge         *float64           `json:"pose_age_s"`
	FloorSource     string             `json:"floor_source"`
	Joints          map[string]float64 `json:"joints"`
	Components      map[string]any     `json:"components"`
	GripperMapping  string             `json:"gripper_mapping"`
	Errors          []string           `json:"errors"`
}

type twinSample struct {
	State   PilotTwinState
	At      time.Time
	Session *pilotSession
}

func validTwin(v PilotTwinState) bool {
	finite := func(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }
	if !finite(v.SampleInterval) || (v.SampleInterval != 0 && (v.SampleInterval < .1 || v.SampleInterval > 60)) {
		return false
	}
	if v.SchemaVersion != "1.0.0" || v.Frame != "map" || v.Units != "m_rad" || v.StreamID == "" || len(v.StreamID) > 128 || v.Sequence < 1 || !finite(v.CollectedAt) || !finite(v.SampleAge) || v.SampleAge < 0 || v.SampleAge > 120 || !store.ValidMapAssetHash(v.ModelSHA256) || !store.ValidMapAssetHash(v.URDFSHA256) || len(v.Joints) > 128 {
		return false
	}
	if v.Pose != nil && len(v.Pose) != 4 {
		return false
	}
	if v.Pose != nil && v.PoseAge == nil {
		return false
	}
	if v.PoseAge != nil && (!finite(*v.PoseAge) || *v.PoseAge < 0) {
		return false
	}
	for _, x := range v.Pose {
		if !finite(x) {
			return false
		}
	}
	for name, x := range v.Joints {
		if len(name) == 0 || len(name) > 128 || !finite(x) {
			return false
		}
	}
	return true
}

func (s *Service) SyncPilotTwin(pilotID string, value PilotTwinState) error {
	if !validTwin(value) {
		return store.ErrInvalidState
	}
	if !s.IsOnline(pilotID) {
		return ErrPilotOffline
	}
	if err := s.AuthorizeMapAsset(pilotID); err != nil {
		return err
	}
	s.mapMu.Lock()
	defer s.mapMu.Unlock()
	hash, revision, model, urdf, err := s.st.PilotTwinIdentity(pilotID)
	if err != nil {
		return err
	}
	if value.MapHash != hash || value.MappingRevision != revision || value.ModelSHA256 != model || value.URDFSHA256 != urdf {
		return fmt.Errorf("twin identity differs from published map/model: %w", store.ErrInvalidState)
	}
	s.mu.RLock()
	session := s.sessions[pilotID]
	s.mu.RUnlock()
	s.twinMu.Lock()
	defer s.twinMu.Unlock()
	if old, ok := s.twins[pilotID]; ok && old.Session == session && old.State.StreamID == value.StreamID && value.Sequence <= old.State.Sequence {
		return store.ErrInvalidState
	}
	s.twins[pilotID] = twinSample{State: value, At: s.now(), Session: session}
	return nil
}

func (s *Service) ProjectTwin(owner, projectID string) (map[string]any, error) {
	project, err := s.st.GetProject(projectID)
	if err != nil || project.OwnerID != owner {
		return nil, ErrDirectRunScope
	}
	binding, err := s.st.GetPilotMapBinding(projectID)
	empty := map[string]any{"available": false, "reason": "等待所选 Pilot 的机器人反馈"}
	if err == store.ErrNotFound || (err == nil && binding.PilotID == "") {
		return empty, nil
	}
	if err != nil {
		return nil, err
	}
	p, err := s.mapPilot(owner, projectID, binding.PilotID)
	if err != nil {
		return nil, err
	}
	s.twinMu.RLock()
	sample, ok := s.twins[binding.PilotID]
	s.twinMu.RUnlock()
	if !ok {
		return empty, nil
	}
	hash, revision, model, urdf, err := s.st.PilotTwinIdentity(binding.PilotID)
	if err != nil {
		return nil, err
	}
	if sample.State.MapHash != hash || sample.State.MappingRevision != revision || sample.State.ModelSHA256 != model || sample.State.URDFSHA256 != urdf {
		return empty, nil
	}
	s.mu.RLock()
	session := s.sessions[binding.PilotID]
	s.mu.RUnlock()
	age := math.Max(0, s.now().Sub(sample.At).Seconds()) + sample.State.SampleAge
	return map[string]any{"available": true, "pilot_id": binding.PilotID, "robot_id": p.RobotID,
		"online": s.IsOnline(binding.PilotID) && sample.Session == session, "age_s": age, "state": sample.State}, nil
}
