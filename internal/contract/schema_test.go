package contract

import (
	"encoding/json"
	"testing"
)

func TestDeclaredSchemaRejectsInvalidValuesWithoutMutation(t *testing.T) {
	schema, err := Compile(`{"type":"object","required":["target"],"additionalProperties":false,
"properties":{"target":{"$ref":"#/$defs/Target"},"mode":{"enum":["auto","safe"],"default":"auto"}},
"$defs":{"Target":{"type":"object","required":["object_ref"],"properties":{
"object_ref":{"type":"string","minLength":1},"pose_hint":{"anyOf":[{"type":"null"},{"type":"object","required":["position_m"],"properties":{"position_m":{"type":"array","minItems":3,"maxItems":3,"items":{"type":"number"}}}}]}}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{`{"target":{"object_ref":"object-1"}}`, true},
		{`{"target":{"object_ref":"object-1","pose_hint":null}}`, true},
		{`{"target":{"object_ref":"object-1","pose_hint":{"position_m":[1,2,3]}}}`, true},
		{`{}`, false}, {`{"target":{}}`, false},
		{`{"target":{"object_ref":7}}`, false},
		{`{"target":{"object_ref":"object-1","pose_hint":{}}}`, false},
		{`{"target":{"object_ref":"object-1"},"mode":"invented"}`, false},
		{`{"target":{"object_ref":"object-1"},"unexpected":true}`, false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			var value any
			if err := json.Unmarshal([]byte(tc.raw), &value); err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(value)
			if err := schema.Validate(value); (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
			after, _ := json.Marshal(value)
			if string(before) != string(after) {
				t.Fatal("validator must not inject defaults or rewrite input")
			}
		})
	}
}

func TestContractRejectsExternalReferencesAndMissingSchema(t *testing.T) {
	for _, ref := range []string{"file:///etc/passwd", "https://example.com/schema.json"} {
		if _, err := Compile(`{"$ref":"` + ref + `"}`); err == nil {
			t.Fatal("external reference accepted")
		}
	}
	if _, err := CompileObject(nil); err == nil {
		t.Fatal("missing schema accepted")
	}
}
