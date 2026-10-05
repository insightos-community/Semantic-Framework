package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"insightos.cn/semantic-framework/internal/contract"
	"insightos.cn/semantic-framework/internal/store"
)

// Compile once before invoking the model. A missing/broken developer contract
// cannot be repaired by asking the model to invent a replacement schema.
func checkpointReviewer(event map[string]any, accept func(map[string]any)) (func(string) error, error) {
	declared, _ := event["response_schema"].(map[string]any)
	compiled, err := contract.CompileObject(declared)
	if err != nil {
		return nil, fmt.Errorf("checkpoint response contract unavailable: %w", err)
	}
	// Freeze declared state constraints before the model call. Malformed request
	// metadata is a developer contract error, not an output the model can repair.
	current := map[string]any{}
	if raw, exists := event["context"]; exists && raw != nil {
		encoded, err := json.Marshal(raw)
		if err != nil || json.Unmarshal(encoded, &current) != nil {
			return nil, fmt.Errorf("checkpoint context must be a JSON object")
		}
	}
	type selectionRule struct {
		key     string
		fields  []string
		allowed map[string]bool
	}
	rules := []selectionRule{}
	for _, definition := range []selectionRule{
		{key: "allowed_decisions", fields: []string{"decision", "choice", "action"}},
		{key: "allowed_actions", fields: []string{"action"}},
	} {
		raw, present := current[definition.key]
		if !present {
			continue
		}
		encoded, err := json.Marshal(raw)
		var allowed []string
		if err != nil || json.Unmarshal(encoded, &allowed) != nil || len(allowed) == 0 {
			return nil, fmt.Errorf("context.%s must be a nonempty string list", definition.key)
		}
		definition.allowed = map[string]bool{}
		for _, choice := range allowed {
			if strings.TrimSpace(choice) == "" {
				return nil, fmt.Errorf("context.%s contains an empty choice", definition.key)
			}
			definition.allowed[choice] = true
		}
		rules = append(rules, definition)
	}
	return func(text string) error {
		var value map[string]any
		if err := decodeStrictJSON(text, &value); err != nil {
			return fmt.Errorf("checkpoint reply must be a JSON object: %w", err)
		}
		if len(value) == 0 {
			return fmt.Errorf("checkpoint reply must not be empty")
		}
		if err := compiled.Validate(value); err != nil {
			return fmt.Errorf("checkpoint response_schema violation: %w", err)
		}
		for _, field := range []string{"python", "trajectory", "stage", "action_type"} {
			if _, exists := value[field]; exists {
				return fmt.Errorf("checkpoint reply contains forbidden field %q", field)
			}
		}
		for _, rule := range rules {
			// Validate declared restrictions only. No action is invented, selected
			// automatically, or renamed; simultaneous restrictions all apply.
			matched := false
			for _, field := range rule.fields {
				if selected, exists := value[field]; exists {
					choice, ok := selected.(string)
					if !ok || !rule.allowed[choice] {
						return fmt.Errorf("%s is not in context.%s", field, rule.key)
					}
					matched = true
				}
			}
			if !matched {
				return fmt.Errorf("%s has no declared decision selector in reply", rule.key)
			}
		}
		// Revision binding is a declared protocol constraint, not a guessed
		// robot-specific field. Never substitute the latest revision silently.
		if expected, exists := value["expected_plan_revision"]; exists {
			actual, found := current["plan_revision"]
			left, _ := json.Marshal(expected)
			right, _ := json.Marshal(actual)
			if !found || string(left) != string(right) {
				return fmt.Errorf("expected_plan_revision must match context.plan_revision")
			}
		}
		accept(value) // Publish only after every check; do not rewrite valid data.
		return nil
	}, nil
}

func validateTaskOutcome(text string) (taskOutcome, error) {
	var result taskOutcome
	if err := decodeStrictJSON(text, &result); err != nil {
		return result, fmt.Errorf("Worker Task result must be structured JSON: %w", err)
	}
	if result.Kind != "result" || strings.TrimSpace(result.Summary) == "" {
		return result, fmt.Errorf("Worker Task requires kind=result and nonempty summary")
	}
	if len(result.Evidence) != 0 && string(result.Evidence) != "null" {
		var value any
		if err := json.Unmarshal(result.Evidence, &value); err != nil {
			return result, err
		}
		switch value.(type) {
		case map[string]any, []any:
		default:
			return result, fmt.Errorf("Worker Task evidence must be an object or array")
		}
	}
	return result, nil
}

// Metadata only: raw prompts, replies and credentials never enter this metric.
// Existing native model-call records supply duration and tokens separately.
func (s *Service) recordContractCheck(run store.RunSession, purpose string, attempt int, start time.Time, problem error) {
	if s.logger != nil {
		s.logger.Info("agent.contract.check", "run_id", run.ID, "task_id", run.TaskID,
			"purpose", purpose, "attempt", attempt, "valid", problem == nil,
			"duration_ns", time.Since(start).Nanoseconds())
	}
}
