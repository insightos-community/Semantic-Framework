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

import "strings"

const (
	thinkOpenTag  = "<think>"
	thinkCloseTag = "</think>"
)

// contentPart 是模型正文拆分后的一个有序片段。reasoning=true 表示该片段
// 来自服务商以内嵌 <think> 标签返回的思考内容，应进入 reasoning 通道。
type contentPart struct {
	text      string
	reasoning bool
}

// thinkTagSplitter 增量解析模型 Content 中的 <think>...</think>。部分
// OpenAI-compatible 服务（例如 MiniMax）不填写 ReasoningContent，而把思考
// 内容直接嵌入正文；拆分必须跨流式帧保留标签前缀，避免把被切开的标签泄漏到正文。
type thinkTagSplitter struct {
	pending string
	inThink bool
}

// Write 消费一个模型文本增量并返回当前已经可以确定归属的片段。可能构成
// 标签前缀的尾部暂存在 pending，等待下一帧后再判定。
func (s *thinkTagSplitter) Write(chunk string) []contentPart {
	s.pending += chunk
	var parts []contentPart
	for s.pending != "" {
		tag := thinkOpenTag
		if s.inThink {
			tag = thinkCloseTag
		}
		if index := strings.Index(s.pending, tag); index >= 0 {
			parts = appendContentPart(parts, s.pending[:index], s.inThink)
			s.pending = s.pending[index+len(tag):]
			s.inThink = !s.inThink
			continue
		}

		keep := trailingTagPrefixLength(s.pending, tag)
		emitUntil := len(s.pending) - keep
		parts = appendContentPart(parts, s.pending[:emitUntil], s.inThink)
		s.pending = s.pending[emitUntil:]
		break
	}
	return parts
}

// Flush 在模型流结束时输出尚未决议的尾部。即使服务商返回了残缺标签，
// 也必须保留原始文本，不能因为协议异常静默丢失内容。
func (s *thinkTagSplitter) Flush() []contentPart {
	parts := appendContentPart(nil, s.pending, s.inThink)
	s.pending = ""
	return parts
}

// trailingTagPrefixLength 返回 text 尾部与 tag 前缀重合的最长字节数。
// 标签是 ASCII，按字节处理不会切开普通中文内容。
func trailingTagPrefixLength(text, tag string) int {
	limit := len(tag) - 1
	if len(text) < limit {
		limit = len(text)
	}
	for size := limit; size > 0; size-- {
		if strings.HasSuffix(text, tag[:size]) {
			return size
		}
	}
	return 0
}

// appendContentPart 过滤空片段并合并相邻同类内容，减少下游事件数量。
func appendContentPart(parts []contentPart, text string, reasoning bool) []contentPart {
	if text == "" {
		return parts
	}
	if len(parts) > 0 && parts[len(parts)-1].reasoning == reasoning {
		parts[len(parts)-1].text += text
		return parts
	}
	return append(parts, contentPart{text: text, reasoning: reasoning})
}

// visibleModelContent 从模型最终 Content 中移除内嵌的思考片段，只返回可以
// 交给用户或上级 Agent 的正文。AgentTool 的最终结果不再经过流式事件转换，
// 因此必须在工具边界再次归一；否则 SubAgent 的 <think> 标签会随工具结果
// 进入 Leader 上下文，并在前端与 reasoning.delta 重复展示。
func visibleModelContent(content string) string {
	splitter := &thinkTagSplitter{}
	parts := splitter.Write(content)
	parts = append(parts, splitter.Flush()...)
	var visible strings.Builder
	for _, part := range parts {
		if !part.reasoning {
			visible.WriteString(part.text)
		}
	}
	return visible.String()
}
