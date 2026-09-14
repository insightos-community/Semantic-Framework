//go:build !windows

// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
package pilot

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func activateSkill(base, name, version, root string) error {
	activeRoot := filepath.Join(base, "active")
	if err := os.MkdirAll(activeRoot, 0o750); err != nil {
		return err
	}
	link := filepath.Join(activeRoot, safePilotSegment(name))
	temp := link + ".next"
	_ = os.Remove(temp)
	if err := os.Symlink(root, temp); err != nil {
		return err
	}
	if err := os.Rename(temp, link); err != nil {
		if !errors.Is(err, os.ErrExist) {
			_ = os.Remove(temp)
			return err
		}
		_ = os.Remove(link)
		if err := os.Rename(temp, link); err != nil {
			return err
		}
	}
	return nil
}
func deactivateSkill(base, name, version string) error {
	link := filepath.Join(base, "active", safePilotSegment(name))
	if target, err := os.Readlink(link); err == nil && strings.Contains(target, safePilotSegment(version)) {
		return os.Remove(link)
	}
	return nil
}
func activeSkillDirectory(root string, entry os.DirEntry) (string, error) {
	path := filepath.Join(root, entry.Name())
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", nil
	}
	return path, nil
}
