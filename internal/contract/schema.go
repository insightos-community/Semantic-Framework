// Package contract validates declared JSON contracts without changing values or
// executing tools. Business and live-state validators remain authoritative.
package contract

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type localOnlyLoader struct{}

func (localOnlyLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("contract must be self-contained; external reference rejected: %s", url)
}

// Compile supports local $defs/$ref but never fetches a URL or local file.
// Missing/malformed contracts are deployment errors, not model output errors.
func Compile(raw string) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid contract JSON: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(localOnlyLoader{})
	if err := c.AddResource("https://semantic.invalid/contract.json", doc); err != nil {
		return nil, err
	}
	return c.Compile("https://semantic.invalid/contract.json")
}

func CompileObject(schema map[string]any) (*jsonschema.Schema, error) {
	if len(schema) == 0 {
		return nil, fmt.Errorf("missing declared contract")
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	return Compile(string(raw))
}
