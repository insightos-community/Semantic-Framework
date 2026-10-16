package robot

import (
	"errors"
	"fmt"
	"net/url"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

var ErrMapSourceUnavailable = errors.New("Pilot 尚未提供可用地图")

// Acknowledges durable source storage and delivery to all subscribed projects.
// revision remains for older Pilots; it is the source revision, not a project revision.
type PilotMapReceipt struct {
	MapID    string `json:"map_id"`
	Revision int64  `json:"revision"`
}

type PilotMapChoice struct {
	PilotID    string     `json:"pilot_id"`
	RobotID    string     `json:"robot_id"`
	Name       string     `json:"name"`
	Online     bool       `json:"online"`
	ServiceURL string     `json:"service_url,omitempty"`
	Available  bool       `json:"available"`
	Revision   int64      `json:"revision"`
	ReceivedAt *time.Time `json:"received_at,omitempty"`
}

type ProjectMapSources struct {
	Sources []PilotMapChoice       `json:"sources"`
	Binding *store.PilotMapBinding `json:"binding"`
}

// This endpoint is descriptive only. The Server never dereferences a robot-local URL.
func mapServiceURL(config map[string]any) string {
	for _, key := range []string{"service_url", "console_url"} {
		value, _ := config[key].(string)
		u, err := url.Parse(value)
		if err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") {
			u.User = nil
			u.RawQuery = ""
			u.Fragment = ""
			return u.String()
		}
	}
	return ""
}

func (s *Service) mapPilot(owner, projectID, pilotID string) (store.RobotPilot, error) {
	p, err := s.st.GetRobotPilot(pilotID)
	if err != nil {
		return p, err
	}
	enrolled, err := s.st.PilotEnrollmentOwner(pilotID)
	cfg, ok := p.Configuration["map_sync"].(map[string]any)
	if err != nil || enrolled != owner || !ok || p.Backend != "real" || cfg["enabled"] == false {
		return p, ErrDirectRunScope
	}
	if projectID != "" {
		if err = s.ValidateRobotProject(projectID, p.RobotID); err != nil {
			return p, err
		}
	}
	return p, nil
}

func (s *Service) MapSources(owner, projectID string) (ProjectMapSources, error) {
	s.mapMu.Lock()
	defer s.mapMu.Unlock()
	result := ProjectMapSources{Sources: []PilotMapChoice{}}
	project, err := s.st.GetProject(projectID)
	if err != nil || project.OwnerID != owner {
		return result, ErrDirectRunScope
	}
	pilots, err := s.st.ListRobotPilots()
	if err != nil {
		return result, err
	}
	for _, p := range pilots {
		if _, err = s.mapPilot(owner, projectID, p.PilotInstanceID); err != nil {
			continue
		}
		cfg := p.Configuration["map_sync"].(map[string]any)
		item := PilotMapChoice{PilotID: p.PilotInstanceID, RobotID: p.RobotID, Name: p.DisplayName, Online: s.IsOnline(p.PilotInstanceID), ServiceURL: mapServiceURL(cfg)}
		source, sourceErr := s.st.GetPilotMapSourceInfo(p.PilotInstanceID)
		if sourceErr == nil && source.RobotID == p.RobotID {
			item.Available = true
			item.Revision = source.Revision
			item.ReceivedAt = &source.ReceivedAt
		} else if sourceErr != nil && !errors.Is(sourceErr, store.ErrNotFound) {
			return result, sourceErr
		}
		result.Sources = append(result.Sources, item)
	}
	binding, err := s.st.GetPilotMapBinding(projectID)
	if err == nil {
		result.Binding = &binding
	} else if !errors.Is(err, store.ErrNotFound) {
		return result, err
	}
	return result, nil
}

// SelectMapSource is a map subscription, not a robot control or execution binding.
// An empty Pilot stops synchronization while keeping the last map readable.
func (s *Service) SelectMapSource(owner, projectID, pilotID string) (store.MapSnapshot, error) {
	s.mapMu.Lock()
	defer s.mapMu.Unlock()
	project, err := s.st.GetProject(projectID)
	if err != nil || project.OwnerID != owner {
		return store.MapSnapshot{}, ErrDirectRunScope
	}
	if pilotID == "" {
		err = s.st.SavePilotMapBinding(store.PilotMapBinding{ProjectID: projectID, SyncedAt: s.now().UTC()})
		if err != nil {
			return store.MapSnapshot{}, err
		}
		current, err := s.st.GetMapSnapshot(projectID, store.MapSlotReal, 0)
		if err == nil {
			s.publishPilotMap(current, "")
		}
		return current, err
	}
	p, err := s.mapPilot(owner, projectID, pilotID)
	if err != nil {
		return store.MapSnapshot{}, err
	}
	source, err := s.st.GetPilotMapSource(pilotID)
	if errors.Is(err, store.ErrNotFound) {
		return store.MapSnapshot{}, ErrMapSourceUnavailable
	}
	if err != nil {
		return store.MapSnapshot{}, err
	}
	if p.RobotID != source.RobotID {
		return store.MapSnapshot{}, ErrMapSourceUnavailable
	}
	return s.applyPilotSource(projectID, source)
}

func (s *Service) applyPilotSource(projectID string, source store.PilotMapSource) (store.MapSnapshot, error) {
	result, err := s.st.ApplyPilotMapSource(projectID, source, s.now().UTC())
	if errors.Is(err, store.ErrRevisionConflict) || errors.Is(err, store.ErrStaleMapGeneration) {
		result, err = s.st.ApplyPilotMapSource(projectID, source, s.now().UTC())
	}
	if err == nil {
		s.publishPilotMap(result, source.RobotID)
	}
	return result, err
}

func (s *Service) publishPilotMap(result store.MapSnapshot, robotID string) {
	s.events.PublishRobotEvent(result.Map.ProjectID, "semantic_map", store.MapSlotReal, "robot.map.synced", result.Map.Revision,
		map[string]any{"map_id": store.MapSlotReal, "generation": result.Map.Generation, "revision": result.Map.Revision, "robot_id": robotID})
}

// Pilots publish their configured local service. Legacy project_id only seeds a
// missing subscription; it can never replace an explicit selection or disconnect.
func (s *Service) SyncPilotMap(pilotID, legacyProjectID string, snapshot store.PilotMapSnapshot) (PilotMapReceipt, error) {
	s.mapMu.Lock()
	defer s.mapMu.Unlock()
	empty := PilotMapReceipt{}
	if !s.IsOnline(pilotID) {
		return empty, ErrPilotOffline
	}
	owner, err := s.st.PilotEnrollmentOwner(pilotID)
	if err != nil {
		return empty, ErrDirectRunScope
	}
	p, err := s.mapPilot(owner, "", pilotID)
	if err != nil {
		return empty, err
	}
	if legacyProjectID != "" {
		project, projectErr := s.st.GetProject(legacyProjectID)
		cfg := p.Configuration["map_sync"].(map[string]any)
		if projectErr != nil || project.OwnerID != owner || cfg["project_id"] != legacyProjectID {
			return empty, ErrDirectRunScope
		}
		if err = s.ValidateRobotProject(legacyProjectID, p.RobotID); err != nil {
			return empty, err
		}
	}
	source, err := s.st.SavePilotMapSource(pilotID, p.RobotID, snapshot, s.now().UTC())
	if err != nil {
		return empty, err
	}
	initialProject := ""
	if legacyProjectID != "" {
		_, err = s.st.GetPilotMapBinding(legacyProjectID)
		if errors.Is(err, store.ErrNotFound) {
			// The first projection also commits the initial subscription atomically.
			// An existing map from another source requires explicit selection.
			current, readErr := s.st.GetMapSnapshot(legacyProjectID, store.MapSlotReal, 0)
			if readErr != nil {
				return empty, readErr
			}
			for _, entity := range current.Entities {
				if entity.Status != store.EntityStatusRemoved && entity.Properties["pilot_robot_id"] != nil && entity.Properties["pilot_robot_id"] != p.RobotID {
					return empty, fmt.Errorf("legacy project already has a different map source: %w", store.ErrInvalidState)
				}
			}
			if _, err = s.applyPilotSource(legacyProjectID, source); err != nil {
				return empty, err
			}
			initialProject = legacyProjectID
		} else if err != nil {
			return empty, err
		}
	}
	projects, err := s.st.PilotMapProjects(pilotID)
	if err != nil {
		return empty, err
	}
	var failures []error
	for _, projectID := range projects {
		if projectID == initialProject {
			continue
		}
		project, projectErr := s.st.GetProject(projectID)
		if projectErr != nil || project.OwnerID != owner {
			failures = append(failures, ErrDirectRunScope)
			continue
		}
		_, err = s.mapPilot(owner, projectID, pilotID)
		if err == nil {
			_, err = s.applyPilotSource(projectID, source)
		}
		if err != nil {
			binding, readErr := s.st.GetPilotMapBinding(projectID)
			if readErr == nil {
				binding.LastError = err.Error()
				_ = s.st.SavePilotMapBinding(binding)
			}
			failures = append(failures, fmt.Errorf("project %s: %w", projectID, err))
		}
	}
	if len(failures) > 0 {
		return empty, errors.Join(failures...)
	}
	return PilotMapReceipt{MapID: store.MapSlotReal, Revision: source.Revision}, nil
}

func (s *Service) AuthorizeMapAsset(pilotID string) error {
	if pilotID == "" {
		return ErrDirectRunScope
	}
	owner, err := s.st.PilotEnrollmentOwner(pilotID)
	if err != nil {
		return ErrDirectRunScope
	}
	_, err = s.mapPilot(owner, "", pilotID)
	return err
}
