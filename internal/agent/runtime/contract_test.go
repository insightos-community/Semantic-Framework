package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/workflow"
)

type testSkillContracts struct{}

func (testSkillContracts) DescribeSkillInput(context.Context, string, string, string) (map[string]any, error) {
	return map[string]any{"type": "object", "required": []any{"target"}, "properties": map[string]any{"target": map[string]any{"type": "string"}}}, nil
}

func TestSkillContractAddedBeforeModelWithoutChangingIntent(t *testing.T) {
	execution := workflow.TaskExecution{Task: store.Task{AssignedRobotID: "other-robot"},
		SubTasks: []store.SubTask{{Spec: json.RawMessage(`{"skill_name":"other-skill","skill_version":"2.0","intent":{"stable":"unchanged"}}`)}}}
	svc := &Service{skillContracts: testSkillContracts{}}
	before := string(execution.SubTasks[0].Spec)
	prompt, err := svc.withSkillInputContract(context.Background(), execution, "original")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"original", `"input_schema"`, `"required":["target"]`, `"skill_version":"2.0"`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %s", want)
		}
	}
	if string(execution.SubTasks[0].Spec) != before {
		t.Fatal("planning intent was rewritten")
	}
	svc.skillContracts = nil
	if _, err := svc.withSkillInputContract(context.Background(), execution, "original"); err == nil {
		t.Fatal("missing provider must fail closed")
	}
}
