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
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"insightos.cn/semantic-framework/pkg/log"
)

// fakeTool 是测试用的工具实现：脚本化输出/错误/阻塞时长，记录并发水位。
type fakeTool struct {
	// def 契约定义。
	def Definition

	// output 预设输出。
	output string

	// err 预设错误。
	err error

	// block 执行阻塞时长（超时测试用）。
	block time.Duration

	// current 当前并发执行数。
	current int32

	// maxConcurrent 历史最大并发数。
	maxConcurrent int32

	// sharedCurrent/sharedMax 可选的跨工具共享水位计数（串行测试用）。
	sharedCurrent *int32
	sharedMax     *int32
}

// Def 返回测试工具的契约。
func (f *fakeTool) Def() Definition { return f.def }

// Run 执行测试工具：记录并发水位，按预设阻塞/返回。
func (f *fakeTool) Run(ctx context.Context, _ string) (string, error) {
	current, maxConcurrent := &f.current, &f.maxConcurrent
	if f.sharedCurrent != nil {
		current, maxConcurrent = f.sharedCurrent, f.sharedMax
	}
	cur := atomic.AddInt32(current, 1)
	for {
		max := atomic.LoadInt32(maxConcurrent)
		if cur <= max || atomic.CompareAndSwapInt32(maxConcurrent, max, cur) {
			break
		}
	}
	defer atomic.AddInt32(current, -1)

	if f.block > 0 {
		select {
		case <-time.After(f.block):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return f.output, f.err
}

// maxConcurrent 返回测试工具的历史最大并发数。
func (f *fakeTool) peak() int32 { return atomic.LoadInt32(&f.maxConcurrent) }

// newFakeTool 创建一个测试工具（命名空间/风险/超时可配）。
func newFakeTool(name, namespace string, block time.Duration) *fakeTool {
	return &fakeTool{
		def: Definition{
			Name: name, Namespace: namespace, Description: "测试工具",
			ParametersJSON: `{"type":"object","properties":{}}`,
			Annotations:    Annotations{Risk: RiskLow},
		},
		output: `{"ok":true,"data":{}}`,
		block:  block,
	}
}

// newTestExecutor 创建测试执行器（串行命名空间可配）。
func newTestExecutor(t *testing.T, serial []string, tools ...Tool) (*Executor, *Registry) {
	t.Helper()
	reg := NewRegistry()
	for _, tool := range tools {
		if err := reg.Register(tool); err != nil {
			t.Fatalf("注册工具失败: %v", err)
		}
	}
	return NewExecutor(reg, ExecutorOptions{
		SerialNamespaces: serial,
		Logger:           log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
	}), reg
}

// TestRegistryRegisterAndQuery 验证注册表的注册、查重、列表与命名空间过滤。
func TestRegistryRegisterAndQuery(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(newFakeTool("system.echo", "system", 0)); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if err := reg.Register(newFakeTool("artifact.put", "artifact", 0)); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	// 重复注册报错。
	if err := reg.Register(newFakeTool("system.echo", "system", 0)); err == nil {
		t.Error("重复注册应报错")
	}
	// 契约不完整报错。
	bad := &fakeTool{def: Definition{Name: "x.y"}}
	if err := reg.Register(bad); err == nil {
		t.Error("缺 namespace/description 应报错")
	}

	if _, ok := reg.Get("system.echo"); !ok {
		t.Error("Get 应命中已注册工具")
	}
	if _, ok := reg.Get("bogus.tool"); ok {
		t.Error("Get 不应命中未注册工具")
	}

	defs := reg.List()
	if len(defs) != 2 || defs[0].Name != "artifact.put" || defs[1].Name != "system.echo" {
		t.Errorf("List 应按名排序返回全部，实际: %+v", defs)
	}
	byNs := reg.ByNamespace("system")
	if len(byNs) != 1 || byNs[0].Name != "system.echo" {
		t.Errorf("ByNamespace 不符: %+v", byNs)
	}
}

// TestNamespaceMatch 验证命名空间模式匹配（通配前缀与精确名）。
func TestNamespaceMatch(t *testing.T) {
	cases := []struct {
		patterns []string
		name     string
		want     bool
	}{
		{[]string{"artifact.*"}, "artifact.put", true},
		{[]string{"artifact.*"}, "artifact.get", true},
		{[]string{"artifact.*"}, "system.echo", false},
		{[]string{"system.*", "artifact.*"}, "system.calc", true},
		{[]string{"system.echo"}, "system.echo", true},
		{[]string{"system.echo"}, "system.time", false},
		{nil, "system.echo", false},
	}
	for _, c := range cases {
		if got := NamespaceMatch(c.patterns, c.name); got != c.want {
			t.Errorf("NamespaceMatch(%v, %q) = %v, 期望 %v", c.patterns, c.name, got, c.want)
		}
	}

	reg := NewRegistry()
	_ = reg.Register(newFakeTool("system.echo", "system", 0))
	_ = reg.Register(newFakeTool("system.time", "system", 0))
	_ = reg.Register(newFakeTool("artifact.put", "artifact", 0))
	matched := reg.MatchNamespaces([]string{"system.*"})
	if len(matched) != 2 {
		t.Errorf("MatchNamespaces(system.*) 应命中 2 个，实际: %+v", matched)
	}
}

// TestExecutorConcurrent 验证同轮多调用并发执行（无串行命名空间时）。
func TestExecutorConcurrent(t *testing.T) {
	a, b := newFakeTool("system.a", "system", 100*time.Millisecond), newFakeTool("device.x", "device", 100*time.Millisecond)
	exec, _ := newTestExecutor(t, nil, a, b)

	start := time.Now()
	results := exec.Execute(context.Background(), []Call{
		{Name: "system.a"}, {Name: "device.x"},
	})
	elapsed := time.Since(start)

	if len(results) != 2 || results[0].Name != "system.a" || results[1].Name != "device.x" {
		t.Fatalf("结果应按输入序返回，实际: %+v", results)
	}
	for _, r := range results {
		if !strings.Contains(r.Output, `"ok":true`) {
			t.Errorf("结果应为成功，实际: %s", r.Output)
		}
	}
	// 并发语义：两个 100ms 调用总耗时应远小于串行的 200ms。
	if elapsed >= 190*time.Millisecond {
		t.Errorf("两个调用应并发执行，耗时: %s", elapsed)
	}
	if a.peak() != 1 || b.peak() != 1 {
		t.Errorf("不同工具的并发水位应各为 1，实际: %d, %d", a.peak(), b.peak())
	}
}

// TestExecutorNamespaceSerial 验证串行命名空间的调用互斥。
func TestExecutorNamespaceSerial(t *testing.T) {
	a, b := newFakeTool("device.open", "device", 80*time.Millisecond), newFakeTool("device.close", "device", 80*time.Millisecond)
	// 跨工具共享水位：串行时同命名空间的全局并发数恒为 1。
	var sharedCurrent, sharedMax int32
	a.sharedCurrent, a.sharedMax = &sharedCurrent, &sharedMax
	b.sharedCurrent, b.sharedMax = &sharedCurrent, &sharedMax
	exec, _ := newTestExecutor(t, []string{"device"}, a, b)

	start := time.Now()
	exec.Execute(context.Background(), []Call{{Name: "device.open"}, {Name: "device.close"}})
	elapsed := time.Since(start)

	// 串行语义：两个 80ms 调用总耗时应接近 160ms。
	if elapsed < 150*time.Millisecond {
		t.Errorf("同命名空间调用应串行，耗时: %s", elapsed)
	}
	if got := atomic.LoadInt32(&sharedMax); got != 1 {
		t.Errorf("串行命名空间全局并发水位应为 1，实际: %d", got)
	}
}

// TestExecutorTimeout 验证 annotations.timeout 硬上限与结构化超时错误。
func TestExecutorTimeout(t *testing.T) {
	slow := newFakeTool("system.slow", "system", 5*time.Second)
	slow.def.Annotations.Timeout = 50 * time.Millisecond
	exec, _ := newTestExecutor(t, nil, slow)

	out, err := exec.ExecuteOne(context.Background(), "system.slow", `{}`)
	if err != nil {
		t.Fatalf("工具超时应返回结构化错误而非 Go 错误，实际: %v", err)
	}
	assertErrorJSON(t, out, CodeTimeout, true)
}

// TestExecutorErrorFormat 验证各类失败的结构化错误格式。
func TestExecutorErrorFormat(t *testing.T) {
	// 工具返回 *Error：code/retryable 原样透传。
	typed := newFakeTool("system.calc", "system", 0)
	typed.err = &Error{Code: "DIVIDE_BY_ZERO", Message: "除数不能为零", Retryable: false}
	// 工具返回普通 error：归一 TOOL_ERROR。
	generic := newFakeTool("system.boom", "system", 0)
	generic.err = errors.New("内部爆炸")
	exec, _ := newTestExecutor(t, nil, typed, generic)

	out, err := exec.ExecuteOne(context.Background(), "system.calc", `{}`)
	if err != nil {
		t.Fatalf("工具错误应返回结构化结果，实际 Go 错误: %v", err)
	}
	assertErrorJSON(t, out, "DIVIDE_BY_ZERO", false)

	out, _ = exec.ExecuteOne(context.Background(), "system.boom", `{}`)
	assertErrorJSON(t, out, CodeToolError, false)

	// 未注册工具。
	out, _ = exec.ExecuteOne(context.Background(), "ghost.tool", `{}`)
	assertErrorJSON(t, out, CodeUnknownTool, false)
}

// assertErrorJSON 断言输出是 {ok:false,error:{code,retryable}} 形态的结构化错误。
func assertErrorJSON(t *testing.T, out, wantCode string, wantRetryable bool) {
	t.Helper()
	var parsed struct {
		OK    bool `json:"ok"`
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("输出应为合法 JSON，实际: %q（%v）", out, err)
	}
	if parsed.OK {
		t.Errorf("应为失败结果，实际: %s", out)
	}
	if parsed.Error.Code != wantCode || parsed.Error.Retryable != wantRetryable || parsed.Error.Message == "" {
		t.Errorf("错误结构不符: got %+v, want code=%s retryable=%v", parsed.Error, wantCode, wantRetryable)
	}
}

// TestOKResult 验证成功结果的线上格式。
func TestOKResult(t *testing.T) {
	out, err := OKResult(map[string]any{"result": 42})
	if err != nil {
		t.Fatalf("OKResult 失败: %v", err)
	}
	var parsed struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil || !parsed.OK {
		t.Fatalf("成功结果格式不符: %q, %v", out, err)
	}
	if string(parsed.Data) != `{"result":42}` {
		t.Errorf("data 不符: %s", parsed.Data)
	}
}

// TestExecutorParentCancel 验证父 ctx 取消作为 Go 错误传播（运行级取消）。
func TestExecutorParentCancel(t *testing.T) {
	slow := newFakeTool("system.slow", "system", 5*time.Second)
	exec, _ := newTestExecutor(t, nil, slow)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	var runErr error
	go func() {
		defer wg.Done()
		_, runErr = exec.ExecuteOne(ctx, "system.slow", `{}`)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	wg.Wait()
	if !errors.Is(runErr, context.Canceled) {
		t.Errorf("父 ctx 取消应传播 context.Canceled，实际: %v", runErr)
	}
}
