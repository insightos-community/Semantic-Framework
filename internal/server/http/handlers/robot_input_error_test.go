package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	robotdomain "insightos.cn/semantic-framework/internal/robot"
)

func TestRobotInputErrorIsCorrectableBeforeExecution(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeRobotError(recorder, fmt.Errorf("%w: plan.parameters.target: expected 4 joints", robotdomain.ErrSkillInputInvalid))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("input rejected before execution returned HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "ROBOT_SKILL_INPUT_INVALID" || !strings.Contains(body.Error.Message, "plan.parameters.target") {
		t.Fatalf("lost correctable field diagnostic: %s", recorder.Body.String())
	}
}
