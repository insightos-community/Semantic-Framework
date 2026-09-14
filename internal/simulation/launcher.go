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

package simulation

import (
	"context"
	"errors"
	"fmt"
	processport "insightos.cn/semantic-framework/internal/ports/process"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// RuntimeProcess 是 Framework 按需启动后可停止和检查的进程。
type RuntimeProcess interface {
	Alive() bool
	Stop(context.Context) error
}

// RuntimeLauncher 在 Runtime 不可达时启动一个外部进程。
type RuntimeLauncher interface {
	Start(context.Context) (RuntimeProcess, error)
}

// ExecLauncher 不通过 shell 执行 Plugin，避免场景参数被解释为宿主命令。
type ExecLauncher struct {
	Command string
	Args    []string
	Dir     string
	Env     []string
}

func (l ExecLauncher) Start(_ context.Context) (RuntimeProcess, error) {
	if strings.TrimSpace(l.Command) == "" {
		return nil, fmt.Errorf("%w: 未配置 MuJoCo Runtime 启动命令", ErrRuntimeUnavailable)
	}
	// Runtime 配置允许像 `uv` 一样只填写可执行文件名，与 exec.Command 的既有
	// 语义一致，应当从 PATH 解析；同时 LookPath 也能校验显式配置的相对或绝对路径。
	// 不能直接 os.Stat(l.Command)，否则系统已经安装且可正常执行的 uv 会被误判为离线。
	resolvedCommand, err := exec.LookPath(l.Command)
	if err != nil {
		return nil, fmt.Errorf("%w: 启动 MuJoCo Runtime 失败: 入口不存在 %s（请在 mujoco-runtime 执行 uv sync --frozen --extra dev）",
			ErrRuntimeUnavailable, l.Command)
	}
	command := exec.Command(resolvedCommand, l.Args...)
	// Runtime 与 semantic-server 不能共享终端进程组。用户在前台按 Ctrl-C 时
	// 只能先唤醒 Server；Server 会依次让 Pilot/Ability 取得 hold 证据，最后
	// 再通过 RuntimeSupervisor.Shutdown 停止本进程。若 Runtime 同时收到终端
	// SIGINT，它会早于 Skill stop 退出，真实安全停止就只能得到连接拒绝。
	command.Dir = l.Dir
	command.Env = append(os.Environ(), l.Env...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	tree, err := processport.Start(command)
	if err != nil {
		return nil, fmt.Errorf("%w: 启动 MuJoCo Runtime 失败: %v", ErrRuntimeUnavailable, err)
	}
	process := &execRuntimeProcess{command: command, done: make(chan struct{}), tree: tree}
	go func() {
		err := command.Wait()
		process.mu.Lock()
		process.waitErr = err
		process.exited = true
		close(process.done)
		process.mu.Unlock()
	}()
	return process, nil
}

type execRuntimeProcess struct {
	command *exec.Cmd
	tree    *processport.Tree
	done    chan struct{}

	mu      sync.RWMutex
	exited  bool
	waitErr error
}

func (p *execRuntimeProcess) Alive() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return !p.exited && p.command.Process != nil
}

func (p *execRuntimeProcess) Stop(ctx context.Context) error {
	p.mu.Lock()
	if p.exited || p.command.Process == nil {
		err := p.waitErr
		p.mu.Unlock()
		return normalizeExit(err)
	}
	p.mu.Unlock()

	// ExecLauncher为受管Runtime建立独立进程组。uv只是父进程，真正的
	// plugin-mujoco在它的子进程中；只停止父进程会让Runtime继续占用端口。
	// 这里仅向Framework自己创建的进程组发送信号，外部共享Runtime不会走本路径。
	defer p.tree.Close()
	_ = p.tree.Interrupt()
	select {
	case <-p.done:
		p.mu.RLock()
		err := p.waitErr
		p.mu.RUnlock()
		return controlledRuntimeExit(err)
	case <-ctx.Done():
		_ = p.tree.Kill()
		select {
		case <-p.done:
			p.mu.RLock()
			err := p.waitErr
			p.mu.RUnlock()
			return controlledRuntimeExit(err)
		case <-time.After(time.Second):
			return ctx.Err()
		}
	}
}

func normalizeExit(err error) error {
	if err == nil {
		return nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && !exitError.Success() {
		// uv、Python 进程收到 SIGINT/SIGTERM 后可能把信号转换成 130/143，
		// ProcessState 此时不再标记为 Signaled，但仍属于 Framework 发起的受控停止。
		if exitError.ExitCode() == 130 || exitError.ExitCode() == 143 {
			return nil
		}

		// SIGINT/SIGTERM 是受控停止，不把它报告成 Runtime 崩溃。
		if status, ok := exitError.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return nil
		}
	}
	return err
}

func controlledRuntimeExit(err error) error {
	var exit *exec.ExitError
	if runtime.GOOS == "windows" && errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil
	}
	return normalizeExit(err)
}
