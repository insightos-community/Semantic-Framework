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

package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"insightos.cn/semantic-framework/pkg/log"
)

// defaultExecutorTimeout 是工具未声明 annotations.timeout 时的默认硬上限。
const defaultExecutorTimeout = 30 * time.Second

// 执行器错误码取值（结构化错误的 code 字段，线上协议的一部分）。
const (
	// CodeUnknownTool 工具未注册。
	CodeUnknownTool = "UNKNOWN_TOOL"

	// CodeTimeout 执行超过超时硬上限（retryable=true，瞬态语义）。
	CodeTimeout = "TIMEOUT"

	// CodeToolError 工具返回的非结构化错误（不可重试，交回 Agent 决策）。
	CodeToolError = "TOOL_ERROR"
)

// Call 是一次待执行的工具调用。
type Call struct {
	// Name 工具全名。
	Name string

	// ArgsJSON 参数 JSON 文本。
	ArgsJSON string
}

// Result 是一次工具执行的结果（Output 恒为结构化 JSON 文本，
// 成功 {ok:true,...}，失败 {ok:false,error:{...}}）。
type Result struct {
	// Name 工具全名（与调用对应）。
	Name string

	// Output 结果 JSON 文本。
	Output string
}

// Executor 是并发安全的工具执行器（架构文档 05 §5）：
// 同轮多调用并发执行，命名空间可配置串行，超时硬上限，
// 一切失败归一为结构化错误结果。
type Executor struct {
	// registry 工具注册表（按名定位实现）。
	registry *Registry

	// defaultTimeout 未声明 annotations.timeout 时的默认硬上限。
	defaultTimeout time.Duration

	// serial 标记为串行的命名空间集合（同命名空间的调用互斥）。
	serial map[string]bool

	// nsMu 保护 nsLocks。
	nsMu sync.Mutex

	// nsLocks 串行命名空间的互斥锁（懒创建）。
	nsLocks map[string]*sync.Mutex

	// logger 结构化日志器。
	logger *log.Logger
}

// ExecutorOptions 是执行器的可选配置。
type ExecutorOptions struct {
	// DefaultTimeout 默认超时；≤0 时用 30s。
	DefaultTimeout time.Duration

	// SerialNamespaces 串行执行的命名空间（如对同一设备的写操作）。
	SerialNamespaces []string

	// Logger 结构化日志器；nil 时用丢弃日志器（测试便捷路径）。
	Logger *log.Logger
}

// NewExecutor 创建工具执行器。
func NewExecutor(registry *Registry, opts ExecutorOptions) *Executor {
	timeout := opts.DefaultTimeout
	if timeout <= 0 {
		timeout = defaultExecutorTimeout
	}
	serial := make(map[string]bool, len(opts.SerialNamespaces))
	for _, ns := range opts.SerialNamespaces {
		serial[ns] = true
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
	}
	return &Executor{
		registry:       registry,
		defaultTimeout: timeout,
		serial:         serial,
		nsLocks:        make(map[string]*sync.Mutex),
		logger:         logger,
	}
}

// Execute 并发执行同轮多个工具调用，结果按输入序返回。
// 同轮无依赖的调用并发是架构语义（05 §5）；命名空间串行约束由
// ExecuteOne 内部的命名空间锁保证，与批量并发正交。
func (e *Executor) Execute(ctx context.Context, calls []Call) []Result {
	results := make([]Result, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func(i int, c Call) {
			defer wg.Done()
			output, err := e.ExecuteOne(ctx, c.Name, c.ArgsJSON)
			if err != nil {
				// 父 ctx 取消也会走到这里：批量语义下归一为结构化错误，
				// 调用方（非内核路径）只面向结构化结果。
				output = ErrorResult(CodeToolError, err.Error(), false)
			}
			results[i] = Result{Name: c.Name, Output: output}
		}(i, c)
	}
	wg.Wait()
	return results
}

// ExecuteOne 执行单次工具调用（内核适配路径用）：
// 注册表定位 → 超时硬上限 → 命名空间锁 → 执行 → 结果归一。
// 返回的 Go error 仅表示父 ctx 取消/超时（运行级取消，应向上传播）；
// 工具自身的失败一律转换为结构化错误结果（string + nil error），
// 让模型读到机器可读的错误并自行修正（架构文档 05 §3）。
func (e *Executor) ExecuteOne(ctx context.Context, name, argsJSON string) (string, error) {
	if scope, ok := ExecutionScopeFromContext(ctx); ok &&
		(scope.RunKind == "task_planning" &&
			!PlanningToolAllowed(name) || scope.InteractionMode == "plan" &&
			!ConversationPlanToolAllowed(name)) {
		e.logger.Warn("只读规划工具调用被执行层拒绝", "tool", name,
			"run_kind", scope.RunKind, "interaction_mode", scope.InteractionMode)
		return ErrorResult(CodeToolError,
			fmt.Sprintf("Planning Run 不允许调用工具 %q", name), false), nil
	}
	t, ok := e.registry.Get(name)
	if !ok {
		e.logger.Warn("工具未注册", "tool", name)
		return ErrorResult(CodeUnknownTool, fmt.Sprintf("工具 %q 未注册", name), false), nil
	}
	def := t.Def()

	timeout := def.Annotations.Timeout
	if timeout <= 0 {
		timeout = e.defaultTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 命名空间串行：同命名空间的调用互斥（配置驱动，默认全并发）。
	unlock := e.lockNamespace(def.Namespace)
	defer unlock()

	e.logger.Info("工具调用开始", "tool", name, "namespace", def.Namespace, "timeout", timeout.String())
	start := time.Now()

	type outcome struct {
		out string
		err error
	}
	// 缓冲为 1：超时就绪返回后，迟到的工具结果仍能写入（goroutine 不泄漏）。
	// 工具实现必须尊重 ctx——超时返回后工具 goroutine 可能仍在运行，
	// 这是 Go 无法强制杀死 goroutine 的固有限制，v1 接受（内置工具都尊重 ctx）。
	ch := make(chan outcome, 1)
	go func() {
		out, err := t.Run(callCtx, argsJSON)
		ch <- outcome{out, err}
	}()

	select {
	case r := <-ch:
		if r.err == nil {
			e.logger.Info("工具调用完成", "tool", name, "duration_ms", time.Since(start).Milliseconds())
			return r.out, nil
		}
		if errors.Is(r.err, context.Canceled) {
			return "", r.err
		}
		var terr *Error
		if errors.As(r.err, &terr) {
			e.logger.Warn("工具调用失败", "tool", name, "code", terr.Code, "error", terr.Message, "retryable", terr.Retryable)
			return ErrorResult(terr.Code, terr.Message, terr.Retryable), nil
		}
		if errors.Is(r.err, context.DeadlineExceeded) {
			e.logger.Warn("工具调用超时", "tool", name, "timeout", timeout.String())
			return ErrorResult(CodeTimeout, fmt.Sprintf("工具执行超过 %s 硬上限", timeout), true), nil
		}
		e.logger.Warn("工具调用失败", "tool", name, "code", CodeToolError, "error", r.err.Error())
		return ErrorResult(CodeToolError, r.err.Error(), false), nil

	case <-callCtx.Done():
		if ctx.Err() != nil {
			// 父 ctx 取消/超时：运行级取消，作为 Go 错误传播。
			return "", ctx.Err()
		}
		e.logger.Warn("工具调用超时", "tool", name, "timeout", timeout.String())
		return ErrorResult(CodeTimeout, fmt.Sprintf("工具执行超过 %s 硬上限", timeout), true), nil
	}
}

// lockNamespace 对串行命名空间取锁，返回释放函数；非串行命名空间直接返回空操作。
func (e *Executor) lockNamespace(namespace string) func() {
	if !e.serial[namespace] {
		return func() {}
	}
	e.nsMu.Lock()
	mu, ok := e.nsLocks[namespace]
	if !ok {
		mu = &sync.Mutex{}
		e.nsLocks[namespace] = mu
	}
	e.nsMu.Unlock()

	mu.Lock()
	return mu.Unlock
}
