package kernel

import (
	"context"
	"io"
	"strings"
	"testing"

	"insightos.cn/semantic-framework/pkg/log"
)

// TestEmbeddedThinkTagsAcrossStreamFrames 验证 MiniMax 风格的内嵌 think
// 标签即使被任意切在多个流式帧中，也会进入 reasoning 事件；标签本身不泄漏，
// 最终正文只包含用户应看到的回答。
func TestEmbeddedThinkTagsAcrossStreamFrames(t *testing.T) {
	model := NewMockChatModel()
	model.SetStreamResponse([]string{
		"<thi", "nk>先分析", "图片内容</thi", "nk>\n最终", "回答",
	})
	runner, err := BuildAgent(context.Background(), AgentConfig{
		Name: "reasoning-agent", Role: "coordinator", Model: model, ModelName: "mock",
		MaxTurns: 3, Store: openKernelTestStore(t), Purpose: "chat",
		Logger: log.New(log.Options{Level: log.LevelError, Writer: io.Discard}),
	})
	if err != nil {
		t.Fatalf("构建测试 Agent 失败: %v", err)
	}
	stream, err := runner.Run(context.Background(), nil, "分析图片")
	if err != nil {
		t.Fatalf("启动测试运行失败: %v", err)
	}

	var text, reasoning strings.Builder
	for {
		event, ok := stream.Next()
		if !ok {
			t.Fatal("事件流在完成事件前关闭")
		}
		switch event.Kind {
		case EventTextDelta:
			text.WriteString(event.Text)
		case EventReasoningDelta:
			reasoning.WriteString(event.Text)
		case EventError:
			t.Fatalf("运行返回错误: %v", event.Err)
		case EventDone:
			if reasoning.String() != "先分析图片内容" {
				t.Fatalf("思考内容拆分错误: %q", reasoning.String())
			}
			if text.String() != "\n最终回答" {
				t.Fatalf("最终正文拆分错误: %q", text.String())
			}
			if strings.Contains(text.String(), "<think>") || strings.Contains(text.String(), "</think>") {
				t.Fatalf("think 标签不应泄漏到正文: %q", text.String())
			}
			return
		}
	}
}

// TestThinkTagSplitterFlushesMalformedTail 验证残缺标签不会被静默吞掉。
func TestThinkTagSplitterFlushesMalformedTail(t *testing.T) {
	splitter := &thinkTagSplitter{}
	parts := splitter.Write("回答<thi")
	parts = append(parts, splitter.Flush()...)
	var text strings.Builder
	for _, part := range parts {
		if part.reasoning {
			t.Fatalf("残缺起始标签不应被误判为思考，实际: %+v", parts)
		}
		text.WriteString(part.text)
	}
	if text.String() != "回答<thi" {
		t.Fatalf("残缺标签应原样保留为正文，实际: %q", text.String())
	}
}
