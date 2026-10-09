// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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
