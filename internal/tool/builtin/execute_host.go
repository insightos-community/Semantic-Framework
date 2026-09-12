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
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cloudwego/eino/adk/filesystem"
	"github.com/google/uuid"

	"insightos.cn/semantic-framework/internal/tool"
)

// localExecutor 是 Eino-ext Local Backend 的最小执行面。
type localExecutor interface {
	Execute(ctx context.Context, input *filesystem.ExecuteRequest) (*filesystem.ExecuteResponse, error)
}

// localExecutorFactory 允许单元测试替换真实宿主命令，生产仍直接创建上游
// Local Backend，不复制其 Shell 执行实现。
type localExecutorFactory func(ctx context.Context) (localExecutor, error)

// executeHostTool 在当前 Project workspace 内调用 Eino-ext Local Backend。
type executeHostTool struct {
	newBackend localExecutorFactory
}

// newExecuteHostTool 创建生产用宿主执行工具。
func newExecuteHostTool() *executeHostTool {
	return &executeHostTool{newBackend: func(ctx context.Context) (localExecutor, error) {
		return newPlatformHostBackend(ctx)
	}}
}

// Def 返回 execute_host 的模型工具契约。该工具只有在服务端和当前会话
// 同时开启宿主权限时才会进入 Agent 工具集。
func (t *executeHostTool) Def() tool.Definition {
	return tool.Definition{
		Name:      tool.ExecuteHostToolName,
		Namespace: tool.ExecuteHostToolName,
		Description: "在当前 Project workspace 的宿主环境中执行命令。仅用于明确需要本机工具链的 Skill；" +
			"workdir 可使用相对路径或 /workspace 虚拟路径，不会自动降级到 Docker。",
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

// Run 在再次校验 Server 与会话双开关后执行宿主命令。权限缺失直接拒绝，
// 不回退到 Docker execute，也不会把命令改交其他后端。
func (t *executeHostTool) Run(ctx context.Context, argsJSON string) (string, error) {
	args, timeout, err := parseExecuteArgs(argsJSON)
	if err != nil {
		return "", err
	}
	scope, ok := tool.ExecutionScopeFromContext(ctx)
	if !ok {
		return "", &tool.Error{Code: "EXECUTION_SCOPE_MISSING", Message: "execute_host 只能在已绑定 Project 的会话中运行"}
	}
	if !scope.HostExecutionAllowed || !scope.HostExecutionEnabled {
		return "", &tool.Error{Code: "HOST_EXECUTION_DISABLED", Message: "Server 或当前会话未开启宿主执行"}
	}
	hostWorkdir, err := resolveHostWorkdir(scope.WorkspaceRoot, args.Workdir)
	if err != nil {
		return "", err
	}

	backend, err := t.newBackend(ctx)
	if err != nil {
		return "", &tool.Error{Code: "HOST_BACKEND_UNAVAILABLE", Message: "创建 Eino Local Backend 失败: " + err.Error()}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Local Backend 没有 workdir 与进程组句柄，因此用 setsid 建立本次命令
	// 专属进程组，并把组长 PID 写入 Project 内的临时文件。取消时只补充一次
	// 进程组 kill，实际命令创建和等待仍由上游 Local Backend 完成。
	pidFile := filepath.Join(hostWorkdir, ".semantic-exec-"+uuid.NewString()+".pid")
	defer func() { _ = os.Remove(pidFile) }()
	innerCommand := "echo $$ > " + shellSingleQuote(pidFile) +
		" && exec /bin/sh -c " + shellSingleQuote(args.Command)
	command := "cd -- " + shellSingleQuote(hostWorkdir) +
		" && " + hostSessionCommand() + shellSingleQuote(innerCommand)
	type backendResult struct {
		response *filesystem.ExecuteResponse
		err      error
	}
	resultCh := make(chan backendResult, 1)
	go func() {
		response, executeErr := backend.Execute(runCtx, &filesystem.ExecuteRequest{Command: command})
		resultCh <- backendResult{response: response, err: executeErr}
	}()

	var result backendResult
	select {
	case result = <-resultCh:
	case <-runCtx.Done():
		killHostProcessGroup(pidFile)
		// 上游在进程组退出后应立即完成 Wait；有限等待避免异常后端永久
		// 阻塞 Agent Run，同时缓冲 channel 保证 goroutine 可以退出。
		select {
		case <-resultCh:
		case <-time.After(2 * time.Second):
		}
		return "", hostExecutionFailure(runCtx, runCtx.Err())
	}
	if result.err != nil {
		return "", hostExecutionFailure(runCtx, result.err)
	}
	response := result.response
	exitCode := 0
	if response.ExitCode != nil {
		exitCode = *response.ExitCode
	}
	return tool.OKResult(map[string]any{
		"output": response.Output, "exit_code": exitCode,
		"truncated": response.Truncated, "workdir": args.Workdir,
	})
}

// killHostProcessGroup 读取本次命令的组长 PID 并终止整个进程组。PID 文件
// 尚未生成表示命令还没进入用户 Shell，此时上游 CommandContext 已足够取消。
func killHostProcessGroup(pidFile string) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// resolveHostWorkdir 同时进行词法和符号链接校验。宿主执行与容器不同，
// workspace 内指向外部的符号链接必须拒绝，否则相对路径仍可能逃逸。
func resolveHostWorkdir(workspaceRoot, requested string) (string, error) {
	hostWorkdir, _, err := resolveDockerWorkdir(workspaceRoot, requested)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(hostWorkdir, 0o700); err != nil {
		return "", &tool.Error{Code: "WORKDIR_CREATE_FAILED", Message: "创建 Project 工作目录失败: " + err.Error()}
	}
	realRoot, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return "", &tool.Error{Code: "INVALID_WORKSPACE", Message: "解析 Project workspace 失败: " + err.Error()}
	}
	realWorkdir, err := filepath.EvalSymlinks(hostWorkdir)
	if err != nil {
		return "", &tool.Error{Code: "INVALID_WORKDIR", Message: "解析 workdir 失败: " + err.Error()}
	}
	rel, err := filepath.Rel(realRoot, realWorkdir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", &tool.Error{Code: "WORKDIR_OUTSIDE_PROJECT", Message: "workdir 不能通过符号链接越出 Project workspace"}
	}
	return realWorkdir, nil
}

// shellSingleQuote 把可信路径编码为 POSIX Shell 单引号参数，目录中的空格、
// 引号和元字符不会改变 cd 命令结构。
func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

// hostExecutionFailure 归一化 Local Backend 的取消、超时和普通错误。
func hostExecutionFailure(ctx context.Context, err error) error {
	if ctx.Err() == context.Canceled {
		return &tool.Error{Code: "EXECUTION_CANCELLED", Message: "宿主命令已取消"}
	}
	if ctx.Err() == context.DeadlineExceeded {
		return &tool.Error{Code: "EXECUTION_TIMEOUT", Message: "宿主命令执行超时"}
	}
	return &tool.Error{Code: "HOST_EXECUTE_FAILED", Message: fmt.Sprintf("宿主命令执行失败: %v", err), Retryable: false}
}
