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

package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

// OnRobotExecutionChanged 把 Pilot 已持久化的物理执行事实收敛到唯一 SubTask。
// Robot Execution 与 Workflow 状态分开保存，是为了允许 Agent Run 在
// robot.run accepted 后结束；真正的完成只能由 Skill 终态和结果证据驱动。
func (s *Service) OnRobotExecutionChanged(ctx context.Context, execution store.RobotExecution,
	eventType string, payload map[string]any) error {
	if execution.WorkflowID == "" || execution.TaskID == "" || execution.SubtaskID == "" {
		return nil
	}
	// Action、Stage 与 Artifact 事件仅提供时间线证据，即使它们附带终态快照，
	// 也不能触发 SubTask 终结或恢复。完成判据必须来自正式 Execution 生命周期。
	if !strings.HasPrefix(eventType, "execution.") &&
		!strings.HasPrefix(eventType, "robot.execution.") &&
		eventType != "skill.started" && eventType != "skill.stop.finalized" &&
		eventType != "agent.requested" && eventType != "agent.resolved" {
		return nil
	}
	task, err := s.st.GetTask(execution.TaskID)
	if err != nil {
		return err
	}
	subTask, err := s.st.GetSubTask(execution.SubtaskID)
	if err != nil {
		return err
	}
	if task.WorkflowID != execution.WorkflowID || subTask.TaskID != task.ID {
		return fmt.Errorf("Robot Execution 与 Workflow Task 归属不一致: %w", store.ErrInvalidState)
	}
	if subTask.ExecutionRef == "" && subTask.Status == store.TaskStatusRunning {
		attached, attachErr := s.st.AttachSubTaskExecution(subTask.ID, subTask.Revision,
			execution.ID, time.Now().UTC())
		if attachErr != nil && !errors.Is(attachErr, store.ErrRevisionConflict) {
			return attachErr
		}
		if attachErr == nil {
			subTask = attached
		} else {
			subTask, err = s.st.GetSubTask(subTask.ID)
			if err != nil {
				return err
			}
		}
	}
	if subTask.ExecutionRef != "" && subTask.ExecutionRef != execution.ID {
		return fmt.Errorf("SubTask 已关联另一个 Robot Execution: %w", store.ErrInvalidState)
	}

	switch execution.Status {
	case "completed":
		if subTask.Status == store.TaskStatusStopping {
			return s.finishStoppingRobotSubTask(ctx, task, subTask, execution,
				"robot_execution_completed_during_stop")
		}
		if err := s.completeRobotSubTask(subTask, execution); err != nil {
			return err
		}
		s.publishCurrent(execution.WorkflowID, "subtask.completed")
		return s.continueOrFinishTask(context.WithoutCancel(ctx), task.ID)
	case "waiting_agent":
		if eventType == "agent.requested" {
			s.launchRobotAgentDecision(context.WithoutCancel(ctx), task, subTask, execution, payload)
		}
		return nil
	case "failed":
		if subTask.Status == store.TaskStatusStopping {
			started, actionErr := s.robotExecutionMayHavePhysicalAction(execution.ID)
			if actionErr != nil {
				return actionErr
			}
			held, heldErr := s.robotExecutionSafelyHeld(execution.ID)
			if heldErr != nil {
				return heldErr
			}
			if safelyStoppedRobotExecution(execution) || held {
				return s.finishStoppingRobotSubTask(ctx, task, subTask, execution,
					"robot_execution_failed_but_held")
			}
			if !started {
				return s.finishStoppingRobotSubTask(ctx, task, subTask, execution,
					"robot_execution_failed_before_action")
			}
			return s.pauseRobotSubTask(task, subTask, "execution_state_unknown",
				robotExecutionResult(execution, eventType, payload))
		}
		physicalStarted, actionErr := s.robotExecutionStartedPhysicalAction(execution.ID)
		if actionErr != nil {
			return actionErr
		}
		if physicalStarted {
			held, heldErr := s.robotExecutionSafelyHeld(execution.ID)
			if heldErr != nil {
				return heldErr
			}
			if safelyStoppedRobotExecution(execution) || held {
				// 物理动作已发生，但 Ability 已留下安全保持证据（策略超时、
				// 异常自终止同样由它撤销执行并核对 hold）。任务未达成而物理
				// 状态可判定，直接收敛为 stopped，而不是永久 execution_state_unknown。
				return s.finishConfirmedRobotStop(ctx, task, subTask, execution)
			}
			// 物理Action已经改变Robot或场景，但失败结果没有明确hold证据。
			// 此时不能启动Recovery重放原Skill，必须保留Robot锁等待对账。
			return s.pauseRobotSubTask(task, subTask, "execution_state_unknown",
				robotExecutionResult(execution, eventType, payload))
		}
		if err := s.pauseRobotSubTask(task, subTask, "robot_execution_failed",
			robotExecutionResult(execution, eventType, payload)); err != nil {
			return err
		}
		s.launchTaskRecovery(context.WithoutCancel(ctx), task.ID, subTask.ID, execution.ID)
		return nil
	case "interrupted":
		// interrupted 期间 Robot 的物理状态未知。Task 可以等待对账或人工处理，
		// 但不得释放 Robot 锁，也不得把原 Action 自动重放到另一台 Robot。
		return s.pauseRobotSubTask(task, subTask, "execution_state_unknown",
			robotExecutionResult(execution, eventType, payload))
	case "stopped", "cancelled":
		if subTask.Status == store.TaskStatusStopping {
			if safelyStoppedRobotExecution(execution) {
				return s.finishStoppingRobotSubTask(ctx, task, subTask, execution,
					"robot_execution_stopped")
			}
			return s.pauseRobotSubTask(task, subTask, "execution_state_unknown",
				robotExecutionResult(execution, eventType, payload))
		}
		if safelyStoppedRobotExecution(execution) {
			return s.finishConfirmedRobotStop(ctx, task, subTask, execution)
		}
		return s.pauseRobotSubTask(task, subTask, "robot_execution_stopped",
			robotExecutionResult(execution, eventType, payload))
	default:
		return nil
	}
}

// finishStoppingRobotSubTask 只在 Pilot 已回传物理执行终态后收敛停止。
// HTTP stop 的 accepted、Worker 退出或上层 Context 取消都不能进入这里。
func (s *Service) finishStoppingRobotSubTask(ctx context.Context, task store.Task,
	subTask store.SubTask, execution store.RobotExecution, reason string) error {
	result := robotExecutionResult(execution, "execution.stop_confirmed", execution.Result)
	if _, err := s.st.TransitionSubTask(subTask.ID, subTask.Revision,
		store.TaskStatusStopped, reason, result, time.Now().UTC()); err != nil {
		return err
	}
	latest, err := s.st.GetTask(task.ID)
	if err != nil {
		return err
	}
	if latest.Status == store.TaskStatusStopping &&
		taskSubTasksSafelyTerminal(mustSubTasks(s.st, latest.ID)) {
		if _, err = s.st.TransitionTask(latest.ID, latest.Revision,
			store.TaskStatusStopped, reason, "", result, time.Now().UTC()); err != nil {
			return err
		}
	}
	s.publishCurrent(task.WorkflowID, "subtask.stopped")
	_ = s.convergeStoppedWorkflow(task.WorkflowID)
	return nil
}

// finishConfirmedRobotStop 处理通过 robot.stop 直接停止当前 Execution 的情况。
// Pilot 只有取得物理安全证据才会把执行标记为 stopped；此时 Task DAG 已无法
// 按原目标继续，必须把所属 Workflow 一并收敛，而不是留下永久 paused/running。
// 其他活动 Task 仍复用统一停止传播，确保多 Agent Workflow 不遗留后台动作。
func (s *Service) finishConfirmedRobotStop(ctx context.Context, task store.Task,
	subTask store.SubTask, execution store.RobotExecution) error {
	stopReason := "robot_execution_stopped"
	if execution.Result["confirmation_source"] == "operator" {
		// 人工确认可能逐台收敛并行 Robot。前一台的停止传播会让剩余未知执行
		// 暂时回到 paused；后一台确认时必须继续保留人工确认原因，不能被通用
		// robot_execution_stopped 覆盖，否则成功请求将失去幂等识别依据。
		stopReason = "operator_confirmed_stop"
	} else if execution.Status == "failed" {
		// 任务失败但已被证明安全保持：收敛为 stopped 的同时保留真实原因，
		// 不冒充成功停止，也不冒充任务失败后物理状态未知。
		stopReason = "robot_execution_failed_but_held"
	}
	workflowValue, err := s.st.GetWorkflow(task.WorkflowID)
	if err != nil {
		return err
	}
	if workflowValue.Status == store.WorkflowStatusRunning ||
		workflowValue.Status == store.WorkflowStatusPaused {
		// Server 重启或 Pilot 短暂离线时，活动执行会先进入 paused 等待对账。
		// 后续一旦 Pilot 给出真实 stop/hold 证据，就应继续走统一停止收敛；
		// 否则会留下“Task 已停止但 Workflow 永久 paused”的壳，阻塞下一份计划。
		workflowValue, err = s.st.TransitionWorkflowWithReason(workflowValue.ID,
			workflowValue.Revision, store.WorkflowStatusStopping,
			stopReason, time.Now().UTC())
		if err != nil {
			return err
		}
	}
	latestTask, err := s.st.GetTask(task.ID)
	if err != nil {
		return err
	}
	if latestTask.Status == store.TaskStatusRunning || latestTask.Status == store.TaskStatusPaused {
		latestTask, err = s.st.TransitionTask(latestTask.ID, latestTask.Revision,
			store.TaskStatusStopping, stopReason, "", nil, time.Now().UTC())
		if err != nil {
			return err
		}
	}
	latestSubTask, err := s.st.GetSubTask(subTask.ID)
	if err != nil {
		return err
	}
	if latestSubTask.Status == store.TaskStatusRunning || latestSubTask.Status == store.TaskStatusPaused {
		latestSubTask, err = s.st.TransitionSubTask(latestSubTask.ID, latestSubTask.Revision,
			store.TaskStatusStopping, stopReason, nil, time.Now().UTC())
		if err != nil {
			return err
		}
	}
	if err := s.finishStoppingRobotSubTask(ctx, latestTask, latestSubTask, execution,
		stopReason); err != nil {
		return err
	}
	if err := s.stopWorkflowTasks(ctx, workflowValue.ProjectID, workflowValue.ID); err != nil {
		return err
	}
	return s.convergeStoppedWorkflow(workflowValue.ID)
}

func safelyStoppedRobotExecution(execution store.RobotExecution) bool {
	safe, _ := execution.Result["safe"].(bool)
	return safe
}

// robotExecutionSafelyHeld 从 Action 事实流读取物理安全保持证据。Ability 是
// 物理状态的唯一权威：撤销执行后它显式 hold 并核对 in_hold，把 safe 证据写进
// 该次 Action 的 result，随 action.terminal 事件持久化。这里按发生顺序重建
// "最后一个物理事实"：每次 physical_started 的 action.started 都推翻此前的保持
// 证明，只有其后的安全保持证明才重新成立。Skill 层的失败文案不参与判定。
func (s *Service) robotExecutionSafelyHeld(executionID string) (bool, error) {
	held := false
	var after int64
	for {
		events, err := s.st.ListRobotExecutionEvents(executionID, after, 1000)
		if err != nil {
			return false, err
		}
		for _, item := range events {
			after = item.Sequence
			switch item.Type {
			case "action.started":
				if started, _ := item.Payload["physical_started"].(bool); started {
					held = false
				}
			case "action.terminal":
				result, ok := item.Payload["result"].(map[string]any)
				if !ok {
					continue
				}
				if safe, exists := result["safe"].(bool); exists {
					held = safe
				}
			}
		}
		if len(events) < 1000 {
			break
		}
	}
	return held, nil
}

func (s *Service) launchRobotAgentDecision(parent context.Context, task store.Task,
	subTask store.SubTask, execution store.RobotExecution, event map[string]any) {
	decider, ok := s.executor.(RobotAgentDecisionExecutor)
	if !ok || s.robotReply == nil {
		_ = s.pauseRobotSubTask(task, subTask, "robot_agent_runtime_unavailable",
			robotExecutionResult(execution, "agent.requested", event))
		return
	}
	// 同一 Task 只有一个可写 Agent Run。重复的 Pilot 事件或浏览器重连可能
	// 再次送达同一请求，Task 预留会让后来的副本直接返回；它不会产生第二个
	// Robot Agent 决策，更不会重新下发已经开始的物理 Action。
	if !s.reserveTask(task) {
		return
	}
	s.launchReservedRobotAgentDecision(parent, task, subTask, execution, event, decider)
}

func (s *Service) launchReservedRobotAgentDecision(parent context.Context, task store.Task,
	subTask store.SubTask, execution store.RobotExecution, event map[string]any,
	decider RobotAgentDecisionExecutor) {
	s.launchReservedRobotAgentDecisionWithAnswer(parent, task, subTask, execution,
		event, nil, decider)
}

func (s *Service) launchReservedRobotAgentDecisionWithAnswer(parent context.Context,
	task store.Task, subTask store.SubTask, execution store.RobotExecution,
	event map[string]any, answer *store.Interaction, decider RobotAgentDecisionExecutor) {
	decisionCtx, cancelDecision := context.WithCancel(parent)
	if !s.bindReservedTaskCancel(task.ID, cancelDecision) {
		return
	}
	go func() {
		defer s.releaseTask(task)
		workflowValue, err := s.st.GetWorkflow(task.WorkflowID)
		if err != nil {
			s.pauseRobotDecisionFailure(task.ID, subTask.ID, execution, event, err, 0)
			return
		}
		project, err := s.st.GetProject(workflowValue.ProjectID)
		if err != nil {
			s.pauseRobotDecisionFailure(task.ID, subTask.ID, execution, event, err, 0)
			return
		}
		conversation, err := s.st.GetChatSession(workflowValue.ConversationID)
		if err != nil {
			s.pauseRobotDecisionFailure(task.ID, subTask.ID, execution, event, err, 0)
			return
		}
		var decision map[string]any
		attempts := 0
		for {
			attempts++
			decision, err = decider.ResolveRobotAgentRequest(decisionCtx, RobotAgentDecisionRequest{
				UserID: project.OwnerID, Project: project, Conversation: conversation,
				Workflow: workflowValue, Task: task, SubTask: subTask,
				Execution: execution, Event: event, Answer: answer,
			})
			if err == nil {
				s.recordRobotDecisionAttempt(execution.ID, attempts, nil)
				break
			}
			s.recordRobotDecisionAttempt(execution.ID, attempts, err)
			if errors.Is(err, context.Canceled) || errors.Is(err, ErrTaskWaitingInput) {
				return
			}
			if errors.Is(err, ErrRobotAgentDecisionInvalid) &&
				attempts < MaxRobotAgentDecisionAttempts && decisionCtx.Err() == nil {
				continue
			}
			s.pauseRobotDecisionFailure(task.ID, subTask.ID, execution, event, err, attempts)
			return
		}
		if decisionCtx.Err() != nil || !s.robotDecisionStillCurrent(task.ID,
			subTask.ID, execution.ID) {
			return
		}
		reply := map[string]any{
			"execution_id":      execution.ID,
			"skill_name":        execution.SkillName,
			"stage":             stringValue(event["stage"]),
			"decision_key":      stringValue(event["decision_key"]),
			"decision_revision": decisionRevision(event["decision_revision"]),
			"payload":           decision,
			"decision_attempts": attempts,
		}
		if err := s.robotReply.ReplyAgentRequest(execution.ID, reply); err != nil {
			s.pauseRobotDecisionFailure(task.ID, subTask.ID, execution, event, err, attempts)
		}
	}()
}

func (s *Service) resumeRobotDecisionFromInteraction(parent context.Context,
	answer store.Interaction, task store.Task) error {
	decider, ok := s.executor.(RobotAgentDecisionExecutor)
	if !ok || s.robotReply == nil {
		return ErrExecutorUnavailable
	}
	var candidate *store.SubTask
	for _, item := range mustSubTasks(s.st, task.ID) {
		if item.Status == store.TaskStatusPaused && item.WaitingReason == "waiting_input" &&
			item.Kind == "robot_skill" && item.ExecutionRef != "" {
			if candidate != nil {
				return store.ErrInvalidState
			}
			copy := item
			candidate = &copy
		}
	}
	if candidate == nil {
		return store.ErrInvalidState
	}
	execution, err := s.st.GetRobotExecution(candidate.ExecutionRef)
	if err != nil || execution.Status != "waiting_agent" {
		return store.ErrInvalidState
	}
	requested, found, err := s.latestRobotAgentRequest(execution.ID)
	if err != nil {
		return err
	}
	if !found {
		return store.ErrInvalidState
	}
	var reply any
	if answer.Status == store.InteractionStatusCancelled {
		// 取消问题不是替用户选择一个恢复策略。原Robot Agent仍需收到明确的
		// cancelled结果，才能结束等待、改写问题或在批准范围内安全退出。
		reply = map[string]any{"interaction_outcome": "cancelled"}
	} else if err := json.Unmarshal([]byte(answer.Reply), &reply); err != nil {
		return err
	}
	contextValue, _ := requested["context"].(map[string]any)
	contextCopy := make(map[string]any, len(contextValue)+1)
	for key, value := range contextValue {
		contextCopy[key] = value
	}
	contextCopy["interaction_answer"] = reply
	requested = cloneAnyMap(requested)
	requested["context"] = contextCopy

	runningSubTask, err := s.st.TransitionSubTask(candidate.ID, candidate.Revision,
		store.TaskStatusRunning, "", candidate.Result, time.Now().UTC())
	if err != nil {
		return err
	}
	runningTask, err := s.st.TransitionTask(task.ID, task.Revision,
		store.TaskStatusRunning, "", task.ResultSummary, task.Evidence, time.Now().UTC())
	if err != nil {
		_, _ = s.st.TransitionSubTask(runningSubTask.ID, runningSubTask.Revision,
			store.TaskStatusPaused, "waiting_input", runningSubTask.Result, time.Now().UTC())
		return err
	}
	if !s.reserveTask(runningTask) {
		_, _ = s.st.TransitionSubTask(runningSubTask.ID, runningSubTask.Revision,
			store.TaskStatusPaused, "waiting_input", runningSubTask.Result, time.Now().UTC())
		_, _ = s.st.TransitionTask(runningTask.ID, runningTask.Revision,
			store.TaskStatusPaused, "waiting_input", runningTask.ResultSummary,
			runningTask.Evidence, time.Now().UTC())
		return ErrTaskBusy
	}
	s.launchReservedRobotAgentDecisionWithAnswer(parent, runningTask, runningSubTask,
		execution, requested, &answer, decider)
	return nil
}

// latestRobotAgentRequest 只读取Pilot已经持久化的请求事件。事件处理与当前
// Task Agent Run释放写门禁存在先后竞态，因此即时回调和Run结束后的接续都
// 必须复用同一份持久事实，不能依赖某个进程内待处理标记。
func (s *Service) latestRobotAgentRequest(executionID string) (map[string]any, bool, error) {
	events, err := s.st.ListRobotExecutionEvents(executionID, 0, 1000)
	if err != nil {
		return nil, false, err
	}
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type == "agent.requested" {
			return events[index].Payload, true, nil
		}
	}
	return nil, false, nil
}

func cloneAnyMap(input map[string]any) map[string]any {
	result := make(map[string]any, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func (s *Service) robotDecisionStillCurrent(taskID, subTaskID, executionID string) bool {
	task, taskErr := s.st.GetTask(taskID)
	subTask, subTaskErr := s.st.GetSubTask(subTaskID)
	execution, executionErr := s.st.GetRobotExecution(executionID)
	workflowValue, workflowErr := s.st.GetWorkflow(task.WorkflowID)
	return taskErr == nil && subTaskErr == nil && executionErr == nil && workflowErr == nil &&
		workflowValue.Status == store.WorkflowStatusRunning &&
		task.Status == store.TaskStatusRunning &&
		subTask.Status == store.TaskStatusRunning &&
		subTask.ExecutionRef == executionID && execution.Status == "waiting_agent"
}

func (s *Service) recordRobotDecisionAttempt(executionID string, attempt int, cause error) {
	sequence, err := s.st.NextRobotExecutionEventSequence(executionID)
	if err != nil {
		return
	}
	payload := map[string]any{"decision_attempts": attempt}
	if cause != nil {
		payload["decision_error"] = cause.Error()
	}
	_ = s.st.AppendRobotExecutionEvent(store.RobotExecutionEvent{
		ExecutionID: executionID, Sequence: sequence, Type: "agent.decision_attempted",
		Payload: payload, CreatedAt: time.Now().UTC(),
	})
}

func (s *Service) pauseRobotDecisionFailure(taskID, subTaskID string,
	execution store.RobotExecution, event map[string]any, cause error, attempts int) {
	task, taskErr := s.st.GetTask(taskID)
	subTask, subTaskErr := s.st.GetSubTask(subTaskID)
	if taskErr != nil || subTaskErr != nil {
		return
	}
	payload := make(map[string]any, len(event)+2)
	for key, value := range event {
		payload[key] = value
	}
	payload["decision_error"] = cause.Error()
	payload["decision_attempts"] = attempts
	_ = s.pauseRobotSubTask(task, subTask, "robot_agent_decision_failed",
		robotExecutionResult(execution, "agent.decision_failed", payload))
	if workflowValue, err := s.st.GetWorkflow(task.WorkflowID); err == nil &&
		workflowValue.Status == store.WorkflowStatusRunning {
		// 只把决策失败升到 Workflow。waiting_input 等其它暂停不得走这条路径，
		// 否则通用 Resume 会把现场误判成可重放的 Task。
		_, _ = s.st.TransitionWorkflowWithReason(workflowValue.ID, workflowValue.Revision,
			store.WorkflowStatusPaused, "robot_agent_decision_failed", time.Now().UTC())
		s.publishCurrent(task.WorkflowID, "workflow.paused")
	}
}

func decisionRevision(value any) int64 {
	switch number := value.(type) {
	case int:
		return int64(number)
	case int64:
		return number
	case float64:
		return int64(number)
	default:
		return 0
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func (s *Service) completeRobotSubTask(subTask store.SubTask,
	execution store.RobotExecution) error {
	if subTask.Status == store.TaskStatusCompleted {
		return nil
	}
	if subTask.Status == store.TaskStatusPaused {
		resumed, err := s.st.TransitionSubTask(subTask.ID, subTask.Revision,
			store.TaskStatusRunning, "", nil, time.Now().UTC())
		if err != nil {
			return err
		}
		subTask = resumed
	}
	if subTask.Status != store.TaskStatusRunning {
		return nil
	}
	result := robotExecutionResult(execution, "execution.terminal", execution.Result)
	_, err := s.st.TransitionSubTask(subTask.ID, subTask.Revision,
		store.TaskStatusCompleted, "", result, time.Now().UTC())
	return err
}

func (s *Service) pauseRobotSubTask(task store.Task, subTask store.SubTask,
	reason string, result json.RawMessage) error {
	if subTask.Status == store.TaskStatusRunning || subTask.Status == store.TaskStatusStopping {
		paused, err := s.st.TransitionSubTask(subTask.ID, subTask.Revision,
			store.TaskStatusPaused, reason, result, time.Now().UTC())
		if err != nil {
			return err
		}
		subTask = paused
	}
	if task.Status == store.TaskStatusRunning || task.Status == store.TaskStatusStopping {
		if _, err := s.st.TransitionTask(task.ID, task.Revision, store.TaskStatusPaused,
			reason, "", result, time.Now().UTC()); err != nil {
			return err
		}
	}
	if workflowValue, err := s.st.GetWorkflow(task.WorkflowID); err == nil &&
		workflowValue.Status == store.WorkflowStatusStopping {
		// 停止过程中状态未知时不能继续显示“停止中”或强行 stopped。Workflow
		// 回到 paused，保留 execution_ref 与 Robot 锁，等待对账/人工处理。
		_, _ = s.st.TransitionWorkflowWithReason(workflowValue.ID, workflowValue.Revision,
			store.WorkflowStatusPaused, reason, time.Now().UTC())
	}
	s.publishCurrent(task.WorkflowID, "task.paused")
	s.appendMilestone(task.WorkflowID, task.ID, subTask.ID, "task_paused", store.TaskStatusPaused)
	return nil
}

// launchTaskRecovery 在明确 failed 后只启动一次短决策 Run。恢复 Run 不会
// 直接修改 Task/SubTask；Workflow Service 只接受类型化决策，并在同一事务中
// 替换未开始步骤。interrupted/unknown 根本不会调用这里。
func (s *Service) launchTaskRecovery(parent context.Context, taskID, subTaskID, executionID string) {
	recovery, ok := s.executor.(TaskRecoveryExecutor)
	if !ok {
		return
	}
	task, err := s.st.GetTask(taskID)
	subTask, subTaskErr := s.st.GetSubTask(subTaskID)
	execution, executionErr := s.st.GetRobotExecution(executionID)
	if err != nil || subTaskErr != nil || executionErr != nil ||
		task.Status != store.TaskStatusPaused || task.WaitingReason != "robot_execution_failed" ||
		subTask.Status != store.TaskStatusPaused || subTask.WaitingReason != "robot_execution_failed" ||
		subTask.ExecutionRef != execution.ID || execution.Status != "failed" {
		return
	}
	if !s.reserveTask(task) {
		return
	}
	task, subTask, err = s.st.UpdatePausedRobotWaitReason(task.ID, task.Revision,
		subTask.ID, subTask.Revision, "robot_execution_failed", "recovery_running",
		time.Now().UTC())
	if err != nil {
		s.releaseTask(task)
		return
	}
	s.launchReservedTaskRecovery(parent, task, subTask, execution, nil, recovery)
}

func (s *Service) launchReservedTaskRecovery(parent context.Context, task store.Task,
	subTask store.SubTask, execution store.RobotExecution, answer *store.Interaction,
	recovery TaskRecoveryExecutor) {
	recoveryCtx, cancelRecovery := context.WithCancel(parent)
	if !s.bindReservedTaskCancel(task.ID, cancelRecovery) {
		s.releaseTask(task)
		return
	}
	go func() {
		continueAfterRecovery := false
		defer func() {
			s.releaseTask(task)
			if continueAfterRecovery {
				_ = s.continueOrFinishTask(context.Background(), task.ID)
			}
		}()
		workflowValue, workflowErr := s.st.GetWorkflow(task.WorkflowID)
		if workflowErr != nil || execution.Status != "failed" {
			return
		}
		project, projectErr := s.st.GetProject(workflowValue.ProjectID)
		conversation, conversationErr := s.st.GetChatSession(workflowValue.ConversationID)
		if projectErr != nil || conversationErr != nil {
			return
		}
		decision, decideErr := recovery.RecoverTask(recoveryCtx, TaskRecoveryRequest{
			UserID: project.OwnerID, Project: project, Conversation: conversation,
			Workflow: workflowValue, Task: task, SubTask: subTask, Execution: execution,
			Answer: answer,
		})
		if decideErr != nil {
			if errors.Is(decideErr, context.Canceled) || errors.Is(decideErr, ErrTaskWaitingInput) {
				return
			}
			latestTask, taskErr := s.st.GetTask(task.ID)
			latestSubTask, subTaskErr := s.st.GetSubTask(subTask.ID)
			if taskErr == nil && subTaskErr == nil &&
				latestTask.Status == store.TaskStatusPaused && latestTask.WaitingReason == "recovery_running" &&
				latestSubTask.Status == store.TaskStatusPaused && latestSubTask.WaitingReason == "recovery_running" {
				_, _, _ = s.st.UpdatePausedRobotWaitReason(latestTask.ID, latestTask.Revision,
					latestSubTask.ID, latestSubTask.Revision, "recovery_running",
					"recovery_exhausted", time.Now().UTC())
			}
			s.appendMilestone(task.WorkflowID, task.ID, subTask.ID,
				"task_recovery_failed", store.TaskStatusPaused)
			return
		}
		if recoveryCtx.Err() != nil || !s.failedRecoveryStillCurrent(task,
			subTask, execution) {
			return
		}
		switch decision.Decision {
		case "revise_pending":
			updated, _, applyErr := s.st.ApplyFailedRobotRecovery(task.ID, task.Revision,
				subTask.ID, decision.Replacements, time.Now().UTC())
			if applyErr != nil {
				return
			}
			s.publishCurrent(task.WorkflowID, "task.recovery_revised")
			s.appendMilestone(task.WorkflowID, task.ID, subTask.ID,
				"task_recovery_revised", updated.Status)
			continueAfterRecovery = true
		case "fail_task":
			s.failPausedTask(task.ID, subTask.ID, decision.Summary)
		default:
			// 没有形成明确动作时保持 paused，比擅自重试或伪造终态更安全。
			s.appendMilestone(task.WorkflowID, task.ID, subTask.ID,
				"task_recovery_waiting", store.TaskStatusPaused)
		}
	}()
}

func (s *Service) resumeTaskRecoveryFromInteraction(parent context.Context,
	answer store.Interaction, task store.Task) error {
	recovery, ok := s.executor.(TaskRecoveryExecutor)
	if !ok {
		return ErrExecutorUnavailable
	}
	var candidate *store.SubTask
	var execution store.RobotExecution
	for _, item := range mustSubTasks(s.st, task.ID) {
		if item.Status != store.TaskStatusPaused || item.WaitingReason != "waiting_input" ||
			item.Kind != "robot_skill" || item.ExecutionRef == "" {
			continue
		}
		value, err := s.st.GetRobotExecution(item.ExecutionRef)
		if err != nil || value.Status != "failed" {
			continue
		}
		if candidate != nil {
			return store.ErrInvalidState
		}
		copy := item
		candidate, execution = &copy, value
	}
	if candidate == nil {
		return store.ErrInvalidState
	}
	if !s.reserveTask(task) {
		return ErrTaskBusy
	}
	resumedTask, resumedSubTask, err := s.st.UpdatePausedRobotWaitReason(task.ID,
		task.Revision, candidate.ID, candidate.Revision, "waiting_input",
		"recovery_running", time.Now().UTC())
	if err != nil {
		s.releaseTask(task)
		return err
	}
	s.launchReservedTaskRecovery(parent, resumedTask, resumedSubTask, execution, &answer, recovery)
	return nil
}

func (s *Service) failedRecoveryStillCurrent(task store.Task, subTask store.SubTask,
	execution store.RobotExecution) bool {
	currentTask, taskErr := s.st.GetTask(task.ID)
	currentSubTask, subTaskErr := s.st.GetSubTask(subTask.ID)
	currentExecution, executionErr := s.st.GetRobotExecution(execution.ID)
	workflowValue, workflowErr := s.st.GetWorkflow(task.WorkflowID)
	return taskErr == nil && subTaskErr == nil && executionErr == nil && workflowErr == nil &&
		workflowValue.Status == store.WorkflowStatusRunning &&
		currentTask.Status == store.TaskStatusPaused &&
		currentTask.WaitingReason == "recovery_running" &&
		currentTask.Revision == task.Revision &&
		currentSubTask.Status == store.TaskStatusPaused &&
		currentSubTask.WaitingReason == "recovery_running" &&
		currentSubTask.ExecutionRef == execution.ID &&
		currentExecution.Status == "failed"
}

func (s *Service) failPausedTask(taskID, failedSubTaskID, summary string) {
	task, err := s.st.GetTask(taskID)
	if err != nil || task.Status != store.TaskStatusPaused {
		return
	}
	if subTask, subErr := s.st.GetSubTask(failedSubTaskID); subErr == nil &&
		subTask.Status == store.TaskStatusPaused {
		_, _ = s.st.TransitionSubTask(subTask.ID, subTask.Revision,
			store.TaskStatusFailed, "recovery_exhausted", subTask.Result, time.Now().UTC())
	}
	for _, pending := range mustSubTasks(s.st, task.ID) {
		if pending.Status == store.TaskStatusPending {
			_, _ = s.st.TransitionSubTask(pending.ID, pending.Revision,
				store.TaskStatusStopped, "task_failed", nil, time.Now().UTC())
		}
	}
	latest, err := s.st.GetTask(task.ID)
	if err != nil || latest.Status != store.TaskStatusPaused {
		return
	}
	if strings.TrimSpace(summary) == "" {
		summary = "Robot Skill 恢复预算已耗尽"
	}
	failed, err := s.st.TransitionTask(latest.ID, latest.Revision, store.TaskStatusFailed,
		"robot_recovery_failed", summary, nil, time.Now().UTC())
	if err != nil {
		return
	}
	s.publishCurrent(failed.WorkflowID, "task.failed")
	s.appendMilestone(failed.WorkflowID, failed.ID, failedSubTaskID,
		"task_failed", store.TaskStatusFailed)
	s.releaseTaskResources(failed.ID)
	if workflowValue, workflowErr := s.st.GetWorkflow(failed.WorkflowID); workflowErr == nil {
		if project, projectErr := s.st.GetProject(workflowValue.ProjectID); projectErr == nil {
			s.finishOrSchedule(context.Background(), project.OwnerID, failed.WorkflowID)
		}
	}
}

func robotExecutionResult(execution store.RobotExecution, eventType string,
	payload any) json.RawMessage {
	value, _ := json.Marshal(map[string]any{
		"robot_execution_id": execution.ID,
		"skill_name":         execution.SkillName,
		"skill_version":      execution.SkillVersion,
		"status":             execution.Status,
		"result":             execution.Result,
		"error":              execution.Error,
		"artifact_refs":      execution.ArtifactRefs,
		"terminal_event":     eventType,
		"terminal_payload":   payload,
	})
	return value
}
