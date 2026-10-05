package builtin

import (
	"fmt"
	"testing"

	robotdomain "insightos.cn/semantic-framework/internal/robot"
)

func TestSkillInputErrorIsCorrectableNotPhysicalRetry(t *testing.T) {
	err := robotToolError(fmt.Errorf("%w: target.pose_hint.position_m: Field required", robotdomain.ErrSkillInputInvalid))
	if err.Code != "ROBOT_SKILL_INPUT_INVALID" {
		t.Fatal(err)
	}
	if err.Retryable {
		t.Fatal("invalid arguments must not trigger automatic replay")
	}
}
