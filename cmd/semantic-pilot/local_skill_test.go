package main

import (
	"strings"
	"testing"
)

func TestSplitSkillReference(t *testing.T) {
	name, version, err := splitSkillReference("grasp-object@0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if name != "grasp-object" || version != "0.2.0" {
		t.Fatalf("unexpected reference %q %q", name, version)
	}
	if _, _, err := splitSkillReference("grasp-object"); err == nil {
		t.Fatal("reference without an exact version must be rejected")
	}
}

func TestReadJSONObject(t *testing.T) {
	value, err := readJSONObject("-", strings.NewReader(`{"target":{"object_ref":"tote-large-smoke"}}`))
	if err != nil {
		t.Fatal(err)
	}
	target, ok := value["target"].(map[string]any)
	if !ok || target["object_ref"] != "tote-large-smoke" {
		t.Fatalf("unexpected input: %#v", value)
	}
}
