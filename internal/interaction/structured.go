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

package interaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
)

type StructuredRequest struct {
	InteractionMode string
	ProjectID       string
	SessionID       string
	WorkflowID      string
	TaskID          string
	RunID           string
	SourceAgentID   string
	TargetAgentID   string
	SourceRevision  int64
	Type            string
	UIKind          string
	Prompt          string
	Payload         json.RawMessage
	Candidates      json.RawMessage
	ResponseSchema  json.RawMessage
	MapID           string
	MapGeneration   int64
	ExpiresAt       *time.Time
	// AllowOther 仅用于单选/多选。nil 和 true 都允许用户在预设候选之外
	// 填写一个明确的自由文本值；false 用于必须严格命中候选 ID 的场景。
	AllowOther *bool
}

type StructuredPayload struct {
	InteractionMode string          `json:"interaction_mode,omitempty"`
	InteractionID   string          `json:"interaction_id"`
	WorkflowID      string          `json:"workflow_id,omitempty"`
	TaskID          string          `json:"task_id,omitempty"`
	SourceRevision  int64           `json:"source_revision"`
	Type            string          `json:"type"`
	UIKind          string          `json:"ui_kind,omitempty"`
	Prompt          string          `json:"prompt"`
	Data            json.RawMessage `json:"data,omitempty"`
	Candidates      json.RawMessage `json:"candidates,omitempty"`
	ResponseSchema  json.RawMessage `json:"response_schema,omitempty"`
	ExpiresAt       *time.Time      `json:"expires_at,omitempty"`
	AllowOther      bool            `json:"allow_other"`
}

type PlanSuggestionRequest struct {
	UserID             string
	ProjectID          string
	SessionID          string
	RunID              string
	SourceAgentID      string
	Goal               string
	Summary            string
	ApprovedScope      json.RawMessage
	Constraints        json.RawMessage
	CompletionCriteria json.RawMessage
	MapBinding         json.RawMessage
	// Tasks 与 Dependencies 只在用户显式进入 Plan Mode 后使用。此时
	// Conversation 中的 Leader 已经完成需求澄清并形成主要 TODO，工具应把
	// 这些结构直接交给 Proposal，而不是再启动第二个模型重新猜一次计划。
	Tasks        []store.TaskDraft
	Dependencies []store.TaskDependency
}

// AgentQuestionField 是 interaction.ask 允许模型描述的最小字段集合。Server
// 根据这些字段生成响应 Schema；模型不能传入任意 JSON Schema，从而让 Web、
// AnswerRouter 与下一轮 Agent Run 始终面对同一份可解释数据。
type AgentQuestionField struct {
	Name        string                `json:"name"`
	Label       string                `json:"label"`
	Description string                `json:"description,omitempty"`
	ValueType   string                `json:"value_type"`
	Required    bool                  `json:"required"`
	Options     []AgentQuestionOption `json:"options,omitempty"`
}

type AgentQuestionOption struct {
	Value       string `json:"value"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type AgentQuestionRequest struct {
	UserID          string
	InteractionMode string
	ProjectID       string
	SessionID       string
	WorkflowID      string
	TaskID          string
	RunID           string
	SourceAgentID   string
	UIKind          string
	Prompt          string
	Fields          []AgentQuestionField
	AllowOther      *bool
}

// CreateAgentQuestion 从已校验的 Agent Run scope 创建持久 Interaction。归属、
// Agent、Task 与 revision 均由执行环境提供，模型只负责问题和呈现字段；回答后
// AnswerRouter 会创建新的 Run，因此这里绝不阻塞当前模型请求。
func (s *Service) CreateAgentQuestion(ctx context.Context,
	req AgentQuestionRequest) (store.Interaction, error) {
	run, err := s.st.GetRunSession(req.RunID)
	if err != nil || run.Status != store.RunStatusRunning || run.ProjectID != req.ProjectID ||
		run.ChatSessionID != req.SessionID || run.AgentID != req.SourceAgentID ||
		run.WorkflowID != req.WorkflowID || run.TaskID != req.TaskID {
		return store.Interaction{}, store.ErrInvalidState
	}
	conversation, err := s.st.GetChatSession(req.SessionID)
	if err != nil || conversation.ProjectID != req.ProjectID {
		return store.Interaction{}, store.ErrNotFound
	}
	sourceRevision := conversation.Revision
	if req.TaskID != "" {
		task, taskErr := s.st.GetTask(req.TaskID)
		if taskErr != nil || task.WorkflowID != req.WorkflowID {
			return store.Interaction{}, store.ErrNotFound
		}
		sourceRevision = task.Revision
	}
	interactionType, schema, candidates, payload, err := buildAgentQuestion(req)
	if err != nil {
		return store.Interaction{}, err
	}
	return s.CreateStructured(ctx, StructuredRequest{
		ProjectID: req.ProjectID, SessionID: req.SessionID, WorkflowID: req.WorkflowID,
		InteractionMode: req.InteractionMode,
		TaskID:          req.TaskID, RunID: req.RunID, SourceAgentID: req.SourceAgentID,
		SourceRevision: sourceRevision, Type: interactionType, UIKind: req.UIKind,
		Prompt: req.Prompt, Payload: payload, Candidates: candidates,
		ResponseSchema: schema, AllowOther: req.AllowOther,
	})
}

func buildAgentQuestion(req AgentQuestionRequest) (string, json.RawMessage,
	json.RawMessage, json.RawMessage, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return "", nil, nil, nil, store.ErrInvalidState
	}
	if req.UIKind == "confirm" {
		if len(req.Fields) != 0 {
			return "", nil, nil, nil, fmt.Errorf("confirm 不接受 fields: %w", store.ErrInvalidState)
		}
		return store.InteractionTypeConfirm,
			json.RawMessage(`{"type":"object","required":["approved"],"properties":{"approved":{"type":"boolean"}},"additionalProperties":false}`),
			nil, nil, nil
	}
	if !store.ValidInteractionUIKind(req.UIKind) || len(req.Fields) == 0 {
		return "", nil, nil, nil, store.ErrInvalidState
	}
	if req.UIKind != store.InteractionUIForm && len(req.Fields) != 1 {
		return "", nil, nil, nil, fmt.Errorf("%s 只能定义一个回答字段: %w", req.UIKind, store.ErrInvalidState)
	}
	properties := make(map[string]any, len(req.Fields))
	required := make([]string, 0, len(req.Fields))
	seen := make(map[string]struct{}, len(req.Fields))
	for _, field := range req.Fields {
		name := strings.TrimSpace(field.Name)
		if name == "" || strings.TrimSpace(field.Label) == "" {
			return "", nil, nil, nil, store.ErrInvalidState
		}
		if _, exists := seen[name]; exists {
			return "", nil, nil, nil, fmt.Errorf("Interaction 字段重复: %s", name)
		}
		seen[name] = struct{}{}
		valueType := strings.TrimSpace(field.ValueType)
		if valueType == "" {
			valueType = "string"
		}
		property := map[string]any{"type": valueType, "title": strings.TrimSpace(field.Label)}
		if field.Description != "" {
			property["description"] = strings.TrimSpace(field.Description)
		}
		if req.UIKind == store.InteractionUIMultiSelect {
			property["type"] = "array"
			property["items"] = map[string]any{"type": "string"}
			valueType = "array"
		}
		if valueType != "string" && valueType != "number" && valueType != "integer" &&
			valueType != "boolean" && valueType != "array" {
			return "", nil, nil, nil, fmt.Errorf("不支持的回答字段类型: %s", valueType)
		}
		if req.UIKind == store.InteractionUISingleSelect || req.UIKind == store.InteractionUIMultiSelect {
			if len(field.Options) == 0 {
				return "", nil, nil, nil, fmt.Errorf("选择类 Interaction 必须提供 options: %w", store.ErrInvalidState)
			}
			values := make([]string, 0, len(field.Options))
			for _, option := range field.Options {
				if strings.TrimSpace(option.Value) == "" || strings.TrimSpace(option.Label) == "" {
					return "", nil, nil, nil, store.ErrInvalidState
				}
				values = append(values, option.Value)
			}
			if req.UIKind == store.InteractionUIMultiSelect {
				property["items"].(map[string]any)["enum"] = values
			} else {
				property["enum"] = values
			}
		}
		properties[name] = property
		if field.Required {
			required = append(required, name)
		}
	}
	schemaObject := map[string]any{"type": "object", "properties": properties,
		"additionalProperties": false}
	if len(required) > 0 {
		schemaObject["required"] = required
	}
	schema, _ := json.Marshal(schemaObject)
	payload, _ := json.Marshal(map[string]any{"fields": req.Fields})
	var candidates json.RawMessage
	if len(req.Fields) == 1 && len(req.Fields[0].Options) > 0 {
		candidates, _ = json.Marshal(req.Fields[0].Options)
	}
	return store.InteractionTypeInput, schema, candidates, payload, nil
}

// CreateStructured 创建可跨重启恢复的 confirm/input。它不阻塞调用方；
// 需要继续 Agent 时由 AnswerRouter 创建新 Run。
func (s *Service) CreateStructured(_ context.Context, req StructuredRequest) (store.Interaction, error) {
	project, err := s.st.GetProject(req.ProjectID)
	if err != nil || project.ArchivedAt != nil || !project.IsActive {
		return store.Interaction{}, store.ErrProjectInactive
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return store.Interaction{}, store.ErrInvalidState
	}
	if req.Type != store.InteractionTypeConfirm && req.Type != store.InteractionTypeInput {
		return store.Interaction{}, store.ErrInvalidState
	}
	if req.Type == store.InteractionTypeInput && !store.ValidInteractionUIKind(req.UIKind) {
		return store.Interaction{}, store.ErrInvalidState
	}
	if req.Type == store.InteractionTypeConfirm {
		req.UIKind = ""
	}
	if len(req.ResponseSchema) == 0 {
		if req.Type == store.InteractionTypeConfirm {
			req.ResponseSchema = json.RawMessage(`{"type":"object","required":["approved"],"properties":{"approved":{"type":"boolean"}},"additionalProperties":false}`)
		} else {
			return store.Interaction{}, fmt.Errorf("input response_schema 不能为空: %w", store.ErrInvalidState)
		}
	}
	if _, err := compileResponseSchema(req.ResponseSchema); err != nil {
		return store.Interaction{}, err
	}
	id := store.NewInteractionID()
	if len(req.Candidates) == 0 {
		req.Candidates = structuredCandidates(req.Payload)
	}
	allowOther := (req.UIKind == store.InteractionUISingleSelect || req.UIKind == store.InteractionUIMultiSelect) &&
		(req.AllowOther == nil || *req.AllowOther)
	payload := StructuredPayload{InteractionID: id, InteractionMode: req.InteractionMode, WorkflowID: req.WorkflowID, TaskID: req.TaskID, SourceRevision: req.SourceRevision, Type: req.Type, UIKind: req.UIKind, Prompt: strings.TrimSpace(req.Prompt), Data: req.Payload, Candidates: req.Candidates, ResponseSchema: req.ResponseSchema, ExpiresAt: req.ExpiresAt, AllowOther: allowOther}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return store.Interaction{}, err
	}
	now := time.Now().UTC()
	value := store.Interaction{ID: id, ProjectID: req.ProjectID, SessionID: req.SessionID, WorkflowID: req.WorkflowID, TaskID: req.TaskID, RunID: req.RunID, Agent: req.SourceAgentID, TargetAgentID: req.TargetAgentID, SourceRevision: req.SourceRevision, Type: req.Type, UIKind: req.UIKind, Status: store.InteractionStatusPending, Payload: string(encoded), ResponseSchema: string(req.ResponseSchema), MapID: req.MapID, MapGeneration: req.MapGeneration, CreatedAt: now, ExpiresAt: req.ExpiresAt}
	if err := s.st.CreateInteraction(value); err != nil {
		return store.Interaction{}, err
	}
	value, err = s.st.GetInteraction(id)
	if err != nil {
		return store.Interaction{}, err
	}
	env := ws.NewEnvelope(value.SessionID, ws.ChannelInteraction, EventTypeInteractionRequest, ws.ImportanceCritical, payload)
	s.decorateEnvelope(&env, value)
	s.bus.Publish(event.TopicAgentEvents, env)
	return value, nil
}

func compileResponseSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("response_schema 不是合法 JSON: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("interaction.json", doc); err != nil {
		return nil, err
	}
	schema, err := compiler.Compile("interaction.json")
	if err != nil {
		return nil, fmt.Errorf("response_schema 编译失败: %w", err)
	}
	return schema, nil
}
func validateResponse(schemaText string, response json.RawMessage) error {
	schema, err := compileResponseSchema(json.RawMessage(schemaText))
	if err != nil {
		return err
	}
	value, err := jsonschema.UnmarshalJSON(strings.NewReader(string(response)))
	if err != nil {
		return fmt.Errorf("response 不是合法 JSON: %w", err)
	}
	if err := schema.Validate(value); err != nil {
		return fmt.Errorf("response 不满足 schema: %w", err)
	}
	return nil
}

// ReplyStructured 先做归属、revision、schema 与资源校验，再原子应答。
func (s *Service) ReplyStructured(_ context.Context, userID, projectID, interactionID string, response json.RawMessage, expectedStateRevision int64) error {
	if err := s.st.ProjectWritableByUser(userID, projectID); err != nil {
		return err
	}
	value, err := s.st.GetInteraction(interactionID)
	if err != nil || value.ProjectID != projectID {
		return ErrNotFound
	}
	if value.Status != store.InteractionStatusPending {
		return ErrNotPending
	}
	if value.ExpiresAt != nil && !time.Now().UTC().Before(*value.ExpiresAt) {
		_ = s.st.ExpireInteraction(value.ID, time.Now().UTC())
		return ErrNotPending
	}
	if value.SourceRevision > 0 && value.SourceRevision != expectedStateRevision {
		return store.ErrRevisionConflict
	}
	// 工具审批由当前 Run 的 waiter 在原断点继续执行，不是一个新的 Task
	// 交互续跑。checkpoint_id 是这类同步审批的明确标识；它没有结构化 Task
	// Interaction 的 schema，也绝不能在回答后创建新的 Workflow attempt。
	if value.CheckpointID != "" {
		return s.replyCheckpointApproval(value, response)
	}
	responseSchema := value.ResponseSchema
	if (value.UIKind == store.InteractionUISingleSelect || value.UIKind == store.InteractionUIMultiSelect) && structuredAllowsOther(value.Payload) {
		responseSchema = selectionSchemaAllowOther(responseSchema, value.UIKind)
	}
	if err := validateResponse(responseSchema, response); err != nil {
		return err
	}
	if err := s.validateStructuredResources(userID, value, response); err != nil {
		return err
	}
	if err := s.st.AnswerInteractionAtRevision(value.ID, string(response), expectedStateRevision, time.Now().UTC()); errors.Is(err, store.ErrNotPending) {
		return ErrNotPending
	} else if err != nil {
		return err
	}
	answered, err := s.st.GetInteraction(value.ID)
	if err != nil {
		return err
	}
	s.publishStructuredResolved(answered, response)
	s.wakeLegacyConfirm(answered, response)
	if err := s.routeInteractionAnswer(context.Background(), answered); err != nil {
		s.logger.Error("Interaction answer routing deferred", "interaction_id", answered.ID, "error", err)
	}
	return nil
}

// CancelStructured 终结当前问题，但不替用户选择答案，也不自动停止Workflow。
// 原Agent会在新的continuation Run中收到cancelled结果，自行决定改写计划、
// 提出更合适的问题或在批准边界内结束当前Task。
func (s *Service) CancelStructured(_ context.Context, userID, projectID,
	interactionID string) error {
	value, err := s.st.GetInteraction(interactionID)
	if err != nil || value.ProjectID != projectID || value.CheckpointID != "" {
		return ErrNotFound
	}
	project, err := s.st.GetProject(projectID)
	if err != nil || project.OwnerID != userID || !project.IsActive || project.ArchivedAt != nil {
		return store.ErrProjectInactive
	}
	if value.Status != store.InteractionStatusPending {
		return ErrNotPending
	}
	if value.TaskID != "" {
		task, taskErr := s.st.GetTask(value.TaskID)
		if taskErr != nil || task.WorkflowID != value.WorkflowID ||
			task.Status != store.TaskStatusPaused || task.WaitingReason != "waiting_input" {
			return store.ErrRevisionConflict
		}
	}
	if err := s.st.CancelInteraction(value.ID, time.Now().UTC()); err != nil {
		return err
	}
	cancelled, err := s.st.GetInteraction(value.ID)
	if err != nil {
		return err
	}
	empty := json.RawMessage(`{}`)
	s.publishStructuredResolved(cancelled, empty)
	if err := s.routeInteractionAnswer(context.Background(), cancelled); err != nil {
		s.logger.Error("Interaction cancel routing deferred", "interaction_id", cancelled.ID,
			"error", err)
		return nil
	}
	return nil
}

// replyCheckpointApproval 完成同步工具审批并唤醒原 Run。Server 重启后 waiter
// 可能已不存在，此时仍把回答标记为 handled，但不会重新执行旧 checkpoint。
func (s *Service) replyCheckpointApproval(value store.Interaction, response json.RawMessage) error {
	var reply struct {
		Approved *bool `json:"approved"`
	}
	if err := json.Unmarshal(response, &reply); err != nil || reply.Approved == nil {
		return fmt.Errorf("工具审批响应必须包含 approved: %w", store.ErrInvalidState)
	}
	if err := s.st.AnswerInteractionAtRevision(value.ID, string(response),
		value.SourceRevision, time.Now().UTC()); errors.Is(err, store.ErrNotPending) {
		return ErrNotPending
	} else if err != nil {
		return err
	}
	answered, err := s.st.GetInteraction(value.ID)
	if err != nil {
		return err
	}
	s.publishStructuredResolved(answered, response)
	s.wakeLegacyConfirm(answered, response)
	return s.st.MarkInteractionHandled(answered.ID, time.Now().UTC(), "")
}

func (s *Service) validateStructuredResources(userID string, value store.Interaction, response json.RawMessage) error {
	if value.TaskID != "" {
		task, err := s.st.GetTask(value.TaskID)
		if err != nil || task.WorkflowID != value.WorkflowID ||
			task.Status != store.TaskStatusPaused || task.WaitingReason != "waiting_input" ||
			value.SourceRevision <= 0 || value.SourceRevision > task.Revision {
			return store.ErrRevisionConflict
		}
	} else if value.WorkflowID != "" {
		workflow, err := s.st.GetWorkflow(value.WorkflowID)
		if err != nil || workflow.ProjectID != value.ProjectID || workflow.Revision != value.SourceRevision {
			return store.ErrRevisionConflict
		}
	} else if value.SourceRevision > 0 {
		conversation, err := s.st.GetChatSession(value.SessionID)
		if err != nil || conversation.ProjectID != value.ProjectID || conversation.Revision != value.SourceRevision {
			return store.ErrRevisionConflict
		}
	}
	switch value.UIKind {
	case store.InteractionUIMapSelect:
		var body struct {
			MapID      string `json:"map_id"`
			Generation int64  `json:"generation"`
			Selections []struct {
				Kind     string    `json:"kind"`
				EntityID string    `json:"entity_id"`
				FrameID  string    `json:"frame_id"`
				Position []float64 `json:"position"`
			} `json:"selections"`
		}
		if err := json.Unmarshal(response, &body); err != nil {
			return err
		}
		if body.MapID != value.MapID || body.Generation != value.MapGeneration {
			return store.ErrStaleMapGeneration
		}
		ids := make([]string, 0)
		allowed := structuredCandidateIDs(value.Payload, "entity_id")
		if len(body.Selections) == 0 {
			return store.ErrInvalidState
		}
		for _, selection := range body.Selections {
			switch selection.Kind {
			case "entity", "region":
				if selection.EntityID == "" {
					return store.ErrInvalidState
				}
				if len(allowed) > 0 {
					if _, ok := allowed[selection.EntityID]; !ok {
						return store.ErrInvalidState
					}
				}
				ids = append(ids, selection.EntityID)
			case "point":
				if selection.FrameID == "" || len(selection.Position) != 3 {
					return store.ErrInvalidState
				}
			default:
				return store.ErrInvalidState
			}
		}
		return s.st.ValidateMapReference(value.ProjectID, body.MapID, body.Generation, ids)
	case store.InteractionUIImageSelect, store.InteractionUIFileSelect:
		var body struct {
			ArtifactID  string   `json:"artifact_id"`
			ArtifactIDs []string `json:"artifact_ids"`
		}
		if err := json.Unmarshal(response, &body); err != nil {
			return err
		}
		ids := append([]string(nil), body.ArtifactIDs...)
		if body.ArtifactID != "" {
			ids = append(ids, body.ArtifactID)
		}
		if len(ids) == 0 {
			return store.ErrInvalidState
		}
		allowed := structuredCandidateIDs(value.Payload, "artifact_id")
		if len(allowed) == 0 {
			return store.ErrInvalidState
		}
		for _, id := range ids {
			if _, ok := allowed[id]; !ok {
				return store.ErrInvalidState
			}
			artifact, err := s.st.GetArtifactMeta(id)
			if err != nil || artifact.OwnerID != userID {
				return store.ErrNotFound
			}
			if value.UIKind == store.InteractionUIImageSelect && !strings.HasPrefix(artifact.MediaType, "image/") {
				return store.ErrInvalidState
			}
		}
	case store.InteractionUISingleSelect, store.InteractionUIMultiSelect:
		allowed := structuredCandidateIDs(value.Payload, "value")
		if len(allowed) == 0 {
			return store.ErrInvalidState
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(response, &body); err != nil {
			return err
		}
		field := selectionResponseField(value.ResponseSchema, value.UIKind)
		var values []string
		if value.UIKind == store.InteractionUIMultiSelect {
			if err := json.Unmarshal(body[field], &values); err != nil {
				return store.ErrInvalidState
			}
		} else {
			var selected string
			if err := json.Unmarshal(body[field], &selected); err != nil {
				return store.ErrInvalidState
			}
			values = append(values, selected)
		}
		if len(values) == 0 {
			return store.ErrInvalidState
		}
		allowOther := structuredAllowsOther(value.Payload)
		for _, candidate := range values {
			if strings.TrimSpace(candidate) == "" {
				return store.ErrInvalidState
			}
			if _, ok := allowed[candidate]; !ok && !allowOther {
				return store.ErrInvalidState
			}
		}
	}
	return nil
}

func structuredAllowsOther(payload string) bool {
	var envelope StructuredPayload
	return json.Unmarshal([]byte(payload), &envelope) == nil && envelope.AllowOther
}

// selectionResponseField 从选择类Interaction的单字段Schema读取真实字段名。
// Agent可以使用confirm_resubmit等业务名称，Server和Web都不能擅自改回value。
func selectionResponseField(schemaText string, uiKind string) string {
	fallback := "value"
	if uiKind == store.InteractionUIMultiSelect {
		fallback = "values"
	}
	var schema map[string]any
	if json.Unmarshal([]byte(schemaText), &schema) != nil {
		return fallback
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok || len(properties) != 1 {
		return fallback
	}
	for name := range properties {
		return name
	}
	return fallback
}

// selectionSchemaAllowOther 只移除选择值上的 enum/const，保留 object、required、
// array 长度等其余schema限制。这样“其他”仍使用原始业务字段名，不会因为
// 允许自由文本而放宽整个Interaction响应。
func selectionSchemaAllowOther(schemaText string, uiKind string) string {
	var schema map[string]any
	if json.Unmarshal([]byte(schemaText), &schema) != nil {
		return schemaText
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return schemaText
	}
	field := selectionResponseField(schemaText, uiKind)
	property, ok := properties[field].(map[string]any)
	if !ok {
		return schemaText
	}
	if uiKind == store.InteractionUIMultiSelect {
		if items, ok := property["items"].(map[string]any); ok {
			delete(items, "enum")
			delete(items, "const")
		}
	} else {
		delete(property, "enum")
		delete(property, "const")
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return schemaText
	}
	return string(encoded)
}

func structuredCandidates(data json.RawMessage) json.RawMessage {
	var value map[string]json.RawMessage
	if json.Unmarshal(data, &value) != nil {
		return nil
	}
	for _, key := range []string{"candidates", "options"} {
		if len(value[key]) > 0 {
			return value[key]
		}
	}
	return nil
}

func structuredCandidateIDs(payload string, key string) map[string]struct{} {
	var envelope StructuredPayload
	if json.Unmarshal([]byte(payload), &envelope) != nil {
		return nil
	}
	candidates := envelope.Candidates
	if len(candidates) == 0 {
		candidates = structuredCandidates(envelope.Data)
	}
	var items []map[string]any
	if json.Unmarshal(candidates, &items) != nil {
		return nil
	}
	result := map[string]struct{}{}
	for _, item := range items {
		if id, ok := item[key].(string); ok && id != "" {
			result[id] = struct{}{}
		}
		if key == "artifact_id" {
			if id, ok := item["id"].(string); ok && id != "" {
				result[id] = struct{}{}
			}
		}
	}
	return result
}

func (s *Service) publishStructuredResolved(value store.Interaction, response json.RawMessage) {
	env := ws.NewEnvelope(value.SessionID, ws.ChannelInteraction, EventTypeInteractionResolved, ws.ImportanceCritical, map[string]any{"interaction_id": value.ID, "status": value.Status, "reply": response, "revision": value.Revision})
	s.decorateEnvelope(&env, value)
	s.bus.Publish(event.TopicAgentEvents, env)
}
func (s *Service) wakeLegacyConfirm(value store.Interaction, response json.RawMessage) {
	if value.Type != store.InteractionTypeConfirm {
		return
	}
	var reply replyPayload
	if json.Unmarshal(response, &reply) != nil {
		return
	}
	s.mu.Lock()
	ch, ok := s.waiters[value.ID]
	s.mu.Unlock()
	if ok {
		select {
		case ch <- waitResult{approved: reply.Approved}:
		default:
		}
	}
}
