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

// Package builtin 是内置工具的集合（架构文档 05 §4：进程内 Go 实现，
// 编译进 server，启动时注册）。
//   - system.time / system.echo / system.calc：无副作用的系统工具（risk=low）；
//   - artifact.put（risk=high，写产物，触发 L4 审批）/ artifact.get（risk=medium）/
//     artifact.list（risk=low，列举产物清单）；
//   - execute（risk=high，在 Eino-ext DockerSandbox 中执行命令）；
//   - execute_host（risk=high，经 Eino-ext Local Backend 执行宿主命令）。
package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"insightos.cn/semantic-framework/internal/interaction"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
)

// 工具全名取值（线上契约的一部分，禁止改名）。
const (
	// nameSystemTime 返回当前时间。
	nameSystemTime = "system.time"

	// nameSystemEcho 回显文本。
	nameSystemEcho = "system.echo"

	// nameSystemCalc 四则运算。
	nameSystemCalc = "system.calc"

	// nameArtifactPut 写入产物。
	nameArtifactPut = "artifact.put"

	// nameArtifactGet 读取产物。
	nameArtifactGet = "artifact.get"

	// nameArtifactList 列举产物清单。
	nameArtifactList = "artifact.list"

	nameMapQuery = "map.query"

	namePlanSuggest    = "plan.suggest"
	nameInteractionAsk = "interaction.ask"
)

// summaryRuneLimit 是 artifact.put 未提供 summary 时自动摘要的字数上限。
const summaryRuneLimit = 50

// RegisterAll 把全部内置工具注册到注册表；artifact 工具依赖 store
// （产物元数据 + 内容本体）。Skill 清单与正文由 Eino Skill Middleware
// 渐进加载，不在注册表中维护重复的目录工具。任一注册失败即返回错误。
type PlanProposalSubmitter interface {
	SubmitPlanProposal(context.Context, interaction.PlanSuggestionRequest) (store.PlanProposal, error)
}

type AgentQuestionCreator interface {
	CreateAgentQuestion(context.Context, interaction.AgentQuestionRequest) (store.Interaction, error)
}

type AgentToolCreator interface {
	PlanProposalSubmitter
	AgentQuestionCreator
}

func RegisterAll(reg *tool.Registry, st *store.Store, creators ...AgentToolCreator) error {
	var creator AgentToolCreator
	if len(creators) > 0 {
		creator = creators[0]
	}
	tools := []tool.Tool{
		systemTimeTool{},
		systemEchoTool{},
		systemCalcTool{},
		&artifactPutTool{st: st},
		&artifactGetTool{st: st},
		&artifactListTool{st: st},
		&artifactRegisterTool{st: st},
		&mapQueryTool{st: st},
		&planSuggestTool{creator: creator},
		&interactionAskTool{creator: creator},
		newExecuteTool(),
		newExecuteHostTool(),
	}
	for _, t := range tools {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// systemTimeTool 返回当前 UTC 时间（RFC3339）。
type systemTimeTool struct{}

// Def 返回 system.time 的契约。
func (systemTimeTool) Def() tool.Definition {
	return tool.Definition{
		Name:        nameSystemTime,
		Namespace:   "system",
		Description: "返回当前时间（UTC，RFC3339 格式）。需要知道当前日期/时间时使用。",
		ParametersJSON: `{
			"type": "object",
			"properties": {},
			"additionalProperties": false
		}`,
		Annotations: tool.Annotations{Risk: tool.RiskLow, Idempotent: true},
	}
}

// Run 执行 system.time：返回当前时间。
func (systemTimeTool) Run(_ context.Context, _ string) (string, error) {
	return tool.OKResult(map[string]any{"time": time.Now().UTC().Format(time.RFC3339)})
}

// systemEchoTool 回显 text 参数（联调/探测用）。
type systemEchoTool struct{}

// Def 返回 system.echo 的契约。
func (systemEchoTool) Def() tool.Definition {
	return tool.Definition{
		Name:        nameSystemEcho,
		Namespace:   "system",
		Description: "原样回显输入文本。用于验证工具链路是否可用。",
		ParametersJSON: `{
			"type": "object",
			"properties": {
				"text": {"type": "string", "description": "要回显的文本"}
			},
			"required": ["text"],
			"additionalProperties": false
		}`,
		Annotations: tool.Annotations{Risk: tool.RiskLow, Idempotent: true},
	}
}

// systemEchoArgs 是 system.echo 的参数结构。
type systemEchoArgs struct {
	// Text 要回显的文本。
	Text string `json:"text"`
}

// Run 执行 system.echo：回显 text。
func (systemEchoTool) Run(_ context.Context, argsJSON string) (string, error) {
	var args systemEchoArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", &tool.Error{Code: "BAD_ARGUMENTS", Message: "参数不是合法 JSON: " + err.Error()}
	}
	return tool.OKResult(map[string]any{"text": args.Text})
}

// systemCalcTool 四则运算（a op b，op ∈ + - * /）。
type systemCalcTool struct{}

// Def 返回 system.calc 的契约。
func (systemCalcTool) Def() tool.Definition {
	return tool.Definition{
		Name:        nameSystemCalc,
		Namespace:   "system",
		Description: "计算两个数的四则运算：a op b，op 取 +（加）-（减）*（乘）/（除）。",
		ParametersJSON: `{
			"type": "object",
			"properties": {
				"a":  {"type": "number", "description": "左操作数"},
				"op": {"type": "string", "enum": ["+", "-", "*", "/"], "description": "运算符"},
				"b":  {"type": "number", "description": "右操作数"}
			},
			"required": ["a", "op", "b"],
			"additionalProperties": false
		}`,
		Annotations: tool.Annotations{Risk: tool.RiskLow, Idempotent: true},
	}
}

// systemCalcArgs 是 system.calc 的参数结构。
type systemCalcArgs struct {
	// A 左操作数。
	A float64 `json:"a"`

	// Op 运算符（+ - * /）。
	Op string `json:"op"`

	// B 右操作数。
	B float64 `json:"b"`
}

// Run 执行 system.calc：除零返回结构化错误（DIVIDE_BY_ZERO，不可重试）。
func (systemCalcTool) Run(_ context.Context, argsJSON string) (string, error) {
	var args systemCalcArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", &tool.Error{Code: "BAD_ARGUMENTS", Message: "参数不是合法 JSON: " + err.Error()}
	}

	var result float64
	switch args.Op {
	case "+":
		result = args.A + args.B
	case "-":
		result = args.A - args.B
	case "*":
		result = args.A * args.B
	case "/":
		if args.B == 0 {
			return "", &tool.Error{Code: "DIVIDE_BY_ZERO", Message: "除数不能为零", Retryable: false}
		}
		result = args.A / args.B
	default:
		return "", &tool.Error{Code: "UNKNOWN_OPERATOR", Message: fmt.Sprintf("不支持的运算符 %q（仅支持 + - * /）", args.Op)}
	}
	return tool.OKResult(map[string]any{"result": result})
}

// artifactPutTool 把内容写入 artifact store（risk=high：写操作需人工审批）。
type artifactPutTool struct {
	// st 元数据存储（产物元数据与内容本体）。
	st *store.Store
}

// Def 返回 artifact.put 的契约。
func (t *artifactPutTool) Def() tool.Definition {
	return tool.Definition{
		Name:        nameArtifactPut,
		Namespace:   "artifact",
		Description: "把文本内容保存为产物（artifact），返回产物引用。需要保存报告、导出结果、留存文件时使用。",
		ParametersJSON: `{
			"type": "object",
			"properties": {
				"content":    {"type": "string", "description": "要保存的内容本体"},
				"media_type": {"type": "string", "description": "内容的媒体类型，默认 text/plain"},
				"summary":    {"type": "string", "description": "内容的一句话摘要；缺省时自动截取内容开头"},
				"metadata":   {"type": "object", "description": "附加元数据（如来源、标签）"}
			},
			"required": ["content"],
			"additionalProperties": false
		}`,
		Annotations: tool.Annotations{Risk: tool.RiskHigh},
	}
}

// artifactPutArgs 是 artifact.put 的参数结构。
type artifactPutArgs struct {
	// Content 要保存的内容本体。
	Content string `json:"content"`

	// MediaType 媒体类型（默认 text/plain）。
	MediaType string `json:"media_type"`

	// Summary 摘要（缺省自动截取）。
	Summary string `json:"summary"`

	// Metadata 附加元数据。
	Metadata map[string]any `json:"metadata"`
}

// Run 执行 artifact.put：内容本体写产物目录，元数据落库，返回引用。
func (t *artifactPutTool) Run(_ context.Context, argsJSON string) (string, error) {
	var args artifactPutArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", &tool.Error{Code: "BAD_ARGUMENTS", Message: "参数不是合法 JSON: " + err.Error()}
	}
	if args.Content == "" {
		return "", &tool.Error{Code: "EMPTY_CONTENT", Message: "content 不能为空"}
	}
	if args.MediaType == "" {
		args.MediaType = "text/plain"
	}
	if args.Summary == "" {
		args.Summary = summarize(args.Content)
	}

	metadata := "{}"
	if args.Metadata != nil {
		data, err := json.Marshal(args.Metadata)
		if err != nil {
			return "", &tool.Error{Code: "BAD_METADATA", Message: "metadata 无法序列化: " + err.Error()}
		}
		metadata = string(data)
	}

	a, err := t.st.PutArtifact(args.MediaType, args.Summary, metadata, []byte(args.Content))
	if err != nil {
		return "", err
	}
	return tool.OKResult(map[string]any{
		"artifact_id": a.ID,
		"uri":         a.URI,
		"summary":     a.Summary,
	})
}

// summarize 从内容开头截取摘要（前 50 字，压缩空白）。
func summarize(content string) string {
	text := strings.Join(strings.Fields(content), " ")
	runes := []rune(text)
	if len(runes) > summaryRuneLimit {
		return string(runes[:summaryRuneLimit]) + "…"
	}
	return text
}

// artifactGetTool 按 ID 读取产物元数据与内容（risk=medium：读产物内容）。
type artifactGetTool struct {
	// st 元数据存储。
	st *store.Store
}

// Def 返回 artifact.get 的契约。
func (t *artifactGetTool) Def() tool.Definition {
	return tool.Definition{
		Name:        nameArtifactGet,
		Namespace:   "artifact",
		Description: "按产物 ID 读取产物的元数据与内容本体。需要查看已保存产物的详细内容时使用。",
		ParametersJSON: `{
			"type": "object",
			"properties": {
				"artifact_id": {"type": "string", "description": "产物 ID（art- 前缀）"}
			},
			"required": ["artifact_id"],
			"additionalProperties": false
		}`,
		Annotations: tool.Annotations{Risk: tool.RiskMedium, Idempotent: true},
	}
}

// artifactGetArgs 是 artifact.get 的参数结构。
type artifactGetArgs struct {
	// ArtifactID 产物 ID。
	ArtifactID string `json:"artifact_id"`
}

// Run 执行 artifact.get：产物不存在返回结构化 ARTIFACT_NOT_FOUND。
func (t *artifactGetTool) Run(_ context.Context, argsJSON string) (string, error) {
	var args artifactGetArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", &tool.Error{Code: "BAD_ARGUMENTS", Message: "参数不是合法 JSON: " + err.Error()}
	}

	a, content, err := t.st.GetArtifact(args.ArtifactID)
	if errors.Is(err, store.ErrNotFound) {
		return "", &tool.Error{Code: "ARTIFACT_NOT_FOUND", Message: fmt.Sprintf("产物 %q 不存在", args.ArtifactID)}
	}
	if err != nil {
		return "", err
	}
	return tool.OKResult(map[string]any{
		"artifact_id": a.ID,
		"uri":         a.URI,
		"media_type":  a.MediaType,
		"summary":     a.Summary,
		"metadata":    json.RawMessage(a.Metadata),
		"size":        a.Size,
		"created_at":  a.CreatedAt.Format(time.RFC3339),
		"content":     string(content),
	})
}

// artifactListTool 列举产物清单（risk=low：只读元数据，不返回内容本体）。
// 为什么需要它：只有 put/get 时模型无法回答"当前有哪些产物"（get 需要
// 具体 ID），列举是产物命名空间的最小完备能力；内容本体仍只能经
// artifact.get 按 ID 读取（引用传递纪律，见 04 §8）。
type artifactListTool struct {
	// st 元数据存储。
	st *store.Store
}

// Def 返回 artifact.list 的契约。
func (t *artifactListTool) Def() tool.Definition {
	return tool.Definition{
		Name:        nameArtifactList,
		Namespace:   "artifact",
		Description: "列举当前已保存的产物清单（ID/类型/摘要/大小/时间，按创建时间倒序）。询问「有哪些产物/产物列表」时使用；要读具体产物内容再用 artifact.get。",
		ParametersJSON: `{
			"type": "object",
			"properties": {
				"limit": {"type": "integer", "description": "返回条数上限（默认 20，最大 100）", "minimum": 1, "maximum": 100}
			},
			"additionalProperties": false
		}`,
		Annotations: tool.Annotations{Risk: tool.RiskLow, Idempotent: true},
	}
}

// artifactListArgs 是 artifact.list 的参数结构。
type artifactListArgs struct {
	// Limit 返回条数上限。
	Limit int `json:"limit"`
}

// Run 执行 artifact.list：返回产物元数据清单（不含内容本体）。
func (t *artifactListTool) Run(_ context.Context, argsJSON string) (string, error) {
	var args artifactListArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", &tool.Error{Code: "BAD_ARGUMENTS", Message: "参数不是合法 JSON: " + err.Error()}
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	arts, err := t.st.ListArtifacts(limit, 0)
	if err != nil {
		return "", err
	}
	items := make([]map[string]any, 0, len(arts))
	for _, a := range arts {
		items = append(items, map[string]any{
			"artifact_id": a.ID,
			"media_type":  a.MediaType,
			"summary":     a.Summary,
			"metadata":    json.RawMessage(a.Metadata),
			"size":        a.Size,
			"created_at":  a.CreatedAt.Format(time.RFC3339),
		})
	}
	return tool.OKResult(map[string]any{
		"total": len(items),
		"items": items,
	})
}
