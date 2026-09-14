// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func Executable(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
func VenvExecutable(root, name string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(root, "Scripts", Executable(name))
	}
	return filepath.Join(root, "bin", name)
}
func Runnable(info os.FileInfo) bool {
	return !info.IsDir() && (runtime.GOOS == "windows" || info.Mode()&0111 != 0)
}
func RenderBackend() string {
	switch runtime.GOOS {
	case "windows":
		return "glfw"
	case "darwin":
		return "cgl"
	default:
		return "egl"
	}
}

func SamePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return a == b
}
