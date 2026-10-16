package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/workflow"
)

type failingSkillContract struct{ err error }

func (f failingSkillContract) DescribeSkillInput(context.Context, string, string, string) (map[string]any, error) {
	return nil, f.err
}

func TestOfflineContractReadIsResourceWaitNotBusinessFailure(t *testing.T) {
	for _, cause := range []error{robot.ErrPilotOffline,
		fmt.Errorf("read installed Skill contract: %w", robot.ErrPilotOffline),
		robot.ErrSkillUnavailable, errors.New("invalid installed Skill contract")} {
		svc := &Service{skillContracts: failingSkillContract{cause}}
		_, err := svc.withSkillInputContract(context.Background(), workflow.TaskExecution{
			Task: store.Task{AssignedRobotID: "robot"}, SubTasks: []store.SubTask{{
				Spec: json.RawMessage(`{"skill_name":"r1pro-manipulation","skill_version":"1"}`),
			}},
		}, "prompt")
		if errors.Is(err, workflow.ErrTaskWaitingResource) != errors.Is(cause, robot.ErrPilotOffline) {
			t.Fatalf("wrong failure classification: %v => %v", cause, err)
		}
		if !errors.Is(err, cause) {
			t.Fatalf("lost original error: %v", err)
		}
	}
}
