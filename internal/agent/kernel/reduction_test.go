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
	"regexp"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/pkg/log"
)

// kernelTestLogger 返回丢弃输出的测试日志器，避免中间件调试日志污染测试结果。
func kernelTestLogger() *log.Logger {
	return log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
}

// artifactIDPattern 从截断提示中提取外置产物 ID（路径即引用，art-<uuid>）。
var artifactIDPattern = regexp.MustCompile(`art-[0-9a-f-]{36}`)

// TestReductionTruncatesAndOffloads 端到端验证 reduction middleware：
// 超大工具结果（>20000 字符）被截断为 引用+预览 进入模型输入，
// 全文经 Backend 外置到 store，按产物 ID 可取回原文。
func TestReductionTruncatesAndOffloads(t *testing.T) {
	st := openKernelTestStore(t)

	// 30000+ 字符的工具结果（超过 20000 截断阈值）。
	big := strings.Repeat("数据", 15001)
	bigTool, err := utils.InferTool("big_result", "返回超大结果",
		func(context.Context, struct{}) (string, error) { return big, nil })
	if err != nil {
		t.Fatalf("构建工具失败: %v", err)
	}

	m := NewMockChatModel()
	m.SetScript(
		MockReply{ToolCalls: []schema.ToolCall{{
			ID:       "call-1",
			Type:     "function",
			Function: schema.FunctionCall{Name: "big_result", Arguments: `{}`},
		}}},
		MockReply{Content: "结果已收到。"},
	)

	runner, err := BuildAgent(context.Background(), AgentConfig{
		Name:      "reduction-agent",
		Model:     m,
		ModelName: "mock",
		Tools:     []Tool{bigTool},
		MaxTurns:  5,
		Store:     st,
		Logger:    kernelTestLogger(),
	})
	if err != nil {
		t.Fatalf("BuildAgent 失败: %v", err)
	}

	es, err := runner.Run(context.Background(), nil, "取数")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if _, _, runErr := collectRun(t, es); runErr != nil {
		t.Fatalf("运行出错: %v", runErr)
	}

	// ① 第二轮模型输入中的工具消息应是截断提示：不含全文、含产物 ID 与
	// 读回工具名。
	inputs := m.CallInputs()
	if len(inputs) != 2 {
		t.Fatalf("模型应被调用 2 次，实际: %d", len(inputs))
	}
	var toolMsg *schema.Message
	for _, msg := range inputs[1] {
		if msg.Role == schema.Tool {
			toolMsg = msg
		}
	}
	if toolMsg == nil {
		t.Fatal("第二轮输入应含工具结果消息")
	}
	if strings.Contains(toolMsg.Content, big) {
		t.Error("工具结果应被截断，模型输入不应含全文")
	}
	if !strings.Contains(toolMsg.Content, reductionReadFileTool) {
		t.Errorf("截断提示应引导用 %s 读回，实际: %.200q", reductionReadFileTool, toolMsg.Content)
	}
	id := artifactIDPattern.FindString(toolMsg.Content)
	if id == "" {
		t.Fatalf("截断提示应含外置产物 ID，实际: %.200q", toolMsg.Content)
	}

	// ② 外置可取：按产物 ID 从 store 读回全文与元数据。
	a, content, err := st.GetArtifact(id)
	if err != nil {
		t.Fatalf("按产物 ID 取回外置内容失败: %v", err)
	}
	if string(content) != big {
		t.Errorf("外置全文应与原工具结果一致（%d 字符），实际: %d 字符",
			len([]rune(big)), len([]rune(string(content))))
	}
	if !strings.Contains(a.Metadata, "reduction") {
		t.Errorf("产物元数据应标记 reduction 来源，实际: %s", a.Metadata)
	}
	if a.Summary == "" {
		t.Error("外置产物应有摘要")
	}
}

// TestReductionSmallResultUntouched 验证未超阈值的工具结果原样进入模型
// 输入（无外置、无截断）。
func TestReductionSmallResultUntouched(t *testing.T) {
	st := openKernelTestStore(t)

	smallTool, err := utils.InferTool("small_result", "返回小结果",
		func(context.Context, struct{}) (string, error) { return "小结果", nil })
	if err != nil {
		t.Fatalf("构建工具失败: %v", err)
	}

	m := NewMockChatModel()
	m.SetScript(
		MockReply{ToolCalls: []schema.ToolCall{{
			ID:       "call-1",
			Type:     "function",
			Function: schema.FunctionCall{Name: "small_result", Arguments: `{}`},
		}}},
		MockReply{Content: "收到。"},
	)

	runner, err := BuildAgent(context.Background(), AgentConfig{
		Name:      "reduction-agent",
		Model:     m,
		ModelName: "mock",
		Tools:     []Tool{smallTool},
		MaxTurns:  5,
		Store:     st,
		Logger:    kernelTestLogger(),
	})
	if err != nil {
		t.Fatalf("BuildAgent 失败: %v", err)
	}

	es, err := runner.Run(context.Background(), nil, "取数")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if _, _, runErr := collectRun(t, es); runErr != nil {
		t.Fatalf("运行出错: %v", runErr)
	}

	inputs := m.CallInputs()
	if len(inputs) != 2 {
		t.Fatalf("模型应被调用 2 次，实际: %d", len(inputs))
	}
	var toolMsg *schema.Message
	for _, msg := range inputs[1] {
		if msg.Role == schema.Tool {
			toolMsg = msg
		}
	}
	if toolMsg == nil || toolMsg.Content != "小结果" {
		t.Errorf("小结果应原样进入模型输入，实际: %+v", toolMsg)
	}

	// 无外置产物（小结果不触发外置）。
	artifacts, err := st.ListArtifacts(0, 0)
	if err != nil {
		t.Fatalf("ListArtifacts 失败: %v", err)
	}
	if len(artifacts) != 0 {
		t.Errorf("小结果不应外置产物，实际: %d 个", len(artifacts))
	}
}
