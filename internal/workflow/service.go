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
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/store"
)

// Service 是 Plan 与 Task 的应用服务。HTTP、Interaction 与 Server 启动恢复
// 都通过这里推进状态，不能绕过 Planning Run 或 Scheduler 直接改表。
type Service struct {
	st                  *store.Store
	planner             Planner
	executor            TaskExecutor
	robotReply          RobotAgentReplySink
	robotStop           RobotExecutionStopper
	conversationResumer ConversationInteractionResumer
	bus                 *event.Bus

	mu              sync.Mutex
	activeTasks     map[string]context.CancelFunc
	activeResources map[string]string
	activeRuns      map[string]string
	shuttingDown    bool
}

func NewService(deps Deps) (*Service, error) {
	if deps.Store == nil {
		return nil, errors.New("Workflow Service 缺少 Store")
	}
	return &Service{
		st: deps.Store, bus: deps.Bus, planner: deps.Planner, executor: deps.Executor,
		robotReply:          deps.RobotReply,
		robotStop:           deps.RobotStop,
		conversationResumer: deps.ConversationResumer,
		activeTasks:         make(map[string]context.CancelFunc),
		activeResources:     make(map[string]string),
		activeRuns:          make(map[string]string),
	}, nil
}

// Shutdown 使调度器停止接受新的 Task。Agent Runtime 负责取消仍在执行的
// Agent Run；Proposal 提交本身是短事务，不再持有后台 Planner。
func (s *Service) Shutdown() {
	s.mu.Lock()
	s.shuttingDown = true
	s.mu.Unlock()
}

func prepareTaskIDs(draft store.WorkflowDraft) (store.WorkflowDraft, error) {
	if len(draft.Tasks) == 0 {
		return store.WorkflowDraft{}, fmt.Errorf("Leader 未生成 Task: %w", store.ErrInvalidState)
	}
	for index := range draft.Tasks {
		if strings.TrimSpace(draft.Tasks[index].ID) == "" {
			draft.Tasks[index].ID = store.NewTaskID()
		}
		// Task Agent 才负责初始 SubTask，Leader 输出中的同名字段不能绕过
		// 该 Planning Run。
		draft.Tasks[index].SubTasks = nil
	}
	return draft, nil
}

func (s *Service) PauseWorkflow(_ context.Context, userID, projectID, workflowID string,
	expectedRevision int64) (store.WorkflowView, error) {
	if _, _, _, err := s.requireWorkflow(userID, projectID, workflowID); err != nil {
		return store.WorkflowView{}, err
	}
	if _, err := s.st.TransitionWorkflowWithReason(workflowID, expectedRevision,
		store.WorkflowStatusPaused, "user_paused", time.Now().UTC()); err != nil {
		return store.WorkflowView{}, err
	}
	result, err := s.st.GetWorkflowView(workflowID)
	if err == nil {
		s.publishView(result, "workflow.paused")
	}
	return result, err
}

func (s *Service) ResumeWorkflow(ctx context.Context, userID, projectID, workflowID string,
	expectedRevision int64) (store.WorkflowView, error) {
	if s.executor == nil {
		return store.WorkflowView{}, ErrExecutorUnavailable
	}
	current, _, _, err := s.requireWorkflow(userID, projectID, workflowID)
	if err != nil {
		return store.WorkflowView{}, err
	}
	if current.Workflow.Revision != expectedRevision {
		return store.WorkflowView{}, store.ErrRevisionConflict
	}
	if err := validateExplicitWorkflowResume(current); err != nil {
		return store.WorkflowView{}, err
	}
	if err := s.validateWorkflowMapBinding(current.Workflow); err != nil {
		return store.WorkflowView{}, err
	}
	if _, err := s.st.TransitionWorkflow(workflowID, expectedRevision,
		store.WorkflowStatusRunning, time.Now().UTC()); err != nil {
		return store.WorkflowView{}, err
	}
	if err := s.resumePausedTasks(ctx, userID, workflowID); err != nil {
		if current, getErr := s.st.GetWorkflow(workflowID); getErr == nil && current.Status == store.WorkflowStatusRunning {
			_, _ = s.st.TransitionWorkflowWithReason(workflowID, current.Revision,
				store.WorkflowStatusPaused, "resume_failed", time.Now().UTC())
		}
		return store.WorkflowView{}, err
	}
	s.schedule(ctx, userID, workflowID)
	result, err := s.st.GetWorkflowView(workflowID)
	if err == nil {
		s.publishView(result, "workflow.resumed")
	}
	return result, err
}

// RetryRobotAgentDecision 只对 robot_agent_decision_failed 重开 Decision Run。
// 它不得走通用 Resume：resumePausedTasks 会 launchReservedTask 并再次 robot.run。
func (s *Service) RetryRobotAgentDecision(ctx context.Context, userID, projectID, workflowID string,
	expectedRevision int64) (store.WorkflowView, error) {
	if s.executor == nil {
		return store.WorkflowView{}, ErrExecutorUnavailable
	}
	decider, ok := s.executor.(RobotAgentDecisionExecutor)
	if !ok || s.robotReply == nil {
		return store.WorkflowView{}, ErrExecutorUnavailable
	}
	current, _, _, err := s.requireWorkflow(userID, projectID, workflowID)
	if err != nil {
		return store.WorkflowView{}, err
	}
	if current.Workflow.Revision != expectedRevision {
		return store.WorkflowView{}, store.ErrRevisionConflict
	}
	task, subTask, execution, event, err := s.robotDecisionRetryTarget(current)
	if err != nil {
		return store.WorkflowView{}, err
	}
	now := time.Now().UTC()
	runningWorkflow, err := s.st.TransitionWorkflow(workflowID, expectedRevision,
		store.WorkflowStatusRunning, now)
	if err != nil {
		return store.WorkflowView{}, err
	}
	runningTask, err := s.st.TransitionTask(task.ID, task.Revision, store.TaskStatusRunning,
		"", task.ResultSummary, task.Evidence, now)
	if err != nil {
		_, _ = s.st.TransitionWorkflowWithReason(runningWorkflow.ID, runningWorkflow.Revision,
			store.WorkflowStatusPaused, "robot_agent_decision_failed", now)
		return store.WorkflowView{}, err
	}
	runningSubTask, err := s.st.TransitionSubTask(subTask.ID, subTask.Revision,
		store.TaskStatusRunning, "", subTask.Result, now)
	if err != nil {
		_, _ = s.st.TransitionTask(runningTask.ID, runningTask.Revision, store.TaskStatusPaused,
			"robot_agent_decision_failed", runningTask.ResultSummary, runningTask.Evidence, now)
		_, _ = s.st.TransitionWorkflowWithReason(runningWorkflow.ID, runningWorkflow.Revision,
			store.WorkflowStatusPaused, "robot_agent_decision_failed", now)
		return store.WorkflowView{}, err
	}
	if !s.reserveTask(runningTask) {
		_, _ = s.st.TransitionSubTask(runningSubTask.ID, runningSubTask.Revision,
			store.TaskStatusPaused, "robot_agent_decision_failed", runningSubTask.Result, now)
		_, _ = s.st.TransitionTask(runningTask.ID, runningTask.Revision, store.TaskStatusPaused,
			"robot_agent_decision_failed", runningTask.ResultSummary, runningTask.Evidence, now)
		_, _ = s.st.TransitionWorkflowWithReason(runningWorkflow.ID, runningWorkflow.Revision,
			store.WorkflowStatusPaused, "robot_agent_decision_failed", now)
		return store.WorkflowView{}, ErrTaskBusy
	}
	s.launchReservedRobotAgentDecision(ctx, runningTask, runningSubTask, execution, event, decider)
	result, err := s.st.GetWorkflowView(workflowID)
	if err == nil {
		s.publishView(result, "workflow.robot_decision_retried")
	}
	return result, err
}

func (s *Service) robotDecisionRetryTarget(view store.WorkflowView) (store.Task, store.SubTask,
	store.RobotExecution, map[string]any, error) {
	if view.Workflow.Status != store.WorkflowStatusPaused ||
		view.Workflow.Reason != "robot_agent_decision_failed" {
		return store.Task{}, store.SubTask{}, store.RobotExecution{}, nil, store.ErrInvalidState
	}
	var candidateTask *store.Task
	for index := range view.Tasks {
		task := view.Tasks[index]
		if task.Status == store.TaskStatusPaused && task.WaitingReason == "robot_agent_decision_failed" {
			if candidateTask != nil {
				return store.Task{}, store.SubTask{}, store.RobotExecution{}, nil, store.ErrInvalidState
			}
			copy := task
			candidateTask = &copy
		}
	}
	if candidateTask == nil {
		return store.Task{}, store.SubTask{}, store.RobotExecution{}, nil, store.ErrInvalidState
	}
	var candidateSub *store.SubTask
	for index := range view.SubTasks {
		item := view.SubTasks[index]
		if item.TaskID != candidateTask.ID || item.Status != store.TaskStatusPaused ||
			item.WaitingReason != "robot_agent_decision_failed" || item.Kind != "robot_skill" ||
			item.ExecutionRef == "" {
			continue
		}
		if candidateSub != nil {
			return store.Task{}, store.SubTask{}, store.RobotExecution{}, nil, store.ErrInvalidState
		}
		copy := item
		candidateSub = &copy
	}
	if candidateSub == nil {
		return store.Task{}, store.SubTask{}, store.RobotExecution{}, nil, store.ErrInvalidState
	}
	execution, err := s.st.GetRobotExecution(candidateSub.ExecutionRef)
	if err != nil {
		return store.Task{}, store.SubTask{}, store.RobotExecution{}, nil, err
	}
	if execution.Status != "waiting_agent" || execution.ID == "" ||
		execution.TaskID != candidateTask.ID || execution.SubtaskID != candidateSub.ID ||
		candidateSub.ExecutionRef != execution.ID {
		return store.Task{}, store.SubTask{}, store.RobotExecution{}, nil, store.ErrInvalidState
	}
	requested, found, err := s.latestRobotAgentRequest(execution.ID)
	if err != nil {
		return store.Task{}, store.SubTask{}, store.RobotExecution{}, nil, err
	}
	if !found {
		return store.Task{}, store.SubTask{}, store.RobotExecution{}, nil, store.ErrInvalidState
	}
	return *candidateTask, *candidateSub, execution, requested, nil
}

func (s *Service) StopWorkflow(ctx context.Context, userID, projectID, workflowID string,
	expectedRevision int64) (store.WorkflowView, error) {
	view, _, _, err := s.requireWorkflow(userID, projectID, workflowID)
	if err != nil {
		return store.WorkflowView{}, err
	}
	// stop 是面向同一个 Workflow 的幂等命令。浏览器重试或 Server 重连时，
	// 第一次请求可能已经把 revision 推进到 stopping/stopped；此时继续对账
	// 同一个停止目标，不能再用旧 revision 把用户挡在永久 stopping 外面。
	if view.Workflow.Status == store.WorkflowStatusStopped {
		return view, nil
	}
	if view.Workflow.Status != store.WorkflowStatusRunning &&
		view.Workflow.Status != store.WorkflowStatusPaused &&
		view.Workflow.Status != store.WorkflowStatusStopping {
		return store.WorkflowView{}, store.ErrInvalidState
	}
	if view.Workflow.Status != store.WorkflowStatusStopping {
		if view.Workflow.Revision != expectedRevision {
			return store.WorkflowView{}, store.ErrRevisionConflict
		}
		if _, err := s.st.TransitionWorkflowWithReason(workflowID, expectedRevision,
			store.WorkflowStatusStopping, "user_stopped", time.Now().UTC()); err != nil {
			return store.WorkflowView{}, err
		}
	}
	if err := s.stopWorkflowTasks(ctx, projectID, workflowID); err != nil {
		return store.WorkflowView{}, err
	}
	_ = s.convergeStoppedWorkflow(workflowID)
	result, err := s.st.GetWorkflowView(workflowID)
	if err != nil {
		return result, err
	}
	// 物理执行状态未知时安全策略会把 stopping 退回 paused/execution_state_unknown，
	// 普通 stop 无法证明安全停止。此时必须把"需要人工确认"作为明确结果返回，否则
	// 前端拿到 200 会显示停止成功，而 Robot 锁和场景其实都还在。
	if result.Workflow.Status == store.WorkflowStatusPaused &&
		result.Workflow.Reason == "execution_state_unknown" {
		s.publishView(result, "workflow.stop_requires_confirmation")
		return result, store.ErrOperatorConfirmationRequired
	}
	// 已收敛到 stopped 时 convergeStoppedWorkflow 已经发布终态事件；这里只描述
	// 仍在停止过程中的视图，避免活动流把已暂停/已停止的视图显示成"停止中"。
	if result.Workflow.Status != store.WorkflowStatusStopped {
		s.publishView(result, "workflow.stopping")
	}
	return result, nil
}

// ConfirmWorkflowStop 是 execution_state_unknown 的唯一人工终结入口。它先用
// Workflow revision 锁定用户看到的故障现场，再把每个 Robot SubTask 的精确
// execution_ref 交给 Robot Service 记录安全确认；Task/SubTask 不暴露独立停止
// API，避免局部释放 Robot 后 Workflow 仍被误判为可继续运行。
func (s *Service) ConfirmWorkflowStop(
	ctx context.Context, userID, projectID, workflowID string, expectedRevision int64,
	physicalStateConfirmed bool, reason string,
) (store.WorkflowView, error) {
	reason = strings.TrimSpace(reason)
	if !physicalStateConfirmed || reason == "" {
		return store.WorkflowView{}, store.ErrInvalidState
	}
	view, _, _, err := s.requireWorkflow(userID, projectID, workflowID)
	if err != nil {
		return store.WorkflowView{}, err
	}
	// 已成功人工终结后的重复提交不再依赖旧 revision，保证浏览器超时重试
	// 不会把同一个 Workflow 误报为失败。
	if view.Workflow.Status == store.WorkflowStatusStopped &&
		view.Workflow.Reason == "operator_confirmed_stop" {
		return view, nil
	}
	if view.Workflow.Revision != expectedRevision {
		return store.WorkflowView{}, store.ErrRevisionConflict
	}
	if view.Workflow.Status != store.WorkflowStatusStopping &&
		!(view.Workflow.Status == store.WorkflowStatusPaused &&
			view.Workflow.Reason == "execution_state_unknown") {
		return store.WorkflowView{}, store.ErrInvalidState
	}
	if s.robotStop == nil {
		return store.WorkflowView{}, ErrExecutorUnavailable
	}

	// 在修改 Task 前固定需要确认的 execution_ref。Observer 会在每条确认后复用
	// 正常停止收敛并更新 revision，因此这里不能一边遍历实时 View 一边重新选目标。
	type confirmationTarget struct {
		task    store.Task
		subTask store.SubTask
	}
	targets := make([]confirmationTarget, 0)
	for _, subTask := range view.SubTasks {
		if subTask.Kind == "robot_skill" && subTask.ExecutionRef != "" &&
			!terminalTaskStatus(subTask.Status) {
			for _, task := range view.Tasks {
				if task.ID == subTask.TaskID {
					targets = append(targets, confirmationTarget{task: task, subTask: subTask})
					break
				}
			}
		}
	}

	if view.Workflow.Status == store.WorkflowStatusPaused {
		if _, err := s.st.TransitionWorkflowWithReason(
			workflowID, view.Workflow.Revision, store.WorkflowStatusStopping,
			"operator_confirmed_stop", time.Now().UTC(),
		); err != nil {
			return store.WorkflowView{}, err
		}
	}
	for _, target := range targets {
		expected := store.RobotExecution{
			ID: target.subTask.ExecutionRef, ProjectID: projectID,
			WorkflowID: workflowID, TaskID: target.task.ID, SubtaskID: target.subTask.ID,
			RobotID: target.task.AssignedRobotID, SkillName: "unknown", SkillVersion: "unknown",
			Status: "interrupted", Input: map[string]any{},
		}
		if _, err := s.robotStop.ConfirmOperatorStop(ctx, expected, userID, reason); err != nil {
			return store.WorkflowView{}, err
		}
	}

	// 多 Robot Workflow 在逐条确认时可能暂时回到 paused/unknown。全部确认完成后
	// 再进入一次 stopping 并复用统一传播，终结未运行的 Agent Task、释放 Robot
	// 保留和 Project running 模式；这里不直接批量 UPDATE 数据库。
	current, err := s.st.GetWorkflow(workflowID)
	if err != nil {
		return store.WorkflowView{}, err
	}
	if current.Status == store.WorkflowStatusPaused {
		current, err = s.st.TransitionWorkflowWithReason(
			workflowID, current.Revision, store.WorkflowStatusStopping,
			"operator_confirmed_stop", time.Now().UTC(),
		)
		if err != nil {
			return store.WorkflowView{}, err
		}
	}
	if current.Status == store.WorkflowStatusStopping {
		if err := s.stopWorkflowTasks(ctx, projectID, workflowID); err != nil {
			return store.WorkflowView{}, err
		}
		if err := s.convergeStoppedWorkflow(workflowID); err != nil {
			return store.WorkflowView{}, err
		}
	}
	result, err := s.st.GetWorkflowView(workflowID)
	if err == nil {
		s.publishView(result, "workflow.stop_confirmed_by_operator")
	}
	return result, err
}

func terminalTaskStatus(status string) bool {
	return status == store.TaskStatusCompleted || status == store.TaskStatusFailed ||
		status == store.TaskStatusStopped
}

// validateExplicitWorkflowResume 只允许恢复“用户暂停”以及重启后确认没有活动
// 物理执行的工作。Robot Execution failed/interrupted、等待 Agent/用户和停止
// 对账都有各自的恢复入口；通用 Resume 若越过这些原因会重放真实动作。
func validateExplicitWorkflowResume(view store.WorkflowView) error {
	switch view.Workflow.Reason {
	case "user_paused":
		for _, task := range view.Tasks {
			if task.Status == store.TaskStatusPaused {
				return store.ErrInvalidState
			}
		}
		return nil
	case "server_restarted":
		for _, task := range view.Tasks {
			if task.Status == store.TaskStatusPaused && task.WaitingReason != "server_restarted" {
				return store.ErrInvalidState
			}
		}
		for _, subTask := range view.SubTasks {
			if subTask.Status != store.TaskStatusPaused {
				continue
			}
			if subTask.WaitingReason != "server_restarted" || subTask.ExecutionRef != "" {
				return store.ErrInvalidState
			}
		}
		return nil
	default:
		return store.ErrInvalidState
	}
}

func (s *Service) requireWritableConversation(userID, projectID, conversationID string) (store.Project, store.ChatSession, error) {
	if err := s.st.ProjectWritableByUser(userID, projectID); err != nil {
		return store.Project{}, store.ChatSession{}, err
	}
	conversation, err := s.st.GetChatSession(conversationID)
	if err != nil || conversation.ProjectID != projectID || conversation.UserID != userID || conversation.ArchivedAt != nil {
		return store.Project{}, store.ChatSession{}, store.ErrNotFound
	}
	project, err := s.st.GetProject(projectID)
	return project, conversation, err
}

func (s *Service) requireWorkflow(userID, projectID, workflowID string) (store.WorkflowView, store.Project, store.ChatSession, error) {
	if err := s.st.ProjectWritableByUser(userID, projectID); err != nil {
		return store.WorkflowView{}, store.Project{}, store.ChatSession{}, err
	}
	view, err := s.st.GetWorkflowView(workflowID)
	if err != nil || view.Workflow.ProjectID != projectID {
		return store.WorkflowView{}, store.Project{}, store.ChatSession{}, store.ErrNotFound
	}
	project, err := s.st.GetProject(projectID)
	if err != nil {
		return store.WorkflowView{}, store.Project{}, store.ChatSession{}, err
	}
	conversation, err := s.st.GetChatSession(view.Workflow.ConversationID)
	if err != nil || conversation.UserID != userID || conversation.ProjectID != projectID {
		return store.WorkflowView{}, store.Project{}, store.ChatSession{}, store.ErrNotFound
	}
	return view, project, conversation, nil
}

func (s *Service) RouteInteractionAnswer(ctx context.Context, value store.Interaction) error {
	if value.WorkflowID == "" {
		if s.conversationResumer == nil {
			return ErrExecutorUnavailable
		}
		return s.conversationResumer.ResumeConversationInteraction(ctx, value)
	}
	project, err := s.st.GetProject(value.ProjectID)
	if err != nil {
		return err
	}
	if value.TaskID != "" {
		return s.resumeTaskFromInteraction(ctx, project.OwnerID, value)
	}
	// 新规划流程不会为未批准 Workflow 创建 Interaction。历史 Workflow
	// Interaction 不能再绕过 Proposal revision 边界修改已运行计划。
	return store.ErrInvalidState
}
