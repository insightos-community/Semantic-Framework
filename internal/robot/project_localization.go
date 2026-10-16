package robot

import (
	"errors"
	"math"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

// ProjectRobots calls this only for an owned project member. Read the same
// ephemeral Pilot feedback as the map viewer, indexed by this robot's Pilot;
// never substitute the project's selected map source for another robot.
func (s *Service) projectLocalization(projectID, pilotID string) (map[string]any, error) {
	now := s.now()
	result := map[string]any{
		"available": false, "status": "unreported", "reason": "Pilot 尚未上报实时位姿",
		"source": "pilot_feedback", "pilot_id": pilotID,
		"checked_at": now.UTC().Format(time.RFC3339Nano), "max_age_s": 5.,
		"usage": "当前位置查询；不作为运动到位、停车或安全控制证据",
	}
	s.mapMu.Lock()
	defer s.mapMu.Unlock()
	binding, err := s.st.GetPilotMapBinding(projectID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	result["selected_map_source"] = pilotID != "" && err == nil && binding.PilotID == pilotID
	result["project_map_synchronized"] = false
	s.twinMu.RLock()
	sample, ok := s.twins[pilotID]
	s.twinMu.RUnlock()
	if !ok {
		return result, nil
	}
	v := sample.State
	hash, revision, model, urdf, err := s.st.PilotTwinIdentity(pilotID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if err != nil || v.MapHash != hash || v.MappingRevision != revision || v.ModelSHA256 != model || v.URDFSHA256 != urdf {
		result["status"], result["reason"] = "map_changed", "地图或模型已变更，等待匹配的新位姿"
		return result, nil
	}
	if result["selected_map_source"] == true {
		info, err := s.st.GetPilotMapSourceInfo(pilotID)
		if err != nil {
			return nil, err
		}
		result["project_map_synchronized"] = binding.LastError == "" && binding.SourceRevision == info.Revision
	}
	s.mu.RLock()
	session := s.sessions[pilotID]
	s.mu.RUnlock()
	if session != sample.Session || !s.IsOnline(pilotID) {
		result["status"], result["reason"] = "offline", "Pilot 已断开或重连，旧会话位姿不作为当前位置"
		return result, nil
	}
	if len(v.Pose) != 4 || v.PoseAge == nil {
		result["status"], result["reason"] = "pose_missing", "Pilot 有状态反馈，但本帧没有有效定位"
		return result, nil
	}
	// Source clocks can differ from Server clocks. Freshness uses Server elapsed
	// time plus the source-provided ages, never wall-clock timestamp subtraction.
	age := math.Max(0, now.Sub(sample.At).Seconds()) + v.SampleAge + *v.PoseAge
	result["age_s"] = age
	result["sample_received_at"] = sample.At.UTC().Format(time.RFC3339Nano)
	result["source_collected_at_unix_s"] = v.CollectedAt
	if age > 5 {
		result["status"], result["reason"] = "stale", "位姿已超过 5 秒，不作为当前位置"
		return result, nil
	}
	result["available"], result["status"], result["reason"] = true, "current", ""
	result["frame"], result["units"] = v.Frame, v.Units
	result["map_hash"], result["mapping_revision"] = hash, revision
	result["sequence"] = v.Sequence
	result["pose"] = map[string]any{"x": v.Pose[0], "y": v.Pose[1], "yaw": v.Pose[3]}
	// Twin z may be estimated from a floor annotation; do not present it as a
	// measured localization height.
	return result, nil
}
