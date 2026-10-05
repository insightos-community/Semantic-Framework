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

package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino-ext/components/tool/commandline"
	"github.com/cloudwego/eino-ext/components/tool/commandline/sandbox"

	"insightos.cn/semantic-framework/internal/tool"
)

// fakeDockerSandbox 只模拟 Eino-ext 的最小生命周期，不在单元测试中依赖
// 开发机 Docker；真实容器能力由显式集成测试覆盖。
type fakeDockerSandbox struct {
	created   bool
	cleaned   bool
	createErr error
	command   []string
	output    *commandline.CommandOutput
	runErr    error
}

func (f *fakeDockerSandbox) Create(context.Context) error {
	f.created = true
	return f.createErr
}

func (f *fakeDockerSandbox) RunCommand(_ context.Context, command []string) (*commandline.CommandOutput, error) {
	f.command = append([]string(nil), command...)
	return f.output, f.runErr
}

func (f *fakeDockerSandbox) Cleanup(context.Context) {
	f.cleaned = true
}

// TestExecuteTool 验证 Project 挂载、相对工作目录、Shell 命令和结果结构均
// 原样交给 Eino-ext DockerSandbox，且成功后清理一次性容器。
func TestExecuteTool(t *testing.T) {
	workspace := t.TempDir()
	skillsRoot := t.TempDir()
	fake := &fakeDockerSandbox{output: &commandline.CommandOutput{
		Stdout: "ok\n", Stderr: "warn\n", ExitCode: 7,
	}}
	var gotConfig *sandbox.Config
	execute := &executeTool{newSandbox: func(_ context.Context, config *sandbox.Config) (dockerSandbox, error) {
		copy := *config
		gotConfig = &copy
		return fake, nil
	}}
	ctx := tool.WithExecutionScope(context.Background(), tool.ExecutionScope{
		SessionID: "session-1", ProjectID: "project-1", WorkspaceRoot: workspace,
		SkillsRoot: skillsRoot,
	})

	result, err := execute.Run(ctx, `{"command":"python script.py","workdir":"data","timeout_seconds":12}`)
	if err != nil {
		t.Fatalf("execute 执行失败: %v", err)
	}
	if !fake.created || !fake.cleaned {
		t.Fatalf("沙箱生命周期不完整: created=%v cleaned=%v", fake.created, fake.cleaned)
	}
	if !reflect.DeepEqual(fake.command, []string{"/bin/sh", "-lc", "python script.py"}) {
		t.Fatalf("Shell 命令不一致: %v", fake.command)
	}
	if gotConfig.Image != defaultDockerImage || gotConfig.WorkDir != "/workspace/data" ||
		gotConfig.Timeout != 12*time.Second || gotConfig.NetworkEnabled {
		t.Fatalf("Docker 配置不一致: %+v", gotConfig)
	}
	if gotConfig.VolumeBindings[workspace] != "/workspace" {
		t.Fatalf("Project workspace 挂载不一致: %+v", gotConfig.VolumeBindings)
	}
	if gotConfig.VolumeBindings[skillsRoot] != "/skills" {
		t.Fatalf("Skill 根目录挂载不一致: %+v", gotConfig.VolumeBindings)
	}
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Stdout   string `json:"stdout"`
			Stderr   string `json:"stderr"`
			ExitCode int    `json:"exit_code"`
			Workdir  string `json:"workdir"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(result), &envelope); err != nil {
		t.Fatalf("结果不是合法 JSON: %v", err)
	}
	if !envelope.OK || envelope.Data.Stdout != "ok\n" || envelope.Data.Stderr != "warn\n" ||
		envelope.Data.ExitCode != 7 || envelope.Data.Workdir != "data" {
		t.Fatalf("结构化结果不一致: %+v", envelope)
	}
	if got, err := filepath.Abs(filepath.Join(workspace, "data")); err != nil {
		t.Fatal(err)
	} else if _, _, resolveErr := resolveDockerWorkdir(workspace, "data"); resolveErr != nil || got == "" {
		t.Fatalf("工作目录应可解析: %v", resolveErr)
	}
}

// TestResolveDockerWorkdirAcceptsWorkspaceVirtualPath 验证执行工具与只读文件
// 工具使用同一个 /workspace 虚拟命名空间；模型无需在两类工具间改写路径。
func TestResolveDockerWorkdirAcceptsWorkspaceVirtualPath(t *testing.T) {
	root := t.TempDir()
	host, container, err := resolveDockerWorkdir(root, "/workspace/data")
	if err != nil {
		t.Fatalf("/workspace 虚拟路径应被接受: %v", err)
	}
	if host != filepath.Join(root, "data") || container != "/workspace/data" {
		t.Fatalf("虚拟路径解析不符: host=%q container=%q", host, container)
	}

	rootHost, rootContainer, err := resolveDockerWorkdir(root, "/workspace")
	if err != nil || rootHost != root || rootContainer != "/workspace" {
		t.Fatalf("虚拟根路径解析不符: host=%q container=%q err=%v", rootHost, rootContainer, err)
	}

	if _, _, err := resolveDockerWorkdir(root, "/workspace/../outside"); err == nil {
		t.Fatal("虚拟路径中的 .. 不得越出 Project workspace")
	}
	if _, _, err := resolveDockerWorkdir(root, "/etc"); err == nil {
		t.Fatal("非 /workspace 的绝对路径必须拒绝")
	}
}

// TestExecuteToolReportsMissingImage 验证 Eino-ext 固定镜像未安装时返回稳定、
// 可操作的错误提示，并且仍然清理创建失败的一次性沙箱。
func TestExecuteToolReportsMissingImage(t *testing.T) {
	fake := &fakeDockerSandbox{createErr: errors.New("Error response from daemon: No such image: python:3.9-slim")}
	execute := &executeTool{newSandbox: func(context.Context, *sandbox.Config) (dockerSandbox, error) {
		return fake, nil
	}}
	ctx := tool.WithExecutionScope(context.Background(), tool.ExecutionScope{
		SessionID: "session-1", ProjectID: "project-1", WorkspaceRoot: t.TempDir(),
	})

	_, err := execute.Run(ctx, `{"command":"python --version"}`)
	assertToolErrorCode(t, err, "DOCKER_IMAGE_MISSING")
	if !strings.Contains(err.Error(), "docker pull "+defaultDockerImage) {
		t.Fatalf("缺少镜像时应给出安装命令，实际: %v", err)
	}
	if !fake.cleaned {
		t.Fatal("镜像缺失时也必须清理本次沙箱")
	}
}

// TestExecuteToolRejectsInvalidScope 验证缺少会话作用域和目录越界时，工具在
// 创建容器前失败，避免模型借执行参数访问 Project 之外的宿主目录。
func TestExecuteToolRejectsInvalidScope(t *testing.T) {
	created := false
	execute := &executeTool{newSandbox: func(context.Context, *sandbox.Config) (dockerSandbox, error) {
		created = true
		return &fakeDockerSandbox{}, nil
	}}

	_, err := execute.Run(context.Background(), `{"command":"pwd"}`)
	assertToolErrorCode(t, err, "EXECUTION_SCOPE_MISSING")

	ctx := tool.WithExecutionScope(context.Background(), tool.ExecutionScope{
		SessionID: "session-1", ProjectID: "project-1", WorkspaceRoot: t.TempDir(),
	})
	_, err = execute.Run(ctx, `{"command":"pwd","workdir":"../outside"}`)
	assertToolErrorCode(t, err, "WORKDIR_OUTSIDE_PROJECT")
	if created {
		t.Fatal("非法作用域不应创建 Docker 沙箱")
	}
}

// TestExecuteToolCancellation 验证取消错误使用稳定错误码，并且仍清理容器。
func TestExecuteToolCancellation(t *testing.T) {
	fake := &fakeDockerSandbox{runErr: errors.New("context canceled")}
	execute := &executeTool{newSandbox: func(context.Context, *sandbox.Config) (dockerSandbox, error) {
		return fake, nil
	}}
	ctx, cancel := context.WithCancel(tool.WithExecutionScope(context.Background(), tool.ExecutionScope{
		SessionID: "session-1", ProjectID: "project-1", WorkspaceRoot: t.TempDir(),
	}))
	cancel()

	_, err := execute.Run(ctx, `{"command":"sleep 60"}`)
	assertToolErrorCode(t, err, "EXECUTION_CANCELLED")
	if !fake.cleaned {
		t.Fatal("取消后必须清理本次容器")
	}
}

// assertToolErrorCode 断言工具返回指定的结构化错误码。
func assertToolErrorCode(t *testing.T, err error, want string) {
	t.Helper()
	var toolErr *tool.Error
	if !errors.As(err, &toolErr) {
		t.Fatalf("应返回 *tool.Error，实际: %v", err)
	}
	if toolErr.Code != want {
		t.Fatalf("错误码应为 %q，实际: %q（%v）", want, toolErr.Code, toolErr)
	}
}

// TestExecuteToolDockerIntegration 使用真实 Docker 守护进程验证 Eino-ext
// DockerSandbox、工作区挂载和容器清理。普通单测默认跳过，CI 或本地验收通过
// SEMANTIC_DOCKER_INTEGRATION=1 显式开启，避免隐式拉取镜像。
func TestExecuteToolDockerIntegration(t *testing.T) {
	if os.Getenv("SEMANTIC_DOCKER_INTEGRATION") != "1" {
		t.Skip("未启用真实 Docker 集成测试")
	}
	image := os.Getenv("SEMANTIC_DOCKER_IMAGE")
	if image == "" {
		image = "python:3.9-slim"
	}
	workspace := t.TempDir()
	skillsRoot, err := filepath.Abs(filepath.Join("..", "..", "..", "configs", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	execute := &executeTool{newSandbox: func(ctx context.Context, config *sandbox.Config) (dockerSandbox, error) {
		config.Image = image
		return sandbox.NewDockerSandbox(ctx, config)
	}}
	ctx := tool.WithExecutionScope(context.Background(), tool.ExecutionScope{
		SessionID: "session-docker", ProjectID: "project-docker", WorkspaceRoot: workspace,
		SkillsRoot: skillsRoot,
	})
	// 真实验收显式使用模型可见的 /workspace 虚拟根目录，防止单元测试
	// 只覆盖相对路径、却遗漏线上最常见参数形态。
	result, err := execute.Run(ctx, `{"command":"printf 'docker-ok' > result.txt && cat result.txt","workdir":"/workspace","timeout_seconds":20}`)
	if err != nil {
		t.Fatalf("真实 Docker 执行失败: %v", err)
	}
	if !strings.Contains(result, "docker-ok") {
		t.Fatalf("真实 Docker 输出不符合预期: %s", result)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "result.txt"))
	if err != nil || string(content) != "docker-ok" {
		t.Fatalf("容器输出未写回 Project workspace: content=%q err=%v", content, err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "sample.csv"),
		[]byte("name,score\n甲,10\n乙,20\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	profileResult, err := execute.Run(ctx, `{"command":"python /skills/data/data-profile/scripts/profile.py --input sample.csv --json-output profile.json --markdown-output profile.md","workdir":"/workspace","timeout_seconds":20}`)
	if err != nil || !strings.Contains(profileResult, "row_count") {
		t.Fatalf("data-profile 未在真实 DockerSandbox 中成功运行: result=%s err=%v", profileResult, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "profile.json")); err != nil {
		t.Fatalf("Docker data-profile 未回写报告: %v", err)
	}
}
