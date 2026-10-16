package runtime

import (
	"insightos.cn/semantic-framework/internal/store"
)

// A repaired plan must retain which earlier calls failed and where their
// inputs came from. Completed plans alone cannot identify failed targets.
// References are historical evidence, not a permanent prohibition on retry.
func (s *Service) failedSubTaskContexts(items []store.SubTask, taskID string) []map[string]any {
	result := make([]map[string]any, 0)
	for _, item := range items {
		if item.TaskID != taskID || item.Status != store.TaskStatusFailed ||
			item.Kind != "robot_skill" || item.ExecutionRef == "" {
			continue
		}
		execution, err := s.st.GetRobotExecution(item.ExecutionRef)
		if err != nil || execution.TaskID != taskID || execution.Status != "failed" {
			continue
		}
		result = append(result, failedSubTaskContext(item, execution))
	}
	return result
}

func failedSubTaskContext(item store.SubTask, execution store.RobotExecution) map[string]any {
	errorSummary := map[string]any{}
	for _, key := range []string{"code", "message", "retryable"} {
		if value, exists := execution.Error[key]; exists {
			errorSummary[key] = value
		}
	}
	// These small diagnostics are required to distinguish prerequisites,
	// manual cancellation and native failure. Dropping details made every
	// recovery reread the receipt and sometimes infer an old hardware fault.
	if details, ok := execution.Error["details"].(map[string]any); ok {
		selected := map[string]any{}
		for _, key := range []string{"readiness", "physical_started", "physical_dispatched",
			"layer", "timeout_s", "elapsed_s", "task_name", "native_status", "native_task_status",
			"native_detail", "planner_reason", "manual_cancel", "cancel_origin", "phase",
			"distance_m", "yaw_error_rad", "pose_age_s", "loc_age_s", "measured_arrival", "tolerance",
			"missing_fields", "field_sources"} {
			if value, exists := details[key]; exists {
				selected[key] = contextValue(value, 5)
			}
		}
		if len(selected) > 0 {
			errorSummary["details"] = selected
		}
	}
	return map[string]any{
		"subtask_id": item.ID, "goal": item.Goal, "status": item.Status,
		"execution_id": execution.ID, "skill_name": execution.SkillName,
		"skill_version": execution.SkillVersion, "error": errorSummary,
		"input_bindings": execution.InputBindings,
		"execution_lookup": map[string]any{"tool": "robot.get",
			"arguments": map[string]any{"execution_id": execution.ID}},
	}
}
