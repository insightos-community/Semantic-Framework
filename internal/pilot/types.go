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

// Package pilot 实现 Robot Skill Worker、Action 路由、停止和恢复边界。
package pilot

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	ErrRobotNotFound       = errors.New("robot profile not found")
	ErrActionNotBound      = errors.New("action is not bound to robot")
	ErrInterfaceMismatch   = errors.New("action schema version mismatch")
	ErrRobotBusy           = errors.New("robot already has a foreground physical action")
	ErrExecutionNotFound   = errors.New("pilot execution not found")
	ErrActionKeyConflict   = errors.New("action key is already bound to different content")
	ErrReplayForbidden     = errors.New("interrupted physical action cannot be replayed")
	ErrManualCheckRequired = errors.New("physical state requires manual confirmation")
	ErrSkillInvalid        = errors.New("robot skill package is invalid")
)

// ActionRef 是 Skill 与 Pilot 共同使用的稳定操作标识，不包含 Ability 包名。
type ActionRef struct {
	Type          string `json:"type" yaml:"type"`
	SchemaVersion int    `json:"schema_version" yaml:"schema_version"`
}

func (r ActionRef) Key() string { return fmt.Sprintf("%s@%d", r.Type, r.SchemaVersion) }

// AbilityBinding 把某台 Robot 的 Action 精确绑定到一个 Ability 实例和 Task。
type AbilityBinding struct {
	Action      ActionRef
	AbilityName string
	TaskName    string
	InstanceID  string
	Physical    bool
}

type RobotProfile struct {
	RobotID  string
	Bindings map[string]AbilityBinding
}

// ActionRequest 来自一个已通过 SkillCatalog 校验的 Worker。
type ActionRequest struct {
	SkillExecutionID string
	Key              string
	RobotID          string
	Action           ActionRef
	Input            map[string]any
	Timeout          time.Duration
	FeedbackInterval time.Duration
	StopAction       bool
}

type AbilityTask struct {
	TaskID string `json:"task_id"`
}

type AbilityFeedback struct {
	Sequence     int64            `json:"sequence"`
	Status       string           `json:"status"`
	Phase        string           `json:"phase,omitempty"`
	Progress     float64          `json:"progress"`
	Message      string           `json:"message"`
	Severity     string           `json:"severity"`
	Measurements map[string]any   `json:"measurements,omitempty"`
	Observations []map[string]any `json:"observations,omitempty"`
	EvidenceRefs []string         `json:"evidence_refs,omitempty"`
}

type AbilityExecution struct {
	Status       string            `json:"status"`
	Feedback     []AbilityFeedback `json:"feedback"`
	Observations []map[string]any  `json:"observations"`
	Result       map[string]any    `json:"result"`
	Error        map[string]any    `json:"error"`
}

// AbilityClient 调用精确 AbilityFramework 实例；实现不得按名称回退选择其他实例。
type AbilityClient interface {
	StartTask(ctx context.Context, instanceID, taskName string, input map[string]any) (AbilityTask, error)
	GetExecution(ctx context.Context, instanceID, invocationID string, afterSequence int64) (AbilityExecution, error)
	StopExecution(ctx context.Context, instanceID, invocationID, reason string) (AbilityExecution, error)
}

type ActionStatus string

const (
	ActionAccepted    ActionStatus = "accepted"
	ActionRunning     ActionStatus = "running"
	ActionStopping    ActionStatus = "stopping"
	ActionSucceeded   ActionStatus = "succeeded"
	ActionFailed      ActionStatus = "failed"
	ActionStopped     ActionStatus = "stopped"
	ActionInterrupted ActionStatus = "interrupted"
)

type ActionExecution struct {
	ID               string
	SkillExecutionID string
	Key              string
	RequestJSON      string
	RobotID          string
	Action           ActionRef
	AbilityName      string
	TaskName         string
	InstanceID       string
	InvocationID     string
	FrameworkTaskID  string
	Physical         bool
	Status           ActionStatus
	FeedbackCursor   int64
	Feedback         []AbilityFeedback
	Observations     []map[string]any
	Result           map[string]any
	Error            map[string]any
	StartedAt        time.Time
	UpdatedAt        time.Time
}

type SkillExecutionStatus string

const (
	SkillQueued       SkillExecutionStatus = "queued"
	SkillStarting     SkillExecutionStatus = "starting"
	SkillRunning      SkillExecutionStatus = "running"
	SkillWaitingAgent SkillExecutionStatus = "waiting_agent"
	SkillStopping     SkillExecutionStatus = "stopping"
	SkillCompleted    SkillExecutionStatus = "completed"
	SkillFailed       SkillExecutionStatus = "failed"
	SkillStopped      SkillExecutionStatus = "stopped"
	SkillInterrupted  SkillExecutionStatus = "interrupted"
)

type SkillExecution struct {
	ID              string
	ProjectID       string
	TaskID          string
	SubtaskID       string
	RobotID         string
	SkillName       string
	SkillVersion    string
	Status          SkillExecutionStatus
	Input           map[string]any
	Checkpoint      map[string]any
	FeedbackCursors map[string]int64
	Result          map[string]any
	Error           map[string]any
	StopOutcome     map[string]any
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type AgentRequest struct {
	ExecutionID      string
	SkillName        string
	Stage            string
	DecisionKey      string
	DecisionRevision int
	Reason           string
	Context          map[string]any
	ResponseModel    string
	ResponseSchema   map[string]any
	CreatedAt        time.Time
	Response         map[string]any
	ResolvedAt       *time.Time
}

type AgentReply struct {
	ExecutionID      string         `json:"execution_id"`
	SkillName        string         `json:"skill_name"`
	Stage            string         `json:"stage"`
	DecisionKey      string         `json:"decision_key"`
	DecisionRevision int            `json:"decision_revision"`
	Payload          map[string]any `json:"payload"`
}

type AgentGateway interface {
	Request(ctx context.Context, request AgentRequest) (AgentReply, error)
}

type ObservationSource interface {
	Get(ctx context.Context, robotID, observationRef string) (map[string]any, error)
	Latest(ctx context.Context, robotID, kind, subjectRef string, maxAge time.Duration) (map[string]any, error)
}

// ArtifactService 只向当前 Skill Execution 暴露已经授权的引用和工作目录。
type ArtifactService interface {
	Resolve(context.Context, SkillExecution, string) (map[string]any, error)
	Publish(context.Context, SkillExecution, string, string, string) (map[string]any, error)
}

type ArtifactWorkspace interface {
	Workspace(SkillExecution) (string, error)
}

// AbilityArtifactImporter 是现有 ArtifactStore 的可选扩展。Ability 只能返回
// 交换目录内的相对句柄；Pilot 校验并导入到当前 Execution workspace 后，才
// 允许进入既有上传链路。
type AbilityArtifactImporter interface {
	ImportAbilityArtifact(context.Context, SkillExecution, string, string, string) (map[string]any, error)
}
