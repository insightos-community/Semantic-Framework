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

package pilot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

type SkillRuntimeSpec struct {
	APIVersion     int               `yaml:"api_version"`
	Python         string            `yaml:"python"`
	Entrypoint     string            `yaml:"entrypoint"`
	StopEntrypoint string            `yaml:"stop_entrypoint"`
	InputModel     string            `yaml:"input_model"`
	StateModel     string            `yaml:"state_model"`
	ResultModel    string            `yaml:"result_model"`
	Controllers    map[string]string `yaml:"controllers"`
}

type SkillDefinition struct {
	Name            string           `yaml:"name"`
	Description     string           `yaml:"description"`
	Category        string           `yaml:"category"`
	Version         string           `yaml:"version"`
	Runtime         SkillRuntimeSpec `yaml:"runtime"`
	RequiredActions []ActionRef      `yaml:"required_actions"`
	StopActions     []ActionRef      `yaml:"stop_actions"`
	Directory       string           `yaml:"-"`
}

type SkillCatalog struct {
	mu     sync.RWMutex
	byName map[string]SkillDefinition
}

func ScanSkillCatalog(root string) (*SkillCatalog, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	catalog := &SkillCatalog{byName: make(map[string]SkillDefinition)}
	for _, entry := range entries {
		directory, err := activeSkillDirectory(root, entry)
		if err != nil {
			return nil, err
		}
		if directory == "" {
			continue
		}
		document := filepath.Join(directory, "SKILL.md")
		if _, err := os.Stat(document); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		definition, err := loadSkillDefinition(directory)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrSkillInvalid, directory, err)
		}
		if _, exists := catalog.byName[definition.Name]; exists {
			return nil, fmt.Errorf("%w: duplicate skill %s", ErrSkillInvalid, definition.Name)
		}
		catalog.byName[definition.Name] = definition
	}
	return catalog, nil
}

func (c *SkillCatalog) Resolve(name, version string) (SkillDefinition, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	definition, ok := c.byName[name]
	if !ok {
		return SkillDefinition{}, fmt.Errorf("%w: unknown skill %s", ErrSkillInvalid, name)
	}
	if version != "" && definition.Version != version {
		return SkillDefinition{}, fmt.Errorf("%w: skill %s version %s is unavailable", ErrSkillInvalid, name, version)
	}
	return definition, nil
}

func (c *SkillCatalog) Install(definition SkillDefinition) {
	c.mu.Lock()
	c.byName[definition.Name] = definition
	c.mu.Unlock()
}

func (c *SkillCatalog) Remove(name, version string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.byName[name]
	if !ok || current.Version != version {
		return ErrSkillInvalid
	}
	delete(c.byName, name)
	return nil
}

func (c *SkillCatalog) List() []SkillDefinition {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]SkillDefinition, 0, len(c.byName))
	for _, definition := range c.byName {
		result = append(result, definition)
	}
	return result
}

func LoadSkillDefinition(directory string) (SkillDefinition, error) {
	return loadSkillDefinition(directory)
}

func loadSkillDefinition(directory string) (SkillDefinition, error) {
	content, err := os.ReadFile(filepath.Join(directory, "SKILL.md"))
	if err != nil {
		return SkillDefinition{}, err
	}
	text := string(content)
	if !strings.HasPrefix(text, "---\n") {
		return SkillDefinition{}, fmt.Errorf("SKILL.md must start with YAML frontmatter")
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		return SkillDefinition{}, fmt.Errorf("SKILL.md frontmatter is not closed")
	}
	var definition SkillDefinition
	if err := yaml.Unmarshal([]byte(text[4:4+end]), &definition); err != nil {
		return SkillDefinition{}, err
	}
	definition.Directory = directory
	if definition.Name == "" || definition.Category != "robot_skill" || definition.Version == "" {
		return SkillDefinition{}, fmt.Errorf("name, category=robot_skill and version are required")
	}
	if definition.Runtime.APIVersion != 1 || definition.Runtime.Entrypoint == "" || definition.Runtime.StopEntrypoint == "" {
		return SkillDefinition{}, fmt.Errorf("runtime api_version=1, entrypoint and stop_entrypoint are required")
	}
	if definition.Runtime.InputModel == "" || definition.Runtime.StateModel == "" || definition.Runtime.ResultModel == "" {
		return SkillDefinition{}, fmt.Errorf("input, state and result models are required")
	}
	for _, reference := range []string{
		definition.Runtime.Entrypoint,
		definition.Runtime.StopEntrypoint,
		definition.Runtime.InputModel,
		definition.Runtime.StateModel,
		definition.Runtime.ResultModel,
	} {
		if err := validatePythonReference(directory, reference); err != nil {
			return SkillDefinition{}, err
		}
	}
	for _, reference := range definition.Runtime.Controllers {
		if err := validatePythonReference(directory, reference); err != nil {
			return SkillDefinition{}, err
		}
	}
	if len(definition.RequiredActions) == 0 || len(definition.StopActions) == 0 {
		return SkillDefinition{}, fmt.Errorf("required_actions and stop_actions cannot be empty")
	}
	seen := make(map[string]struct{})
	for _, action := range append(append([]ActionRef{}, definition.RequiredActions...), definition.StopActions...) {
		if action.Type == "" || action.SchemaVersion < 1 {
			return SkillDefinition{}, fmt.Errorf("action type and positive schema_version are required")
		}
		seen[action.Key()] = struct{}{}
	}
	if _, err := os.Stat(filepath.Join(directory, "requirements.lock")); err != nil {
		return SkillDefinition{}, fmt.Errorf("requirements.lock is required: %w", err)
	}
	return definition, nil
}

func validatePythonReference(directory, reference string) error {
	module, attribute, found := strings.Cut(reference, ":")
	if !found || module == "" || attribute == "" {
		return fmt.Errorf("invalid Python reference %q", reference)
	}
	path := filepath.Join(append([]string{directory}, strings.Split(module, ".")...)...) + ".py"
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("Python reference %q cannot be loaded: %w", reference, err)
	}
	return nil
}
