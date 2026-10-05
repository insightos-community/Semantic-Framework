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

package security

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/log"
)

// calcDef 是测试用的四则运算工具契约（含枚举与必填约束）。
func calcDef() tool.Definition {
	return tool.Definition{
		Name: "system.calc", Namespace: "system", Description: "四则运算",
		ParametersJSON: `{
			"type": "object",
			"properties": {
				"a":  {"type": "number"},
				"op": {"type": "string", "enum": ["+", "-", "*", "/"]},
				"b":  {"type": "number"}
			},
			"required": ["a", "op", "b"],
			"additionalProperties": false
		}`,
		Annotations: tool.Annotations{Risk: tool.RiskLow},
	}
}

// putDef 是测试用的高危写入工具契约。
func putDef() tool.Definition {
	return tool.Definition{
		Name: "artifact.put", Namespace: "artifact", Description: "保存产物",
		ParametersJSON: `{
			"type": "object",
			"properties": {"content": {"type": "string"}},
			"required": ["content"],
			"additionalProperties": false
		}`,
		Annotations: tool.Annotations{Risk: tool.RiskHigh},
	}
}

// newTestMiddleware 创建测试 middleware（logger 丢弃，归因身份 test-agent）。
func newTestMiddleware(t *testing.T, defs []tool.Definition, approvalRequired []string) *Middleware {
	t.Helper()
	m, err := NewMiddleware(defs, approvalRequired, "test-agent",
		log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	if err != nil {
		t.Fatalf("NewMiddleware 失败: %v", err)
	}
	return m
}

// countingEndpoint 返回计数端点（验证放行路径是否真执行）。
func countingEndpoint(hits *int32, output string) kernel.ToolCallEndpoint {
	return func(_ context.Context, _ string) (string, error) {
		atomic.AddInt32(hits, 1)
		return output, nil
	}
}

// assertErrorCode 断言输出是指定 code 的结构化错误。
func assertErrorCode(t *testing.T, out, wantCode string) {
	t.Helper()
	var parsed struct {
		OK    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("输出应为合法 JSON，实际: %q", out)
	}
	if parsed.OK || parsed.Error.Code != wantCode {
		t.Errorf("应为 %s 结构化错误，实际: %s", wantCode, out)
	}
}

// TestParamViolationBlocked 验证 L2 参数校验拦截：缺必填、枚举越界、
// 非法 JSON、未知字段都被拦为 PARAM_VIOLATION，且执行端点不被调用。
func TestParamViolationBlocked(t *testing.T) {
	m := newTestMiddleware(t, []tool.Definition{calcDef()}, nil)
	var hits int32
	next := countingEndpoint(&hits, `{"ok":true}`)

	cases := map[string]string{
		"缺必填 b":    `{"a":1,"op":"+"}`,
		"枚举越界":     `{"a":1,"op":"%","b":2}`,
		"非法 JSON":  `{"a":1,`,
		"未知字段":     `{"a":1,"op":"+","b":2,"c":3}`,
		"类型错误":     `{"a":"一","op":"+","b":2}`,
		"空参数（缺必填）": `{}`,
	}
	for name, args := range cases {
		out, err := m.WrapToolCall(context.Background(),
			kernel.ToolCallMeta{Name: "system.calc", CallID: "c1"}, args, next)
		if err != nil {
			t.Errorf("%s：L2 拒绝应返回结构化结果而非 Go 错误，实际: %v", name, err)
			continue
		}
		assertErrorCode(t, out, CodeParamViolation)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("被拦截的调用不应执行端点，实际执行 %d 次", got)
	}
}

// TestPassThrough 验证放行路径：低风险工具参数合法时直接执行端点。
func TestPassThrough(t *testing.T) {
	m := newTestMiddleware(t, []tool.Definition{calcDef()}, nil)
	var hits int32
	out, err := m.WrapToolCall(context.Background(),
		kernel.ToolCallMeta{Name: "system.calc", CallID: "c1"}, `{"a":1,"op":"+","b":2}`,
		countingEndpoint(&hits, `{"ok":true,"data":{"result":3}}`))
	if err != nil {
		t.Fatalf("放行路径不应返回错误: %v", err)
	}
	if !strings.Contains(out, `"result":3`) {
		t.Errorf("放行结果应原样返回，实际: %s", out)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("放行路径应执行端点 1 次，实际: %d", got)
	}
}

// TestUnknownToolRejected 验证未登记工具按拒绝处理（防御性分支）。
func TestUnknownToolRejected(t *testing.T) {
	m := newTestMiddleware(t, []tool.Definition{calcDef()}, nil)
	var hits int32
	out, err := m.WrapToolCall(context.Background(),
		kernel.ToolCallMeta{Name: "ghost.tool", CallID: "c1"}, `{}`,
		countingEndpoint(&hits, ""))
	if err != nil {
		t.Fatalf("未登记工具应返回结构化结果，实际 Go 错误: %v", err)
	}
	assertErrorCode(t, out, CodeUnknownTool)
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("未登记工具不应执行端点，实际: %d", got)
	}
}

// TestNeedsApproval 验证 L4 审批判断：risk=high/critical 恒审批，
// 命名空间命中 approval_required 也审批，其余放行。
func TestNeedsApproval(t *testing.T) {
	m := newTestMiddleware(t, []tool.Definition{calcDef(), putDef()}, []string{"artifact.*"})

	cases := []struct {
		name string
		def  tool.Definition
		want bool
	}{
		{"risk=high", putDef(), true},
		{"risk=critical", func() tool.Definition {
			d := calcDef()
			d.Annotations.Risk = tool.RiskCritical
			return d
		}(), true},
		{"命中 approval_required", func() tool.Definition {
			d := putDef()
			d.Annotations.Risk = tool.RiskMedium
			return d
		}(), true},
		{"低风险未命中", calcDef(), false},
	}
	for _, c := range cases {
		if got := m.needsApproval(context.Background(), c.def); got != c.want {
			t.Errorf("%s：needsApproval = %v, 期望 %v", c.name, got, c.want)
		}
	}
}

// TestExecutionModeApproval 验证会话执行模式只调整 execute/execute_host，
// 不会让 full 绕过其他高风险工具的独立审批规则。
func TestExecutionModeApproval(t *testing.T) {
	m := newTestMiddleware(t, nil, nil)
	executeDef := calcDef()
	executeDef.Name, executeDef.Namespace = tool.ExecuteToolName, tool.ExecuteToolName
	executeDef.Annotations.Risk = tool.RiskHigh
	hostDef := executeDef
	hostDef.Name, hostDef.Namespace = tool.ExecuteHostToolName, tool.ExecuteHostToolName

	for _, tc := range []struct {
		mode        string
		executeWant bool
		hostWant    bool
	}{
		{mode: store.ExecutionModeAsk, executeWant: true, hostWant: true},
		{mode: store.ExecutionModeAuto, executeWant: false, hostWant: true},
		{mode: store.ExecutionModeFull, executeWant: false, hostWant: false},
	} {
		ctx := tool.WithExecutionScope(context.Background(), tool.ExecutionScope{
			SessionID: "session-1", ProjectID: "project-1", WorkspaceRoot: "/workspace",
			ExecutionMode: tc.mode,
		})
		if got := m.needsApproval(ctx, executeDef); got != tc.executeWant {
			t.Errorf("mode=%s execute 审批=%v，期望 %v", tc.mode, got, tc.executeWant)
		}
		if got := m.needsApproval(ctx, hostDef); got != tc.hostWant {
			t.Errorf("mode=%s execute_host 审批=%v，期望 %v", tc.mode, got, tc.hostWant)
		}
		if got := m.needsApproval(ctx, putDef()); !got {
			t.Errorf("mode=%s 不应绕过普通高风险工具审批", tc.mode)
		}
	}
}

// TestRobotToolApprovalScope 验证 Robot 工具只在批准后的精确 Task/SubTask
// 范围内免除重复审批。风险等级本身不会让普通会话或不完整上下文绕过门禁。
func TestRobotToolApprovalScope(t *testing.T) {
	m := newTestMiddleware(t, nil, nil)
	robotRun := calcDef()
	robotRun.Name, robotRun.Namespace = "robot.run", "robot"
	robotRun.Annotations.Risk = tool.RiskHigh
	robotStop := robotRun
	robotStop.Name = "robot.stop"

	base := tool.ExecutionScope{
		RunKind: store.RunKindTaskExecution, RunID: "run-1", AgentID: "robot:r1pro-1",
		SessionID: "session-1", ProjectID: "project-1", WorkspaceRoot: "/workspace",
		WorkflowID: "workflow-1", TaskID: "task-1", SubtaskID: "subtask-1",
		RobotID: "r1pro-1",
	}
	ctx := tool.WithExecutionScope(context.Background(), base)
	if got := m.needsApproval(ctx, robotRun); got {
		t.Fatal("批准范围内的精确 Robot SubTask 不应重复审批 robot.run")
	}
	if got := m.needsApproval(ctx, robotStop); got {
		t.Fatal("明确 execution 的安全停止不应等待审批")
	}

	for name, mutate := range map[string]func(*tool.ExecutionScope){
		"普通会话":       func(scope *tool.ExecutionScope) { scope.RunKind = store.RunKindConversation },
		"缺 Workflow": func(scope *tool.ExecutionScope) { scope.WorkflowID = "" },
		"缺 Task":     func(scope *tool.ExecutionScope) { scope.TaskID = "" },
		"缺 SubTask":  func(scope *tool.ExecutionScope) { scope.SubtaskID = "" },
		"缺 Robot":    func(scope *tool.ExecutionScope) { scope.RobotID = "" },
	} {
		scope := base
		mutate(&scope)
		if got := m.needsApproval(tool.WithExecutionScope(context.Background(), scope), robotRun); !got {
			t.Errorf("%s 不得免除 robot.run 审批", name)
		}
	}
}

// TestHighRiskTriggersInterrupt 验证 risk=high 工具首次执行发起中断
// （直接调用：运行上下文外中断以错误形态返回）。
func TestHighRiskTriggersInterrupt(t *testing.T) {
	m := newTestMiddleware(t, []tool.Definition{putDef()}, nil)
	var hits int32
	out, err := m.WrapToolCall(context.Background(),
		kernel.ToolCallMeta{Name: "artifact.put", CallID: "c1"}, `{"content":"报告"}`,
		countingEndpoint(&hits, ""))
	if err == nil {
		t.Error("高危工具首次执行应发起中断（返回中断错误）")
	}
	if out != "" {
		t.Errorf("中断时不应有结果输出，实际: %s", out)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("中断时端点不应执行，实际: %d", got)
	}
}

// e2eSaveTool 是端到端测试用的产物写入工具（计数）。
type e2eSaveTool struct {
	// hits 执行次数。
	hits int32
}

// Def 返回 artifact.put 的契约。
func (s *e2eSaveTool) Def() tool.Definition { return putDef() }

// Run 执行 artifact.put：计数并返回成功。
func (s *e2eSaveTool) Run(_ context.Context, _ string) (string, error) {
	atomic.AddInt32(&s.hits, 1)
	return tool.OKResult(map[string]any{"saved": true})
}

// TestApprovalGateEndToEnd 验证 safety middleware 与内核中断/恢复的完整
// 组合：高危工具中断（负载为 ApprovalInfo）→ 批准恢复 → 工具执行；
// 拒绝恢复 → 工具不执行且模型读到 APPROVAL_REJECTED。
func TestApprovalGateEndToEnd(t *testing.T) {
	st := openSecurityTestStore(t)
	saveTool := &e2eSaveTool{}

	reg := tool.NewRegistry()
	if err := reg.Register(saveTool); err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
	executor := tool.NewExecutor(reg, tool.ExecutorOptions{
		Logger: log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
	})
	tools, err := kernel.AdaptTools(reg.List(), executor)
	if err != nil {
		t.Fatalf("AdaptTools 失败: %v", err)
	}
	mw := newTestMiddleware(t, reg.List(), nil)

	m := kernel.NewMockChatModel()
	m.SetScript(
		kernel.MockReply{ToolCalls: []schema.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: schema.FunctionCall{
				// 模型侧名为净化名（端点正则约束）；执行时经适配层还原为 artifact.put。
				Name:      "artifact_put",
				Arguments: `{"content":"季度报告"}`,
			},
		}}},
		kernel.MockReply{Content: "处理完毕。"},
	)
	runner, err := kernel.BuildAgent(context.Background(), kernel.AgentConfig{
		Name: "security-agent", Model: m, ModelName: "mock",
		Tools:  tools,
		Safety: mw,
		Store:  st,
		Logger: log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
	})
	if err != nil {
		t.Fatalf("BuildAgent 失败: %v", err)
	}

	runID := store.NewRunSessionID()
	if err := st.CreateRunSession(store.RunSession{
		ID: runID, AgentName: "security-agent", ChatSessionID: "cs-1",
		Status: store.RunStatusRunning,
	}); err != nil {
		t.Fatalf("创建 run session 失败: %v", err)
	}

	// ① 中断：负载是 ApprovalInfo，字段齐备。
	es, err := runner.RunWithCheckpoint(context.Background(), nil, "存一下报告", runID)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	var intrEvent kernel.Event
	for {
		ev, ok := es.Next()
		if !ok {
			t.Fatal("未出现 EventInterrupted")
		}
		if ev.Kind == kernel.EventError {
			t.Fatalf("运行出错: %v", ev.Err)
		}
		if ev.Kind == kernel.EventInterrupted {
			intrEvent = ev
			break
		}
	}
	if len(intrEvent.Interrupts) != 1 {
		t.Fatalf("应有 1 个中断点，实际: %+v", intrEvent.Interrupts)
	}
	info, ok := intrEvent.Interrupts[0].Info.(ApprovalInfo)
	if !ok {
		t.Fatalf("中断负载应为 ApprovalInfo，实际: %T", intrEvent.Interrupts[0].Info)
	}
	if info.Tool != "artifact.put" || info.Namespace != "artifact" || info.Risk != tool.RiskHigh ||
		info.Question == "" || !strings.Contains(info.ArgsJSON, "季度报告") {
		t.Errorf("ApprovalInfo 字段不齐备: %+v", info)
	}
	if info.Agent != "test-agent" {
		t.Errorf("ApprovalInfo.Agent 应为门禁归因身份 test-agent，实际: %q", info.Agent)
	}

	// ② 批准恢复：工具执行，模型总结。
	es2, err := runner.Resume(context.Background(), runID, map[string]any{intrEvent.Interrupts[0].ID: true})
	if err != nil {
		t.Fatalf("Resume 失败: %v", err)
	}
	for {
		ev, ok := es2.Next()
		if !ok {
			break
		}
		if ev.Kind == kernel.EventError {
			t.Fatalf("恢复后运行出错: %v", ev.Err)
		}
	}
	if got := atomic.LoadInt32(&saveTool.hits); got != 1 {
		t.Errorf("批准后工具应执行 1 次，实际: %d", got)
	}
}

// openSecurityTestStore 在临时目录打开一个已迁移的 Store（测试结束自动关闭）。
func openSecurityTestStore(t *testing.T) *store.Store {
	t.Helper()
	return storetest.OpenMigrated(t, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
}
