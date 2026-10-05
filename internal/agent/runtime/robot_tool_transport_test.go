package runtime

import (
	"strings"
	"testing"
)

func TestRobotExecutionFeedbackSeparatesCurrentFailure(t *testing.T) {
	rejected := &purposeToolFailure{Tool: "robot.run", Code: "ROBOT_SKILL_INPUT_INVALID", Message: "invalid enum"}
	fieldFeedback := buildRobotExecutionFeedback(rejected)
	if !strings.Contains(fieldFeedback, "本次未建立 Execution 的工具错误：") || !strings.Contains(fieldFeedback, rejected.Code) {
		t.Fatal("current structured tool rejection was lost")
	}
	textFeedback := buildRobotExecutionFeedback(nil)
	if !strings.Contains(textFeedback, "ROBOT_TOOL_CALL_REQUIRED") || strings.Contains(textFeedback, rejected.Code) || strings.Contains(textFeedback, rejected.Message) {
		t.Fatal("previous field error leaked into text-only correction")
	}
	for _, feedback := range []string{fieldFeedback, textFeedback, robotToolTransportInstruction} {
		for _, required := range []string{"原生工具调用", `{"kind":"call","calls":[...]}`, "execution_id"} {
			if !strings.Contains(feedback, required) {
				t.Fatalf("transport contract missing %s", required)
			}
		}
	}
}
