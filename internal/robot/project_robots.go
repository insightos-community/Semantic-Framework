package robot

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	"insightos.cn/semantic-framework/internal/robotruntime"
	"insightos.cn/semantic-framework/internal/store"
)

type ProjectRobotCatalog struct {
	ProjectID       string           `json:"project_id"`
	Robots          []map[string]any `json:"robots"`
	AvailableRobots []map[string]any `json:"available_robots"`
}

// Discovery reads persisted catalogs plus Gateway connectivity, never robot RPC.
func (s *Service) projectRobotOwned(owner, projectID, robotID string) (bool, error) {
	instance, err := s.st.GetLatestRuntimeByRobot(context.Background(), robotID)
	if err == nil && instance.ProjectID != "" {
		return instance.ProjectID == projectID, nil
	}
	if err != nil && !errors.Is(err, robotruntime.ErrInstanceNotFound) {
		return false, err
	}
	pilot, err := s.st.GetActiveRobotPilot(robotID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	enrolled, err := s.st.PilotEnrollmentOwner(pilot.PilotInstanceID)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && enrolled == owner, err
}

func (s *Service) ProjectRobots(owner, projectID string) (ProjectRobotCatalog, error) {
	result := ProjectRobotCatalog{ProjectID: projectID, Robots: []map[string]any{}, AvailableRobots: []map[string]any{}}
	project, err := s.st.GetProject(projectID)
	if err != nil || project.OwnerID != owner {
		return result, store.ErrNotFound
	}
	ids, err := s.st.ListProjectRobotIDs(projectID)
	if err != nil {
		return result, err
	}
	members := map[string]bool{}
	for _, id := range ids {
		members[id] = true
	}
	snapshot, err := s.DeviceSnapshot()
	if err != nil {
		return result, err
	}
	packages := map[string]store.RobotSkillPackage{}
	for _, p := range snapshot.SkillPackages {
		packages[p.Name+"@"+p.Version] = p
	}
	for _, device := range snapshot.Robots {
		id := stringValue(device["robot_id"])
		owned, err := s.projectRobotOwned(owner, projectID, id)
		if err != nil {
			return result, err
		}
		if !owned {
			continue
		}
		item := map[string]any{}
		for _, key := range []string{"robot_id", "display_name", "model", "backend", "environment", "status", "pilot", "ability_framework", "skill_catalog_revision"} {
			item[key] = device[key]
		}
		// Exact installed versions only; never mix the registry's simulator and
		// hardware variants, and never return host paths or device credentials.
		summaries := []map[string]any{}
		skills, _ := device["installed_skills"].([]store.RobotPilotSkill)
		for _, skill := range skills {
			p := packages[skill.Name+"@"+skill.Version]
			summaries = append(summaries, map[string]any{"name": skill.Name, "version": skill.Version, "description": p.Description, "enabled": skill.Enabled, "status": skill.Status})
		}
		item["skills"] = summaries
		if instance, ok := device["runtime_instance"].(*robotruntime.RuntimeInstance); ok && instance != nil {
			item["managed"] = instance.ProjectID == projectID
		}
		if instance, ok := device["runtime_instance"].(robotruntime.RuntimeInstance); ok {
			item["managed"] = instance.ProjectID == projectID
		}
		if members[id] {
			pilot, _ := device["pilot"].(map[string]any)
			localization, err := s.projectLocalization(projectID, stringValue(pilot["instance_id"]))
			if err != nil {
				return result, err
			}
			item["localization"] = localization
			result.Robots = append(result.Robots, item)
		} else {
			result.AvailableRobots = append(result.AvailableRobots, item)
		}
	}
	sort.Slice(result.Robots, func(i, j int) bool {
		return stringValue(result.Robots[i]["robot_id"]) < stringValue(result.Robots[j]["robot_id"])
	})
	return result, nil
}

func (s *Service) SetProjectRobot(owner, projectID, robotID string, added bool) error {
	project, err := s.st.GetProject(projectID)
	if err != nil || project.OwnerID != owner {
		return store.ErrNotFound
	}
	owned, err := s.projectRobotOwned(owner, projectID, robotID)
	if err != nil {
		return err
	}
	if !owned {
		return store.ErrNotFound
	}
	lock := s.robotAdmission(robotID)
	lock.Lock()
	defer lock.Unlock()
	if !added {
		instance, err := s.st.GetLatestRuntimeByRobot(context.Background(), robotID)
		if err == nil && instance.ProjectID == projectID {
			return store.ErrInvalidState
		}
		if err != nil && !errors.Is(err, robotruntime.ErrInstanceNotFound) {
			return err
		}
		busy, err := s.st.IsRobotTaskReserved(robotID, "")
		if err != nil {
			return err
		}
		if busy {
			return ErrRobotBusy
		}
		if _, err = s.st.GetRobotConversationRun(robotID); err == nil {
			return ErrRobotBusy
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		executions, err := s.st.ListRobotExecutions("", robotID, 200)
		if err != nil {
			return err
		}
		for _, item := range executions {
			if activeRobotStatus(item.Status) {
				return ErrRobotBusy
			}
		}
	}
	return s.st.SetProjectRobot(projectID, robotID, added)
}
