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

package kernel

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	runtimetool "insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/log"
)

// newDelegationTool 构建测试用委派工具：子 agent 用脚本化 mock 模型。
func newDelegationTool(t *testing.T, cfg AgentConfig, def AgentToolDef) tool.BaseTool {
	t.Helper()
	agentTool, err := BuildAgentTool(context.Background(), cfg, def)
	if err != nil {
		t.Fatalf("BuildAgentTool 失败: %v", err)
	}
	return agentTool
}

// subAgentConfig 返回 query-1 的测试 AgentConfig（mock 模型 + 隔离上下文）。
func subAgentConfig(m *MockChatModel) AgentConfig {
	return AgentConfig{
		Name:        "query-1",
		Role:        "service",
		Description: "系统与产物查询助手",
		Instruction: "你是查询助手。",
		Model:       m,
		MaxTurns:    6,
	}
}

// testAgentToolDef 是测试用委派工具定义。
func testAgentToolDef() AgentToolDef {
	return AgentToolDef{
		ToolName:        "ask_query",
		ToolDescription: "系统状态与产物查询助手",
		Timeout:         time.Minute,
	}
}

// TestBuildAgentTool 验证委派工具构建与输入 schema：名称/描述按定义呈现，
// 入参为 {task: string} 且 task 必填。
func TestBuildAgentTool(t *testing.T) {
	at := newDelegationTool(t, subAgentConfig(NewMockChatModel()), testAgentToolDef())

	info, err := at.Info(context.Background())
	if err != nil {
		t.Fatalf("Info 失败: %v", err)
	}
	if info.Name != "ask_query" || info.Desc != "系统状态与产物查询助手" {
		t.Errorf("工具呈现不符: %+v", info)
	}
	js, err := info.ParamsOneOf.ToJSONSchema()
	if err != nil {
		t.Fatalf("参数 schema 转换失败: %v", err)
	}
	prop, ok := js.Properties.Get("task")
	if !ok {
		t.Fatalf("参数 schema 应含 task 属性: %+v", js.Properties)
	}
	if prop.Type != "string" {
		t.Errorf("task 应为 string 类型，实际: %s", prop.Type)
	}
	if len(js.Required) != 1 || js.Required[0] != "task" {
		t.Errorf("task 应为必填参数，实际: %+v", js.Required)
	}
	artifactRefs, ok := js.Properties.Get(AgentToolArtifactRefsParam)
	if !ok || artifactRefs.Type != "array" || artifactRefs.Items == nil || artifactRefs.Items.Type != "string" {
		t.Errorf("artifact_refs 应为可选字符串数组，实际: %+v", artifactRefs)
	}
}

// TestBuildAgentToolNestedRejected 验证嵌套防御（depth ≤1）：SubAgent 的
// 工具集含委派工具时构建直接报错。
func TestBuildAgentToolNestedRejected(t *testing.T) {
	inner := newDelegationTool(t, subAgentConfig(NewMockChatModel()), testAgentToolDef())

	cfg := subAgentConfig(NewMockChatModel())
	cfg.Name = "nested-1"
	cfg.Tools = []tool.BaseTool{inner}
	if _, err := BuildAgentTool(context.Background(), cfg, testAgentToolDef()); err == nil {
		t.Error("工具集含委派工具的 SubAgent 应构建失败（委派深度 ≤1）")
	}

	// 名称/描述缺失同样构建期报错。
	if _, err := BuildAgentTool(context.Background(), subAgentConfig(NewMockChatModel()),
		AgentToolDef{ToolName: "ask_query"}); err == nil {
		t.Error("缺 ToolDescription 应构建失败")
	}
}

// TestAgentToolInvoke 验证委派执行：入参 {task} 解析、子 agent 独立 run、
// 结果文本回传；空 task 与非法 JSON 归一为结构化错误结果（不拖垮父 run）。
func TestAgentToolInvoke(t *testing.T) {
	sub := NewMockChatModel()
	sub.SetResponse("当前产物：report.pdf、log.txt、img.png")
	at := newDelegationTool(t, subAgentConfig(sub), testAgentToolDef())

	out, err := at.(tool.InvokableTool).InvokableRun(context.Background(), `{"task":"查 artifact 列表"}`)
	if err != nil {
		t.Fatalf("委派执行失败: %v", err)
	}
	if out != "当前产物：report.pdf、log.txt、img.png" {
		t.Errorf("委派结果应为子 agent 最终回复，实际: %q", out)
	}
	// 子 agent 收到的用户消息是干净的任务文本（不是 {task} 的原始 JSON）。
	inputs := sub.CallInputs()
	if len(inputs) != 1 {
		t.Fatalf("子 agent 应被调用 1 次，实际: %d", len(inputs))
	}
	var userText string
	for _, msg := range inputs[0] {
		if msg.Role == schema.User {
			userText = msg.Content
		}
	}
	if userText != "查 artifact 列表" {
		t.Errorf("子 agent 用户消息应为任务文本，实际: %q", userText)
	}

	for _, bad := range []string{`{"task":""}`, `not-json`} {
		out, err := at.(tool.InvokableTool).InvokableRun(context.Background(), bad)
		if err != nil {
			t.Errorf("非法入参 %q 应归一为结构化结果而非 Go 错误: %v", bad, err)
		}
		if !strings.HasPrefix(out, `{"ok":false`) {
			t.Errorf("非法入参 %q 应返回结构化错误结果，实际: %q", bad, out)
		}
	}
}

// TestAgentToolStripsEmbeddedReasoning 验证 SubAgent 使用 MiniMax 等兼容
// 服务返回内嵌 <think> 标签时，推理不会作为工具结果再次进入 Leader 上下文。
// 推理事件仍由 Agent 事件冒泡链路独立转发，本测试只约束最终结果边界。
func TestAgentToolStripsEmbeddedReasoning(t *testing.T) {
	sub := NewMockChatModel()
	sub.SetResponse("<think>内部分析，不应进入 Leader 工具结果</think>\n图片中的模型是 MiniMax-M3。")
	at := newDelegationTool(t, subAgentConfig(sub), testAgentToolDef())

	out, err := at.(tool.InvokableTool).InvokableRun(context.Background(), `{"task":"识别图片"}`)
	if err != nil {
		t.Fatalf("委派执行失败: %v", err)
	}
	if out != "\n图片中的模型是 MiniMax-M3。" {
		t.Fatalf("委派结果应只保留可见正文，实际: %q", out)
	}
	if strings.Contains(out, "<think>") || strings.Contains(out, "内部分析") {
		t.Fatalf("委派结果泄漏了 SubAgent 推理内容: %q", out)
	}
}

// TestAgentToolForwardsCurrentImageArtifacts 验证当前消息图片自动转发；用户
// 在后续消息中显式给出自己的 ArtifactRef 也可以转发，而其他用户引用被拒绝。
func TestAgentToolForwardsCurrentImageArtifacts(t *testing.T) {
	st := openKernelTestStore(t)
	artifact, err := st.PutUserArtifact("usr-1", "image/png", "camera.png", "{}",
		[]byte{'P', 'N', 'G'})
	if err != nil {
		t.Fatalf("写入图片 Artifact 失败: %v", err)
	}
	sub := NewMockChatModel()
	sub.SetResponse("图片已收到")
	cfg := subAgentConfig(sub)
	cfg.Store = st
	cfg.Logger = log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
	at := newDelegationTool(t, cfg, testAgentToolDef())

	ctx := WithRunArtifactRefs(context.Background(), []string{artifact.ID})
	out, err := at.(tool.InvokableTool).InvokableRun(ctx, `{"task":"分析当前图片"}`)
	if err != nil || out != "图片已收到" {
		t.Fatalf("图片委派失败: out=%q err=%v", out, err)
	}
	inputs := sub.CallInputs()
	if len(inputs) != 1 {
		t.Fatalf("子 Agent 模型调用次数不符: %d", len(inputs))
	}
	var user *schema.Message
	for _, message := range inputs[0] {
		if message.Role == schema.User {
			user = message
		}
	}
	if user == nil || user.Content != "" || len(user.UserInputMultiContent) != 2 {
		t.Fatalf("子 Agent 应收到任务文本和一张图片: %+v", user)
	}
	if user.UserInputMultiContent[0].Text != "分析当前图片" ||
		user.UserInputMultiContent[1].Image == nil ||
		user.UserInputMultiContent[1].Image.MIMEType != "image/png" {
		t.Fatalf("多模态输入内容不符: %+v", user.UserInputMultiContent)
	}

	// 模拟下一轮只有文本 Artifact ID、没有重新上传附件：执行作用域提供当前
	// 用户身份，AgentTool 应从 Store 校验归属后把原图传给视觉 SubAgent。
	ownerCtx := runtimetool.WithExecutionScope(context.Background(), runtimetool.ExecutionScope{
		SessionID: "cs-1", ProjectID: "proj-1", OwnerID: "usr-1", WorkspaceRoot: t.TempDir(),
	})
	out, err = at.(tool.InvokableTool).InvokableRun(ownerCtx,
		`{"task":"分析既有图片","artifact_refs":["`+artifact.ID+`"]}`)
	if err != nil || out != "图片已收到" {
		t.Fatalf("用户自有既有 ArtifactRef 应允许委派: out=%q err=%v", out, err)
	}
	inputs = sub.CallInputs()
	var explicitUser *schema.Message
	for _, message := range inputs[len(inputs)-1] {
		if message.Role == schema.User {
			explicitUser = message
		}
	}
	if explicitUser == nil || len(explicitUser.UserInputMultiContent) != 2 ||
		explicitUser.UserInputMultiContent[1].Image == nil {
		t.Fatalf("显式 ArtifactRef 未转换为视觉输入: %+v", explicitUser)
	}

	other, err := st.PutUserArtifact("usr-other", "image/png", "private.png", "{}", []byte("PNG"))
	if err != nil {
		t.Fatalf("写入其他用户 Artifact 失败: %v", err)
	}
	out, err = at.(tool.InvokableTool).InvokableRun(ownerCtx,
		`{"task":"越权读取","artifact_refs":["`+other.ID+`"]}`)
	if err != nil || !strings.Contains(out, "ArtifactRef 不存在或不属于当前用户") {
		t.Fatalf("其他用户 ArtifactRef 应返回结构化拒绝: out=%q err=%v", out, err)
	}
}

// TestDelegationEventFlow 验证委派全链路的事件转换：leader（脚本：轮 1 调
// ask_query，轮 2 总结）的工具集含委派工具 → EmitInternalEvents 自动开启 →
// 子 agent 文本增量冒泡为 EventSubAgentDelta、结果归一为 EventSubAgentResult
// （按 call_id 配对任务文本）；子 agent 轮次/用量不归并父 run。
func TestDelegationEventFlow(t *testing.T) {
	sub := NewMockChatModel()
	sub.SetResponse("当前产物：report.pdf、log.txt、img.png")
	delegation := newDelegationTool(t, subAgentConfig(sub), testAgentToolDef())

	leader := NewMockChatModel()
	leader.SetScript(
		MockReply{ToolCalls: []schema.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: schema.FunctionCall{
				Name:      "ask_query",
				Arguments: `{"task":"查 artifact 列表"}`,
			},
		}}},
		MockReply{Content: "当前共有 3 个产物：report.pdf、log.txt、img.png。"},
	)

	runner, err := BuildAgent(context.Background(), AgentConfig{
		Name:        "leader",
		Role:        "coordinator",
		Description: "团队指挥官",
		Instruction: "你是 Leader。",
		Model:       leader,
		Tools:       []tool.BaseTool{delegation},
		MaxTurns:    10,
	})
	if err != nil {
		t.Fatalf("BuildAgent 失败: %v", err)
	}

	stream, err := runner.Run(context.Background(), nil, "查一下有哪些产物")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}

	var subDeltas []string
	var subResult *Event
	var leaderText strings.Builder
	var done *Event
	for {
		ev, ok := stream.Next()
		if !ok {
			break
		}
		switch ev.Kind {
		case EventSubAgentDelta:
			if ev.AgentID != "query-1" {
				t.Errorf("冒泡事件归因不符: AgentID=%q", ev.AgentID)
			}
			subDeltas = append(subDeltas, ev.Text)
		case EventSubAgentResult:
			r := ev
			subResult = &r
		case EventTextDelta:
			leaderText.WriteString(ev.Text)
		case EventError:
			t.Fatalf("运行出现错误事件: %v", ev.Err)
		case EventDone:
			d := ev
			done = &d
		}
	}

	if len(subDeltas) == 0 {
		t.Fatal("未收到 query-1 的 EventSubAgentDelta 冒泡事件")
	}
	if got := strings.Join(subDeltas, ""); got != "当前产物：report.pdf、log.txt、img.png" {
		t.Errorf("冒泡增量拼接应为子 agent 回复全文，实际: %q", got)
	}
	if subResult == nil {
		t.Fatal("未收到 EventSubAgentResult")
	}
	if subResult.AgentID != "query-1" {
		t.Errorf("结果事件归因不符: %+v", subResult)
	}
	if subResult.Task != "查 artifact 列表" {
		t.Errorf("结果事件应按 call_id 配对任务文本，实际: %q", subResult.Task)
	}
	if subResult.Text != "当前产物：report.pdf、log.txt、img.png" {
		t.Errorf("结果事件应携带子 agent 最终回复全文，实际: %q", subResult.Text)
	}
	if leaderText.String() != "当前共有 3 个产物：report.pdf、log.txt、img.png。" {
		t.Errorf("leader 总结文本不符: %q", leaderText.String())
	}
	if done == nil {
		t.Fatal("未收到 EventDone")
	}
	// 父 run 只统计本级轮次（调工具 + 总结 = 2 轮）；子 agent 的 1 轮不归并。
	if done.Turns != 2 {
		t.Errorf("父 run 轮次应为 2（子 agent 轮次不归并），实际: %d", done.Turns)
	}
}
