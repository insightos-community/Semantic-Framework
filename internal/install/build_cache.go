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

package install

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func sourceFingerprint(source string, recipe SourceBuild) (string, error) {
	roots := []string{source}
	for _, path := range recipe.Build.PythonProjects {
		roots = append(roots, filepath.Join(source, path))
	}
	for _, path := range recipe.Build.Files {
		roots = append(roots, filepath.Join(source, path))
	}
	if recipe.Build.RequirementsLock != "" {
		roots = append(roots, filepath.Join(source, recipe.Build.RequirementsLock))
	}
	for _, path := range recipe.Build.AbilityDirectories {
		roots = append(roots, filepath.Join(source, path))
	}
	sort.Strings(roots)
	hash := sha256.New()
	// 缓存身份含构建格式版本；构建规则变化时递增，避免命中旧的构建语义。
	_, _ = io.WriteString(hash, "semantic-component-build-v1\n")
	for index, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path != root && skipBuildEntry(entry.Name()) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("构建输入不是普通文件: %s", path)
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(hash, "%d:%s:%d:%d\n", index, filepath.ToSlash(relative), info.Mode().Perm(), info.Size())
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(hash, file)
			_ = file.Close()
			return err
		})
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func skipBuildEntry(name string) bool {
	return strings.HasPrefix(name, ".") || name == "__pycache__" || name == "node_modules" || name == "build" || name == "dist" || strings.HasSuffix(name, ".egg-info")
}
