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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	adkfs "github.com/cloudwego/eino/adk/filesystem"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/pkg/log"
)

// TestProjectFilesystemBackend 验证领域薄适配器复用 Local Backend 的同时，
// 只接受 /workspace 虚拟路径，并拒绝通过符号链接读取 Project 外部文件。
func TestProjectFilesystemBackend(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "inside.txt"), []byte("项目内容\n第二行"), 0o600); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "secret.txt"), []byte("外部秘密"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(workspace, "escape")); err != nil {
		t.Fatal(err)
	}

	backend, err := newProjectFilesystemBackend(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	content, err := backend.Read(context.Background(), &adkfs.ReadRequest{
		FilePath: "/workspace/inside.txt", Offset: 1, Limit: 1,
	})
	if err != nil || content.Content != "项目内容" {
		t.Fatalf("Project 内文件读取失败: content=%+v err=%v", content, err)
	}
	if _, err := backend.Read(context.Background(), &adkfs.ReadRequest{FilePath: filepath.Join(external, "secret.txt")}); err == nil {
		t.Fatal("宿主真实绝对路径不应被接受")
	}
	if _, err := backend.Read(context.Background(), &adkfs.ReadRequest{FilePath: "/workspace/escape/secret.txt"}); err == nil {
		t.Fatal("通过 Project 内符号链接逃逸应被拒绝")
	}
	if err := backend.Write(context.Background(), &adkfs.WriteRequest{
		FilePath: "/workspace/new.txt", Content: "禁止写入",
	}); err == nil {
		t.Fatal("只读 Backend 不应允许直接写文件")
	}
	if _, err := os.Stat(filepath.Join(workspace, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("被拒绝的写入不应创建文件: %v", err)
	}
}

// TestFilesystemToolBypassesSafety 验证只读文件工具由边界 Backend 自身保证
// 安全，不会被注册表门禁误判为 UNKNOWN_TOOL；写入工具根本不进入模型工具集。
func TestFilesystemToolBypassesSafety(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "note.txt"), []byte("工作区内容"), 0o600); err != nil {
		t.Fatal(err)
	}
	guard := &recordingGuard{}
	model := NewMockChatModel()
	model.SetScript(
		MockReply{ToolCalls: []schema.ToolCall{{
			ID: "read-1", Type: "function",
			Function: schema.FunctionCall{Name: "read_file",
				Arguments: `{"file_path":"/workspace/note.txt","limit":10}`},
		}}},
		MockReply{Content: "已读取。"},
	)
	runner, err := BuildAgent(context.Background(), AgentConfig{
		Name: "leader", Model: model, MaxTurns: 3, Safety: guard, WorkspaceRoot: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := runner.Run(context.Background(), nil, "读取文件")
	if err != nil {
		t.Fatal(err)
	}
	text, _, runErr := collectRun(t, events)
	if runErr != nil || text != "已读取。" {
		t.Fatalf("只读文件工具运行失败: text=%q err=%v", text, runErr)
	}
	if names := guard.calledNames(); len(names) != 0 {
		t.Fatalf("边界内只读文件工具不应进入注册表门禁，实际: %v", names)
	}
	inputs := model.CallInputs()
	if len(inputs) != 2 {
		t.Fatalf("模型应经历文件调用与总结两轮，实际: %d", len(inputs))
	}
	var found bool
	for _, message := range inputs[1] {
		if message.Role == schema.Tool && strings.Contains(message.Content, "工作区内容") {
			found = true
		}
	}
	if !found {
		t.Fatalf("第二轮上下文应包含 Eino read_file 结果: %+v", inputs[1])
	}
}

// TestFilesystemMiddlewareTools 验证 Eino 官方 Middleware 只注入四个只读
// 文件工具，并能通过 /workspace 虚拟路径实际读取 Project 文件。
func TestFilesystemMiddlewareTools(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "hello.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	middleware, err := buildFilesystemMiddleware(context.Background(), workspace,
		log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	if err != nil {
		t.Fatal(err)
	}
	runCtx := &adk.ChatModelAgentContext{}
	_, runCtx, err = middleware.BeforeAgent(context.Background(), runCtx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(runCtx.Instruction, "/workspace") {
		t.Fatalf("Middleware 应注入 Project 虚拟根说明: %q", runCtx.Instruction)
	}
	want := map[string]bool{"ls": false, "read_file": false, "glob": false, "grep": false}
	var readTool tool.InvokableTool
	for _, candidate := range runCtx.Tools {
		info, infoErr := candidate.Info(context.Background())
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		if _, ok := want[info.Name]; !ok {
			t.Fatalf("不应注入写入或命令工具 %q", info.Name)
		}
		want[info.Name] = true
		if info.Name == "read_file" {
			readTool, _ = candidate.(tool.InvokableTool)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("缺少 Eino 文件工具 %q", name)
		}
	}
	if readTool == nil {
		t.Fatal("read_file 应实现 InvokableTool")
	}
	result, err := readTool.InvokableRun(context.Background(), `{"file_path":"/workspace/hello.txt","limit":10}`)
	if err != nil || !strings.Contains(result, "hello") {
		t.Fatalf("Eino read_file 未读取 Project 文件: result=%q err=%v", result, err)
	}
}
