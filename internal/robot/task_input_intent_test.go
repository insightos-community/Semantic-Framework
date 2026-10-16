package robot

import (
	"encoding/json"
	"testing"
)

func TestTaskInputIntentKeepsLegacyAndMetadataSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, spec string
		input      map[string]any
	}{
		{"legacy", `{"skill_name":"one-operation"}`, map[string]any{"target": "table"}},
		{"unspecified", `{"intent":{"purpose":"place"}}`, map[string]any{"task": "plan"}},
		{"opaque", `{"intent":{"task":{"description":"move"}}}`, map[string]any{"task": "plan"}},
		{"no-discriminator", `{"intent":{"task":"execute"}}`, map[string]any{"target": "table"}},
		{"matching", `{"intent":{"task":"execute","phase":"approach"}}`, map[string]any{"task": "execute", "phase": "final"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateTaskInputIntent("step", json.RawMessage(tc.spec), tc.input); err != nil {
				t.Fatal(err)
			}
		})
	}
}
