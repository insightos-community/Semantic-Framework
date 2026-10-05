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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// AbilityMockProcessConfig 只用于第一阶段组合测试。生产 Pilot 仍通过正式
// AbilityFramework 实例发现和调用接口工作，不启动这个进程。
type AbilityMockProcessConfig struct {
	PythonExecutable string
	AbilityRoot      string
	RobotSDKRoot     string
	RobotProfilePath string
	ModelProfilePath string
	ExecutionStore   string
	OnStderrLine     func(string)
}

// AbilityProcessClient 通过 JSON-RPC 调用测试进程中的 ability_py.task_manager、
// 正式 R1ProAbilityService 和 Fake Robot SDK。
type AbilityProcessClient struct {
	command *exec.Cmd
	stdin   io.WriteCloser

	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]chan rpcMessage
	notices   chan rpcMessage
	done      chan struct{}
	waitErrMu sync.RWMutex
	waitErr   error
	nextID    atomic.Uint64
}

func StartAbilityMockProcess(ctx context.Context, config AbilityMockProcessConfig) (*AbilityProcessClient, error) {
	python := config.PythonExecutable
	if python == "" {
		python = "python"
	}
	for name, value := range map[string]string{
		"Ability root":    config.AbilityRoot,
		"Robot SDK root":  config.RobotSDKRoot,
		"Robot profile":   config.RobotProfilePath,
		"Model profile":   config.ModelProfilePath,
		"Execution store": config.ExecutionStore,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("%s is required", name)
		}
	}
	command := exec.CommandContext(
		ctx,
		python,
		"-m", "mock_gateway",
		"--profile", config.RobotProfilePath,
		"--models", config.ModelProfilePath,
		"--store-dir", config.ExecutionStore,
	)
	// Mock Gateway只属于组合测试，生产 Wheel不再携带该入口。测试把 tests目录
	// 单独加入PYTHONPATH，仍复用正式Ability Service和Robot SDK实现。
	pythonPaths := append(robotSDKPythonPaths(config.RobotSDKRoot), config.AbilityRoot,
		filepath.Join(config.AbilityRoot, "tests"))
	if current := os.Getenv("PYTHONPATH"); current != "" {
		pythonPaths = append(pythonPaths, current)
	}
	command.Env = append(os.Environ(), "PYTHONPATH="+strings.Join(pythonPaths, string(os.PathListSeparator)))
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	client := &AbilityProcessClient{
		command: command,
		stdin:   stdin,
		pending: make(map[string]chan rpcMessage),
		notices: make(chan rpcMessage, 8),
		done:    make(chan struct{}),
	}
	go client.readStdout(stdout)
	go client.readStderr(stderr, config.OnStderrLine)
	go func() {
		err := command.Wait()
		client.waitErrMu.Lock()
		client.waitErr = err
		client.waitErrMu.Unlock()
		close(client.done)
	}()
	select {
	case notice := <-client.notices:
		if notice.Method != "ready" {
			_ = client.Kill()
			return nil, fmt.Errorf("ability process first message is %q, expected ready", notice.Method)
		}
		return client, nil
	case <-client.done:
		return nil, fmt.Errorf("ability process exited before ready: %w", client.exitError())
	case <-ctx.Done():
		_ = client.Kill()
		return nil, ctx.Err()
	}
}

func robotSDKPythonPaths(root string) []string {
	core := filepath.Join(root, "packages", "core", "src")
	r1pro := filepath.Join(root, "packages", "r1pro", "src")
	if coreInfo, coreErr := os.Stat(core); coreErr == nil && coreInfo.IsDir() {
		if r1Info, r1Err := os.Stat(r1pro); r1Err == nil && r1Info.IsDir() {
			return []string{core, r1pro}
		}
	}
	return []string{root}
}

func (p *AbilityProcessClient) StartTask(ctx context.Context, instanceID, taskName string, input map[string]any) (AbilityTask, error) {
	value, err := p.call(ctx, "ability.start", map[string]any{
		"instance_id": instanceID,
		"task_name":   taskName,
		"input":       input,
	})
	if err != nil {
		return AbilityTask{}, err
	}
	result, ok := value.(map[string]any)
	if !ok || stringValue(result["task_id"]) == "" {
		return AbilityTask{}, fmt.Errorf("ability.start returned invalid task id")
	}
	return AbilityTask{TaskID: stringValue(result["task_id"])}, nil
}

func (p *AbilityProcessClient) GetExecution(ctx context.Context, instanceID, invocationID string, afterSequence int64) (AbilityExecution, error) {
	value, err := p.call(ctx, "ability.get", map[string]any{
		"instance_id":    instanceID,
		"invocation_id":  invocationID,
		"after_sequence": afterSequence,
	})
	if err != nil {
		return AbilityExecution{}, err
	}
	return decodeAbilityExecution(value)
}

func (p *AbilityProcessClient) StopExecution(ctx context.Context, instanceID, invocationID, reason string) (AbilityExecution, error) {
	value, err := p.call(ctx, "ability.stop", map[string]any{
		"instance_id":   instanceID,
		"invocation_id": invocationID,
		"reason":        reason,
	})
	if err != nil {
		return AbilityExecution{}, err
	}
	return decodeAbilityExecution(value)
}

func (p *AbilityProcessClient) StartCounts(ctx context.Context) (map[string]int, error) {
	value, err := p.call(ctx, "ability.stats", map[string]any{})
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	result := make(map[string]int)
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (p *AbilityProcessClient) Close(ctx context.Context) error {
	_, err := p.call(ctx, "ability.shutdown", map[string]any{})
	_ = p.stdin.Close()
	return err
}

func (p *AbilityProcessClient) Kill() error {
	_ = p.stdin.Close()
	if p.command.Process == nil {
		return nil
	}
	return p.command.Process.Kill()
}

func (p *AbilityProcessClient) call(ctx context.Context, method string, params map[string]any) (any, error) {
	id := fmt.Sprintf("ability-%d", p.nextID.Add(1))
	response := make(chan rpcMessage, 1)
	p.pendingMu.Lock()
	p.pending[id] = response
	p.pendingMu.Unlock()
	defer func() {
		p.pendingMu.Lock()
		delete(p.pending, id)
		p.pendingMu.Unlock()
	}()
	if err := p.write(rpcMessage{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return nil, err
	}
	select {
	case message := <-response:
		if message.Error != nil {
			return nil, fmt.Errorf("ability %s failed: %v", method, message.Error["message"])
		}
		return message.Result, nil
	case <-p.done:
		return nil, p.exitError()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *AbilityProcessClient) write(message rpcMessage) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_, err = p.stdin.Write(append(encoded, '\n'))
	return err
}

func (p *AbilityProcessClient) readStdout(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var message rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil || message.JSONRPC != "2.0" {
			p.failPending(errors.New("ability stdout contains non JSON-RPC data"))
			_ = p.Kill()
			return
		}
		if message.Method != "" {
			p.notices <- message
			continue
		}
		id := fmt.Sprint(message.ID)
		p.pendingMu.Lock()
		target := p.pending[id]
		p.pendingMu.Unlock()
		if target == nil {
			p.failPending(fmt.Errorf("ability returned unknown response id %s", id))
			_ = p.Kill()
			return
		}
		target <- message
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		p.failPending(err)
	}
}

func (p *AbilityProcessClient) readStderr(reader io.Reader, sink func(string)) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		if sink != nil {
			sink(scanner.Text())
		}
	}
}

func (p *AbilityProcessClient) failPending(err error) {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	for _, target := range p.pending {
		select {
		case target <- rpcMessage{Error: map[string]any{"message": err.Error()}}:
		default:
		}
	}
}

func (p *AbilityProcessClient) exitError() error {
	p.waitErrMu.RLock()
	defer p.waitErrMu.RUnlock()
	if p.waitErr == nil {
		return errors.New("ability process exited")
	}
	return p.waitErr
}
