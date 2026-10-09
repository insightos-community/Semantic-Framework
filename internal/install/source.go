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
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// PackSkillSource 为一次源码更新建立不可变快照。摘要来自排序后的文件内容，
// 与源码修改时间、ZIP 时间戳无关；同一源码重复安装会命中同一开发版本。
// 不修改源码 SKILL.md，也不在 Server 执行构建脚本或导入包内 Python 模块。
func PackSkillSource(root string) ([]byte, error) {
	return packSkill(root, true)
}

// PackSkillRelease 保留源码声明版本，供交付构建与 Robot 模板版本对账。
// 发布库仍按原有规则拒绝同名同版本但内容不同的包。
func PackSkillRelease(root string) ([]byte, error) {
	return packSkill(root, false)
}

func packSkill(root string, snapshot bool) ([]byte, error) {
	files := map[string][]byte{}
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		if strings.HasPrefix(entry.Name(), ".") || entry.Name() == "__pycache__" || entry.Name() == "node_modules" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("源码包含非普通文件: %s", path)
		}
		if info.Size() > MaxPackageBytes-total || len(files) >= 4096 {
			return errors.New("源码超过安装包大小或文件数限制")
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(file, MaxPackageBytes-total+1))
		_ = file.Close()
		if err != nil {
			return err
		}
		total += int64(len(body))
		if total > MaxPackageBytes {
			return errors.New("源码超过 100 MiB")
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(name)] = body
		return nil
	})
	if err != nil {
		return nil, err
	}
	manifest := files["SKILL.md"]
	parts := bytes.SplitN(manifest, []byte("---"), 3)
	if len(parts) != 3 || len(bytes.TrimSpace(parts[0])) != 0 {
		return nil, errors.New("源码根目录需要有效的 Robot Skill SKILL.md")
	}
	var header map[string]any
	if err := yaml.Unmarshal(parts[1], &header); err != nil {
		return nil, err
	}
	version, _ := header["version"].(string)
	if header["category"] != "robot_skill" || version == "" {
		return nil, errors.New("源码安装要求 category: robot_skill 和 version")
	}
	archive, err := packFiles(files)
	if err != nil {
		return nil, err
	}
	if !snapshot {
		_, err = Inspect(archive)
		return archive, err
	}
	// 版本保留原版本号作为前缀，摘要区分尚未发布的本地源码修订。
	header["version"] = strings.SplitN(strings.SplitN(version, "+", 2)[0], "-dev.", 2)[0] + "-dev." + Digest(archive)[:16]
	body, err := yaml.Marshal(header)
	if err != nil {
		return nil, err
	}
	files["SKILL.md"] = []byte("---\n" + string(body) + "---" + string(parts[2]))
	archive, err = packFiles(files)
	if err != nil {
		return nil, err
	}
	_, err = Inspect(archive)
	return archive, err
}

func packFiles(files map[string][]byte) ([]byte, error) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var buffer bytes.Buffer
	z := zip.NewWriter(&buffer)
	for _, name := range names {
		writer, err := z.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := writer.Write(files[name]); err != nil {
			return nil, err
		}
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
