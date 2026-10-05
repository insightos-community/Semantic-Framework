package pilot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/contract"
)

// Exercises the real installed Python model via the production Worker protocol.
// No SkillRuntime.Start, Agent, Stage or Action is called in this test.
func TestInstalledInputContractAndPydanticPreflight(t *testing.T) {
	python, sdk := os.Getenv("SEMANTIC_ROBOT_SKILL_PYTHON"), os.Getenv("SEMANTIC_ROBOT_SKILLS_DIR")
	if python == "" || sdk == "" {
		t.Skip("set SEMANTIC_ROBOT_SKILL_PYTHON and SEMANTIC_ROBOT_SKILLS_DIR")
	}
	skills, err := ScanSkillCatalog(filepath.Join(sdk, "semantic_robot_skills", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	runtime := NewSkillRuntime(skills, nil, nil, NewMemorySkillExecutionStore(), WorkerSupervisor{PythonExecutable: python, PythonPaths: []string{sdk}}, nil, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range []string{"semantic-navigation", "grasp-object", "place-object"} {
		definition, err := skills.Resolve(name, "")
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		described, err := runtime.DescribeInput(ctx, name, definition.Version)
		if err != nil {
			t.Fatal(err)
		}
		inputSchema, ok := described["input_schema"].(map[string]any)
		if !ok {
			t.Fatal("missing model schema")
		}
		validator, err := contract.CompileObject(inputSchema)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(inputSchema)
		t.Logf("contract %s@%s bytes=%d elapsed=%s", name, definition.Version, len(raw), time.Since(started))
		if name != "grasp-object" {
			continue
		}
		for _, tc := range []struct {
			name, input, errorField string
			valid                   bool
			schemaValid             bool
		}{
			{"minimal", `{"target":{"object_ref":"box"}}`, "", true, true},
			{"optional_null", `{"target":{"object_ref":"box","pose_hint":null}}`, "", true, true},
			{"valid_pose", `{"target":{"object_ref":"box","pose_hint":{"frame_id":"world","position_m":[0,0,0]}}}`, "", true, true},
			{"missing_target", `{}`, "target", false, false},
			{"missing_object", `{"target":{}}`, "object_ref", false, false},
			{"historical_missing_position", `{"target":{"object_ref":"box","pose_hint":{"frame_id":"world"}}}`, "position_m", false, false},
			{"wrong_enum", `{"target":{"object_ref":"box"},"preferred_strategy":"invented"}`, "preferred_strategy", false, false},
			{"wrong_type", `{"target":{"object_ref":true}}`, "object_ref", false, false},
			// A custom Python validator is stronger than structural JSON Schema.
			{"duplicate_tools", `{"target":{"object_ref":"box"},"tool_refs":["left","left"]}`, "不同工具", false, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var input map[string]any
				if err := json.Unmarshal([]byte(tc.input), &input); err != nil {
					t.Fatal(err)
				}
				before, _ := json.Marshal(input)
				if err := validator.Validate(input); (err == nil) != tc.schemaValid {
					t.Fatalf("schema result=%v", err)
				}
				checked, err := runtime.ValidateInput(ctx, name, definition.Version, input)
				if err != nil {
					t.Fatal(err)
				}
				if checked["valid"] != tc.valid {
					t.Fatalf("preflight=%v", checked)
				}
				if !tc.valid && !strings.Contains(stringValue(checked["error"]), tc.errorField) {
					t.Fatalf("missing field diagnostic: %v", checked)
				}
				after, _ := json.Marshal(input)
				if string(before) != string(after) {
					t.Fatal("caller input was mutated")
				}
			})
		}
	}
	if len(runtime.active) != 0 || len(runtime.activeRobots) != 0 {
		t.Fatal("contract/preflight started physical execution")
	}
}
