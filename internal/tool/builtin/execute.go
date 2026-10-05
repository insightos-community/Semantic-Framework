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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudwego/eino-ext/components/tool/commandline"
	"github.com/cloudwego/eino-ext/components/tool/commandline/sandbox"

	"insightos.cn/semantic-framework/internal/tool"
)

const (
	// nameExecute 是 Docker 沙箱执行工具的稳定名称。
	nameExecute = tool.ExecuteToolName

	// defaultExecuteTimeout 是模型未指定超时时采用的默认值。
	defaultExecuteTimeout = 30 * time.Second

	// maxExecuteTimeout 限制单次模型调用占用容器的最长时间。
	maxExecuteTimeout = 10 * time.Minute

	// dockerWorkspaceRoot 是 Project 工作区在容器内的固定挂载位置。
	dockerWorkspaceRoot = "/workspace"

	// defaultDockerImage 显式固定为当前 Eino-ext DockerSandbox 的默认镜像。
	// 上游组件不会自动拉取镜像，因此这里保留稳定名称供错误提示和安装检查使用。
	defaultDockerImage = "python:3.9-slim"
)

// dockerSandbox 抽象 Eino-ext DockerSandbox 的最小调用面，便于单元测试验证
// 参数和清理行为；生产实现仍直接使用上游组件，不复制容器生命周期逻辑。
type dockerSandbox interface {
	Create(ctx context.Context) error
	RunCommand(ctx context.Context, cmd []string) (*commandline.CommandOutput, error)
	Cleanup(ctx context.Context)
}

// dockerSandboxFactory 创建一次性 Docker 沙箱。每次调用独占一个容器，取消
// 或结束后都必须清理，不能在不同会话之间复用进程状态。
type dockerSandboxFactory func(ctx context.Context, config *sandbox.Config) (dockerSandbox, error)

// executeTool 使用 Eino-ext DockerSandbox 在当前 Project 工作区运行命令。
type executeTool struct {
	newSandbox dockerSandboxFactory
}

// newExecuteTool 创建生产用 execute 工具。
func newExecuteTool() *executeTool {
	return &executeTool{newSandbox: func(ctx context.Context, config *sandbox.Config) (dockerSandbox, error) {
		return sandbox.NewDockerSandbox(ctx, config)
	}}
}

// executeArgs 是 execute 对模型公开的参数。
type executeArgs struct {
	// Command 是交给容器 /bin/sh -lc 执行的命令。
	Command string `json:"command"`

	// Workdir 是当前 Project workspace 内的目录；既接受相对路径，也接受
	// Filesystem Middleware 对模型公开的 /workspace 虚拟路径。
	Workdir string `json:"workdir"`

	// TimeoutSeconds 是本次命令超时秒数，空值使用 30 秒。
	TimeoutSeconds int `json:"timeout_seconds"`
}

// Def 返回 execute 的模型工具契约。Docker 执行具有文件写入和进程副作用，
// 当前阶段先沿用既有高风险审批入口；后续执行模式会单独调整审批策略。
func (t *executeTool) Def() tool.Definition {
	return tool.Definition{
		Name:      nameExecute,
		Namespace: "execute",
		Description: "在隔离的 Docker 沙箱中执行命令。Project workspace 在容器内固定为 /workspace，" +
			"适合运行 Skill 脚本、分析数据和生成项目文件。",
		ParametersJSON: `{
			"type": "object",
			"properties": {
				"command": {"type": "string", "minLength": 1, "description": "要执行的 Shell 命令"},
				"workdir": {"type": "string", "description": "Project workspace 内的相对目录或 /workspace 虚拟路径；默认根目录"},
				"timeout_seconds": {"type": "integer", "minimum": 1, "maximum": 600, "description": "超时秒数，默认 30"}
			},
			"required": ["command"],
			"additionalProperties": false
		}`,
		Annotations: tool.Annotations{Risk: tool.RiskHigh, Timeout: maxExecuteTimeout},
	}
}

// Run 校验 Project 作用域后创建一次性容器，并把工作区挂载到 /workspace。
// DockerSandbox 与未来的宿主后端互不回退：Docker 不可用时直接返回错误。
func (t *executeTool) Run(ctx context.Context, argsJSON string) (string, error) {
	args, timeout, err := parseExecuteArgs(argsJSON)
	if err != nil {
		return "", err
	}
	scope, ok := tool.ExecutionScopeFromContext(ctx)
	if !ok {
		return "", &tool.Error{Code: "EXECUTION_SCOPE_MISSING", Message: "execute 只能在已绑定 Project 的会话中运行"}
	}

	hostWorkdir, containerWorkdir, err := resolveDockerWorkdir(scope.WorkspaceRoot, args.Workdir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(hostWorkdir, 0o700); err != nil {
		return "", &tool.Error{Code: "WORKDIR_CREATE_FAILED", Message: "创建 Project 工作目录失败: " + err.Error()}
	}

	bindings := map[string]string{scope.WorkspaceRoot: dockerWorkspaceRoot}
	if scope.SkillsRoot != "" {
		bindings[scope.SkillsRoot] = "/skills"
	}
	sb, err := t.newSandbox(ctx, &sandbox.Config{
		VolumeBindings: bindings,
		Image:          defaultDockerImage,
		WorkDir:        containerWorkdir,
		NetworkEnabled: false,
		Timeout:        timeout,
	})
	if err != nil {
		return "", &tool.Error{Code: "DOCKER_UNAVAILABLE", Message: "创建 Docker 沙箱失败: " + err.Error(), Retryable: true}
	}
	// 即使调用 context 已取消，也使用独立的短时 context 删除本次容器，避免
	// 取消后残留运行中的沙箱进程。
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sb.Cleanup(cleanupCtx)
	}()

	if err := sb.Create(ctx); err != nil {
		// DockerSandbox 不负责拉取镜像。首次安装缺少固定镜像时返回可操作的
		// 稳定错误，避免用户只能看到 Docker daemon 的底层英文信息。
		if strings.Contains(strings.ToLower(err.Error()), "no such image") {
			return "", &tool.Error{
				Code:    "DOCKER_IMAGE_MISSING",
				Message: "Docker 沙箱镜像未安装，请先执行 docker pull " + defaultDockerImage,
			}
		}
		return "", executionFailure(ctx, "DOCKER_CREATE_FAILED", "启动 Docker 沙箱失败", err)
	}
	output, err := sb.RunCommand(ctx, []string{"/bin/sh", "-lc", args.Command})
	if err != nil {
		return "", executionFailure(ctx, "EXECUTE_FAILED", "Docker 命令执行失败", err)
	}
	return tool.OKResult(map[string]any{
		"stdout":    output.Stdout,
		"stderr":    output.Stderr,
		"exit_code": output.ExitCode,
		"workdir":   args.Workdir,
	})
}

// parseExecuteArgs 解析并归一化 execute 参数。
func parseExecuteArgs(argsJSON string) (executeArgs, time.Duration, error) {
	var args executeArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return args, 0, &tool.Error{Code: "BAD_ARGUMENTS", Message: "参数不是合法 JSON: " + err.Error()}
	}
	args.Command = strings.TrimSpace(args.Command)
	if args.Command == "" {
		return args, 0, &tool.Error{Code: "EMPTY_COMMAND", Message: "command 不能为空"}
	}
	if args.TimeoutSeconds < 0 || args.TimeoutSeconds > int(maxExecuteTimeout/time.Second) {
		return args, 0, &tool.Error{Code: "INVALID_TIMEOUT", Message: "timeout_seconds 必须在 1 到 600 之间"}
	}
	timeout := defaultExecuteTimeout
	if args.TimeoutSeconds > 0 {
		timeout = time.Duration(args.TimeoutSeconds) * time.Second
	}
	return args, timeout, nil
}

// resolveDockerWorkdir 把模型给出的目录同时解析为宿主创建目录和容器工作
// 目录。允许 Filesystem Middleware 已向模型公开的 /workspace 虚拟根路径；
// 其他绝对路径和 .. 越界仍被拒绝，不能把任意宿主目录挂进容器。
func resolveDockerWorkdir(workspaceRoot, requested string) (string, string, error) {
	workspaceRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", "", &tool.Error{Code: "INVALID_WORKSPACE", Message: "Project workspace 无法解析: " + err.Error()}
	}
	requested = strings.TrimSpace(requested)
	// Agent 在只读文件工具中看到的是 /workspace。执行工具必须接受同一个
	// 虚拟命名空间，否则模型先用 ls /workspace 找到文件，随后把相同路径
	// 交给 execute 却会得到 WORKDIR_OUTSIDE_PROJECT。
	if requested == dockerWorkspaceRoot {
		requested = ""
	} else if strings.HasPrefix(requested, dockerWorkspaceRoot+"/") {
		requested = strings.TrimPrefix(requested, dockerWorkspaceRoot+"/")
	}
	requested = filepath.Clean(requested)
	if requested == "." {
		requested = ""
	}
	if filepath.IsAbs(requested) {
		return "", "", &tool.Error{Code: "WORKDIR_OUTSIDE_PROJECT", Message: "workdir 必须是 Project workspace 内的相对路径或 /workspace 虚拟路径"}
	}
	hostWorkdir := filepath.Join(workspaceRoot, requested)
	rel, err := filepath.Rel(workspaceRoot, hostWorkdir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", &tool.Error{Code: "WORKDIR_OUTSIDE_PROJECT", Message: "workdir 不能越出 Project workspace"}
	}
	containerWorkdir := dockerWorkspaceRoot
	if rel != "." {
		containerWorkdir = dockerWorkspaceRoot + "/" + filepath.ToSlash(rel)
	}
	return hostWorkdir, containerWorkdir, nil
}

// executionFailure 把取消、超时和普通 Docker 错误转换成稳定的工具错误码。
func executionFailure(ctx context.Context, defaultCode, prefix string, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return &tool.Error{Code: "EXECUTION_CANCELLED", Message: "Docker 命令已取消"}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "timed out") {
		return &tool.Error{Code: "EXECUTION_TIMEOUT", Message: "Docker 命令执行超时"}
	}
	return &tool.Error{Code: defaultCode, Message: fmt.Sprintf("%s: %v", prefix, err), Retryable: true}
}
