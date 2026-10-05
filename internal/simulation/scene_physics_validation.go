package simulation

import "math"

func validateScenePhysics(document SceneDocument) []ValidationIssue {
	issues := make([]ValidationIssue, 0)
	problem := func(field, message string) {
		issues = append(issues, ValidationIssue{Level: "error", Field: "physics." + field, Message: message})
	}

	for key := range document.Physics {
		switch key {
		case "gravity_m_s2", "timestep_seconds":
		default:
			problem(key, "不支持的场景物理参数 "+key)
		}
	}

	if value, ok := document.Physics["gravity_m_s2"]; ok &&
		!boundedNumberVector(value, 3, -math.MaxFloat64, math.MaxFloat64) {
		problem("gravity_m_s2", "gravity_m_s2 必须是三个有限数值")
	}
	if value, ok := document.Physics["timestep_seconds"]; ok {
		timestep, valid := numeric(value)
		if !valid || timestep <= 0 || timestep > 0.1 {
			problem("timestep_seconds", "物理步长必须大于 0 且不超过 0.1 秒")
		}
	}
	return issues
}
