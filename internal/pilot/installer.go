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
	"bufio"
	"context"
	"fmt"
	"insightos.cn/semantic-framework/internal/ports/platform"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type PreparedSkillEnvironment struct {
	PythonExecutable string
	PythonPaths      []string
}

type SkillInstaller interface {
	Prepare(context.Context, SkillDefinition) (PreparedSkillEnvironment, error)
}

// VenvSkillInstaller 为 Skill 版本建立独立 Python 环境。SDKSource 指向包含
// semantic_robot_skill_sdk 的已固定源码目录或 wheel；生产环境应传发布制品而非临时分支。
// Wheelhouse 指向 bundle 的 wheels 目录；非空时 pip 完全离线解析，不读取宿主
// pip.conf 的 index-url。venv 不能再继承 venv 的 site-packages（--system-site-packages
// 只继承 base python），因此 bundle 固定依赖必须经 Wheelhouse 显式安装。
type VenvSkillInstaller struct {
	BaseDirectory    string
	PythonExecutable string
	SDKSource        string
	Wheelhouse       string
}

var lockedRequirement = regexp.MustCompile(`^[A-Za-z0-9_.-]+(?:\[[A-Za-z0-9_,.-]+\])?==[^[:space:]]+$`)

func (i VenvSkillInstaller) Prepare(ctx context.Context, definition SkillDefinition) (PreparedSkillEnvironment, error) {
	if i.BaseDirectory == "" || i.SDKSource == "" {
		return PreparedSkillEnvironment{}, fmt.Errorf("venv base directory and SDK source are required")
	}
	lockFile := filepath.Join(definition.Directory, "requirements.lock")
	if err := validateRequirementsLock(lockFile); err != nil {
		return PreparedSkillEnvironment{}, err
	}
	python := i.PythonExecutable
	if python == "" {
		python = "python"
	}
	environment := filepath.Join(i.BaseDirectory, definition.Name, definition.Version)
	venvPython := platform.VenvExecutable(environment, "python")
	ready := filepath.Join(environment, ".semantic-ready")
	if _, err := os.Stat(ready); err == nil {
		return PreparedSkillEnvironment{PythonExecutable: venvPython}, nil
	}
	if err := os.MkdirAll(filepath.Dir(environment), 0o755); err != nil {
		return PreparedSkillEnvironment{}, err
	}
	// bundle 的 python/venv 自身是 venv，--system-site-packages 只能继承其
	// base python 的 site-packages，继承不到 bundle 固定的依赖；真正的依赖
	// 复用靠 Wheelhouse 离线安装。Skill SDK 和 requirements.lock 仍安装在
	// 独立 venv 中，安装过程不访问公网，也不把具体 Skill 打进 Robot 类型包。
	if output, err := exec.CommandContext(ctx, python, "-m", "venv", "--system-site-packages", environment).CombinedOutput(); err != nil {
		return PreparedSkillEnvironment{}, fmt.Errorf("create skill venv: %w: %s", err, output)
	}
	if output, err := exec.CommandContext(ctx, venvPython, append([]string{"-m", "pip"}, i.installArgs(i.SDKSource)...)...).CombinedOutput(); err != nil {
		return PreparedSkillEnvironment{}, fmt.Errorf("install robot skill sdk: %w: %s", err, output)
	}
	if output, err := exec.CommandContext(ctx, venvPython, append([]string{"-m", "pip"}, i.installArgs("-r", lockFile)...)...).CombinedOutput(); err != nil {
		return PreparedSkillEnvironment{}, fmt.Errorf("install skill requirements: %w: %s", err, output)
	}
	if err := os.WriteFile(ready, []byte(definition.Name+"@"+definition.Version+"\n"), 0o644); err != nil {
		return PreparedSkillEnvironment{}, err
	}
	return PreparedSkillEnvironment{PythonExecutable: venvPython}, nil
}

// installArgs 构造 pip install 参数。Wheelhouse 非空时强制 --no-index：依赖只能
// 来自 bundle 固定 Wheel，缺失时立即失败，而不是回退到宿主 pip.conf 配置的源。
func (i VenvSkillInstaller) installArgs(target ...string) []string {
	args := []string{"install", "--disable-pip-version-check"}
	if i.Wheelhouse != "" {
		args = append(args, "--no-index", "--find-links", i.Wheelhouse)
	}
	return append(args, target...)
}

func validateRequirementsLock(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	count := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		count++
		if !lockedRequirement.MatchString(line) {
			return fmt.Errorf("requirement is not exactly pinned: %s", line)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("requirements.lock is empty")
	}
	return nil
}
