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
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// 替换前在同一文件系统准备完整目录，旧目录移到父目录以外的归档区，
// 避免 Runtime/Scene 的递归目录扫描同时看见新旧两份配置。
func ReplaceDirectory(source, target string) error {
	return ReplaceDirectoryAndApply(source, target, func() error { return nil })
}

// 目录解析或服务刷新失败时恢复旧目录，失败包保留于隔离目录供排查。
func ReplaceDirectoryAndApply(source, target string, apply func() error) error {
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0750); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(parent), ".component-stage-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := os.CopyFS(staging, os.DirFS(source)); err != nil {
		return err
	}
	backup := ""
	if _, err := os.Stat(target); err == nil {
		archive := filepath.Join(filepath.Dir(parent), "installation-history")
		if err := os.MkdirAll(archive, 0700); err != nil {
			return err
		}
		backup = filepath.Join(archive, filepath.Base(target)+"-"+time.Now().UTC().Format("20060102T150405.000000000"))
		if err := os.Rename(target, backup); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(staging, target); err != nil {
		if backup != "" {
			if restoreErr := os.Rename(backup, target); restoreErr != nil {
				return fmt.Errorf("%v；恢复旧目录失败: %w", err, restoreErr)
			}
		}
		return err
	}
	if err := apply(); err != nil {
		failed := target + "-failed-" + time.Now().UTC().Format("20060102T150405.000000000")
		failed = filepath.Join(filepath.Dir(parent), filepath.Base(failed))
		if moveErr := os.Rename(target, failed); moveErr != nil {
			return fmt.Errorf("%v；隔离失败目录: %w", err, moveErr)
		}
		if backup != "" {
			if restoreErr := os.Rename(backup, target); restoreErr != nil {
				return fmt.Errorf("%v；恢复原目录: %w", err, restoreErr)
			}
		}
		return err
	}
	return nil
}
