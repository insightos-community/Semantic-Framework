package robot

import (
	"context"
	"fmt"

	"insightos.cn/semantic-framework/internal/contract"
)

// DescribeSkillInput uses the currently bound Pilot and exact installed version.
// Failure is explicit: never substitute Registry latest or infer a schema from docs.
func (s *Service) DescribeSkillInput(ctx context.Context, robotID, name, version string) (map[string]any, error) {
	pilot, err := s.st.GetActiveRobotPilot(robotID)
	if err != nil {
		return nil, err
	}
	if !s.IsOnline(pilot.PilotInstanceID) {
		return nil, ErrPilotOffline
	}
	installed, err := s.st.GetRobotPilotSkill(pilot.PilotInstanceID, name, version)
	if err != nil || !installed.Enabled || installed.Status != "installed" {
		return nil, ErrSkillUnavailable
	}
	result, err := s.sendCommandAndWait(ctx, pilot.PilotInstanceID, "skill.describe_input",
		map[string]any{"name": name, "version": version})
	if err != nil {
		return nil, fmt.Errorf("read installed Skill contract: %w", err)
	}
	if result["name"] != name || result["version"] != version {
		return nil, fmt.Errorf("Skill contract identity mismatch")
	}
	schema, _ := result["input_schema"].(map[string]any)
	if _, err := contract.CompileObject(schema); err != nil {
		return nil, fmt.Errorf("invalid installed Skill contract: %w", err)
	}
	return schema, nil
}
