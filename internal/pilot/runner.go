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

package pilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Runner 把 Action 精确路由到 AbilityFramework，并保存不可重放的调用记录。
type Runner struct {
	catalog *Catalog
	client  AbilityClient
	journal ActionJournal
	now     func() time.Time

	mu             sync.Mutex
	activePhysical map[string]string
	feedbackMu     sync.Mutex
	feedback       map[string]*feedbackThrottle
}

type feedbackThrottle struct {
	interval      time.Duration
	lastEmittedAt time.Time
	lastStatus    string
	lastPhase     string
	pending       *AbilityFeedback
}

func NewRunner(catalog *Catalog, client AbilityClient, journals ...ActionJournal) *Runner {
	var journal ActionJournal = NewMemoryActionJournal()
	if len(journals) > 0 && journals[0] != nil {
		journal = journals[0]
	}
	runner := &Runner{
		catalog: catalog, client: client, journal: journal, now: time.Now,
		activePhysical: make(map[string]string), feedback: make(map[string]*feedbackThrottle),
	}
	active, _ := journal.ListActiveActions()
	for _, item := range active {
		if item.Physical {
			runner.activePhysical[item.RobotID] = item.ID
		}
	}
	return runner
}

func (r *Runner) StartAction(ctx context.Context, request ActionRequest) (ActionExecution, error) {
	if request.SkillExecutionID == "" || request.Key == "" {
		return ActionExecution{}, fmt.Errorf("skill execution id and action key are required")
	}
	requestJSON, err := normalizeActionRequest(request)
	if err != nil {
		return ActionExecution{}, err
	}
	if existing, err := r.journal.GetAction(request.SkillExecutionID, request.Key); err == nil {
		if existing.RequestJSON != requestJSON {
			return ActionExecution{}, ErrActionKeyConflict
		}
		if existing.Status == ActionInterrupted {
			return existing, ErrReplayForbidden
		}
		if !actionTerminal(existing.Status) {
			r.configureFeedback(existing.ID, request.FeedbackInterval)
			return r.Refresh(ctx, existing.ID)
		}
		return existing, nil
	} else if !errors.Is(err, errJournalRecordNotFound) {
		return ActionExecution{}, err
	}

	binding, err := r.catalog.Resolve(request.RobotID, request.Action)
	if err != nil {
		return ActionExecution{}, err
	}
	if binding.Physical {
		r.mu.Lock()
		if activeID := r.activePhysical[request.RobotID]; activeID != "" {
			r.mu.Unlock()
			return ActionExecution{}, ErrRobotBusy
		}
		r.mu.Unlock()
	}

	now := r.now()
	execution := ActionExecution{
		ID:               "action-" + uuid.NewString(),
		SkillExecutionID: request.SkillExecutionID,
		Key:              request.Key,
		RequestJSON:      requestJSON,
		RobotID:          request.RobotID,
		Action:           request.Action,
		AbilityName:      binding.AbilityName,
		TaskName:         binding.TaskName,
		InstanceID:       binding.InstanceID,
		InvocationID:     "invocation-" + uuid.NewString(),
		Physical:         binding.Physical,
		Status:           ActionAccepted,
		StartedAt:        now,
		UpdatedAt:        now,
	}
	if err := r.journal.SaveAction(execution); err != nil {
		return ActionExecution{}, err
	}
	r.configureFeedback(execution.ID, request.FeedbackInterval)
	if execution.Physical {
		r.mu.Lock()
		r.activePhysical[execution.RobotID] = execution.ID
		r.mu.Unlock()
	}

	input := cloneMap(request.Input)
	if input == nil {
		input = make(map[string]any)
	}
	input["invocation_id"] = execution.InvocationID
	input["robot_id"] = request.RobotID
	// StartTask 只调用一次。即使连接失败也按可能已产生物理效果处理，不自动重试。
	task, err := r.client.StartTask(ctx, binding.InstanceID, binding.TaskName, input)
	if err != nil {
		var rejected *AbilityStartRejectedError
		if errors.As(err, &rejected) {
			execution.Status = ActionFailed
			execution.Error = map[string]any{"code": "ABILITY_START_REJECTED", "message": err.Error()}
			if execution.Physical {
				r.mu.Lock()
				delete(r.activePhysical, execution.RobotID)
				r.mu.Unlock()
			}
		} else if state, reconcileErr := r.client.GetExecution(ctx, binding.InstanceID, execution.InvocationID, 0); reconcileErr == nil && confirmedAbilityTerminal(state.Status) {
			execution = r.applyAbilityState(execution, state)
		} else {
			execution.Status = ActionInterrupted
			execution.Error = map[string]any{"code": "ABILITY_START_UNCONFIRMED", "message": err.Error()}
		}
		execution.UpdatedAt = r.now()
		_ = r.journal.SaveAction(execution)
		return execution, err
	}
	execution.FrameworkTaskID = task.TaskID
	execution.Status = ActionRunning
	execution.UpdatedAt = r.now()
	if err := r.journal.SaveAction(execution); err != nil {
		return ActionExecution{}, err
	}
	return cloneActionExecution(execution), nil
}

func (r *Runner) Refresh(ctx context.Context, executionID string) (ActionExecution, error) {
	execution, err := r.journal.GetActionByID(executionID)
	if err != nil {
		return ActionExecution{}, ErrExecutionNotFound
	}
	if actionTerminal(execution.Status) {
		return execution, nil
	}
	state, err := r.client.GetExecution(ctx, execution.InstanceID, execution.InvocationID, execution.FeedbackCursor)
	if err != nil {
		return r.markInterrupted(execution, "ABILITY_UNAVAILABLE", err.Error()), err
	}
	return r.applyAbilityState(execution, state), nil
}

func (r *Runner) Feedback(ctx context.Context, executionID string, afterSequence int64) ([]AbilityFeedback, bool, error) {
	execution, err := r.Refresh(ctx, executionID)
	if err != nil && execution.ID == "" {
		return nil, false, err
	}
	items := make([]AbilityFeedback, 0)
	for _, item := range execution.Feedback {
		if item.Sequence > afterSequence {
			items = append(items, item)
		}
	}
	return items, actionTerminal(execution.Status), err
}

func (r *Runner) Result(ctx context.Context, executionID string) (ActionExecution, error) {
	return r.Refresh(ctx, executionID)
}

func (r *Runner) Stop(ctx context.Context, executionID, reason string) (ActionExecution, error) {
	execution, err := r.journal.GetActionByID(executionID)
	if err != nil {
		return ActionExecution{}, ErrExecutionNotFound
	}
	// interrupted 表示物理结果未知，必须允许用户对原 invocation 重试停止；
	// 它禁止动作重放，但不禁止停止，否则物理锁将永远无法正常释放。
	if actionTerminal(execution.Status) && execution.Status != ActionInterrupted {
		return execution, nil
	}
	execution.Status = ActionStopping
	execution.UpdatedAt = r.now()
	if err := r.journal.SaveAction(execution); err != nil {
		return ActionExecution{}, err
	}
	state, err := r.client.StopExecution(ctx, execution.InstanceID, execution.InvocationID, reason)
	if err != nil {
		return r.markInterrupted(execution, "STOP_UNCONFIRMED", err.Error()), err
	}
	return r.applyAbilityState(execution, state), nil
}

// physicalOwner 返回尚未确认结束的物理 Action，供 Skill 保留执行归属和停止入口。
func (r *Runner) physicalOwner(robotID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activePhysical[robotID]
}

// ConfirmInterrupted 只供人工或设备侧确认现场后调用；确认前 Robot 锁保持占用。
func (r *Runner) ConfirmInterrupted(executionID string) (ActionExecution, error) {
	execution, err := r.journal.GetActionByID(executionID)
	if err != nil {
		return ActionExecution{}, ErrExecutionNotFound
	}
	if execution.Status != ActionInterrupted {
		return ActionExecution{}, ErrManualCheckRequired
	}
	r.releasePhysical(execution.RobotID, execution.ID)
	return execution, nil
}

func (r *Runner) applyAbilityState(execution ActionExecution, state AbilityExecution) ActionExecution {
	execution.Status = normalizeActionStatus(state.Status)
	execution = r.applyFeedback(execution, state.Feedback)
	execution.Observations = cloneMaps(state.Observations)
	execution.Result = cloneMap(state.Result)
	execution.Error = cloneMap(state.Error)
	execution.UpdatedAt = r.now()
	_ = r.journal.SaveAction(execution)
	if actionTerminal(execution.Status) && execution.Status != ActionInterrupted {
		r.releasePhysical(execution.RobotID, execution.ID)
	}
	return cloneActionExecution(execution)
}

func (r *Runner) configureFeedback(executionID string, interval time.Duration) {
	r.feedbackMu.Lock()
	defer r.feedbackMu.Unlock()
	if _, exists := r.feedback[executionID]; !exists {
		r.feedback[executionID] = &feedbackThrottle{interval: interval}
	}
}

func (r *Runner) actionPollInterval(executionID string) time.Duration {
	r.feedbackMu.Lock()
	defer r.feedbackMu.Unlock()
	interval := 200 * time.Millisecond
	if throttle := r.feedback[executionID]; throttle != nil && throttle.interval > interval {
		interval = throttle.interval
	}
	return interval
}

func (r *Runner) applyFeedback(execution ActionExecution, incoming []AbilityFeedback) ActionExecution {
	r.configureFeedback(execution.ID, 0)
	now := r.now()
	r.feedbackMu.Lock()
	defer r.feedbackMu.Unlock()
	throttle := r.feedback[execution.ID]
	emit := func(item AbilityFeedback) {
		execution.Feedback = append(execution.Feedback, item)
		throttle.lastEmittedAt = now
		throttle.lastStatus = item.Status
		throttle.lastPhase = item.Phase
		throttle.pending = nil
	}
	for _, item := range incoming {
		if item.Sequence <= execution.FeedbackCursor {
			continue
		}
		execution.FeedbackCursor = item.Sequence
		changed := throttle.lastEmittedAt.IsZero() || item.Status != throttle.lastStatus || item.Phase != throttle.lastPhase
		important := item.Severity == "error" || item.Severity == "critical"
		if throttle.interval <= 0 || changed || important {
			emit(item)
			continue
		}
		copy := item
		throttle.pending = &copy
	}
	terminal := actionTerminal(execution.Status)
	if throttle.pending != nil && (terminal || now.Sub(throttle.lastEmittedAt) >= throttle.interval) {
		emit(*throttle.pending)
	}
	if terminal {
		delete(r.feedback, execution.ID)
	}
	return execution
}

func (r *Runner) markInterrupted(execution ActionExecution, code, message string) ActionExecution {
	execution.Status = ActionInterrupted
	execution.Error = map[string]any{"code": code, "message": message}
	execution.UpdatedAt = r.now()
	_ = r.journal.SaveAction(execution)
	return cloneActionExecution(execution)
}

func (r *Runner) releasePhysical(robotID, executionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activePhysical[robotID] == executionID {
		delete(r.activePhysical, robotID)
	}
}

// normalizeActionRequest 生成可直接审查的规范化请求；encoding/json 会稳定排序
// map key，相同 Action key 因而可以比较真实内容，无需再保存不可读摘要。
func normalizeActionRequest(request ActionRequest) (string, error) {
	encoded, err := json.Marshal(struct {
		RobotID string         `json:"robot_id"`
		Action  ActionRef      `json:"action"`
		Input   map[string]any `json:"input"`
	}{request.RobotID, request.Action, request.Input})
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// confirmedAbilityTerminal 只接受 Ability 明确返回的可释放终态。Start Task 的
// HTTP结果丢失后最多按同一 invocation 查询一次：若 Ability 已持久化 failed /
// succeeded / stopped，说明其状态可对账；查询失败、running、unknown 或
// interrupted 仍然保留 Robot 锁，绝不重新发送物理请求。
func confirmedAbilityTerminal(status string) bool {
	normalized := normalizeActionStatus(status)
	return actionTerminal(normalized) && normalized != ActionInterrupted
}

func normalizeActionStatus(status string) ActionStatus {
	switch status {
	case "accepted":
		return ActionAccepted
	case "running":
		return ActionRunning
	case "stopping":
		return ActionStopping
	case "succeeded", "completed":
		return ActionSucceeded
	case "failed":
		return ActionFailed
	case "stopped", "cancelled":
		return ActionStopped
	case "interrupted", "unknown":
		return ActionInterrupted
	default:
		return ActionInterrupted
	}
}

func actionTerminal(status ActionStatus) bool {
	return status == ActionSucceeded || status == ActionFailed || status == ActionStopped || status == ActionInterrupted
}

func cloneActionExecution(source ActionExecution) ActionExecution {
	result := source
	result.Feedback = append([]AbilityFeedback(nil), source.Feedback...)
	result.Observations = cloneMaps(source.Observations)
	result.Result = cloneMap(source.Result)
	result.Error = cloneMap(source.Error)
	return result
}

func cloneMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneMaps(source []map[string]any) []map[string]any {
	result := make([]map[string]any, len(source))
	for index, item := range source {
		result[index] = cloneMap(item)
	}
	return result
}
