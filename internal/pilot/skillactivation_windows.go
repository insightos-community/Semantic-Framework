// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
package pilot

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Windows active entries are small version references, atomically replaced on
// the same volume. They require neither symlink privilege nor Developer Mode.
func activateSkill(base, name, version, root string) error {
	active := filepath.Join(base, "active")
	if err := os.MkdirAll(active, 0o750); err != nil {
		return err
	}
	data, err := json.Marshal(version)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(base, ".activate-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	_, writeErr := temp.Write(data)
	closeErr := temp.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temp.Name(), filepath.Join(active, safePilotSegment(name)+".skill-ref"))
}
func deactivateSkill(base, name, version string) error {
	path := filepath.Join(base, "active", safePilotSegment(name)+".skill-ref")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var current string
	if err := json.Unmarshal(data, &current); err != nil {
		return err
	}
	if current != version {
		return nil
	}
	return os.Remove(path)
}
func activeSkillDirectory(root string, entry os.DirEntry) (string, error) {
	path := filepath.Join(root, entry.Name())
	if entry.IsDir() {
		return path, nil
	}
	if !strings.HasSuffix(entry.Name(), ".skill-ref") {
		return "", nil
	}
	name := strings.TrimSuffix(entry.Name(), ".skill-ref")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var version string
	if err := json.Unmarshal(data, &version); err != nil {
		return "", err
	}
	if !validSkillReferenceSegment(name) || !validSkillReferenceSegment(version) {
		return "", ErrSkillInvalid
	}
	return filepath.Join(filepath.Dir(root), "packages", name, version), nil
}

func validSkillReferenceSegment(value string) bool {
	return value != "" && value != "." && value != ".." && value == safePilotSegment(value) && !strings.Contains(value, ":") && !strings.HasSuffix(value, ".") && !strings.HasSuffix(value, " ")
}
