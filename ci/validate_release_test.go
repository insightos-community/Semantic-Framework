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

package ci_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestValidateReleaseRequiresExactRCSourceVersion 验证候选版标签不能与基础
// 版本或其他 rc 共用源码版本。测试直接执行正式发布脚本，避免另写一套规则。
func TestValidateReleaseRequiresExactRCSourceVersion(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位发布校验测试文件")
	}
	validator := filepath.Join(filepath.Dir(sourceFile), "validate-release.sh")

	tests := []struct {
		name       string
		tag        string
		version    string
		wantPass   bool
		wantOutput string
	}{
		{
			name:       "rc标签与源码版本完全一致",
			tag:        "v0.3.0-rc.1",
			version:    "0.3.0-rc.1",
			wantPass:   true,
			wantOutput: "发布标签校验通过：0.3.0-rc.1",
		},
		{
			name:       "rc标签不能复用基础源码版本",
			tag:        "v0.3.0-rc.1",
			version:    "0.3.0",
			wantOutput: "源码版本 0.3.0 与标签 0.3.0-rc.1 不一致",
		},
		{
			name:       "不同rc序号不能共用源码版本",
			tag:        "v0.3.0-rc.1",
			version:    "0.3.0-rc.2",
			wantOutput: "源码版本 0.3.0-rc.2 与标签 0.3.0-rc.1 不一致",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			versionDir := filepath.Join(workspace, "pkg", "version")
			if err := os.MkdirAll(versionDir, 0o755); err != nil {
				t.Fatalf("创建版本夹具目录失败: %v", err)
			}
			versionSource := "package version\n\nvar Version = \"" + test.version + "\"\n"
			if err := os.WriteFile(filepath.Join(versionDir, "version.go"), []byte(versionSource), 0o600); err != nil {
				t.Fatalf("写入源码版本夹具失败: %v", err)
			}
			changelog := "# Changelog\n\n## v0.3.0（开发中）\n"
			if err := os.WriteFile(filepath.Join(workspace, "CHANGELOG.md"), []byte(changelog), 0o600); err != nil {
				t.Fatalf("写入 Changelog 夹具失败: %v", err)
			}

			command := exec.Command("bash", validator)
			command.Dir = workspace
			command.Env = append(os.Environ(), "CI_COMMIT_TAG="+test.tag)
			output, err := command.CombinedOutput()
			if test.wantPass && err != nil {
				t.Fatalf("合法 rc 版本应通过，实际失败: %v\n%s", err, output)
			}
			if !test.wantPass && err == nil {
				t.Fatalf("不匹配的 rc 源码版本应失败，实际通过:\n%s", output)
			}
			if !strings.Contains(string(output), test.wantOutput) {
				t.Errorf("校验输出不符: want包含 %q, got %q", test.wantOutput, output)
			}
		})
	}
}
