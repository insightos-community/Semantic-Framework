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
	"math"
	"strings"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

func (s *Service) schedule(parent context.Context, userID, workflowID string) {
	if s.executor == nil {
		return
	}
	view, err := s.st.GetWorkflowView(workflowID)
	if err != nil || view.Workflow.Status != store.WorkflowStatusRunning ||
		view.Workflow.ConfirmedRevision != view.Workflow.Revision {
		return
	}
	project, err := s.st.GetProject(view.Workflow.ProjectID)
	if err != nil || !project.IsActive || project.ArchivedAt != nil || project.Mode != store.ProjectModeRunning {
		return
	}
	runnable, err := s.st.ListRunnableTasks(workflowID)
	if err != nil {
		return
	}
	for _, task := range runnable {
		if mapErr := s.validateWorkflowMapBinding(view.Workflow); mapErr != nil {
			_, _ = s.st.SetPendingTaskReason(task.ID, task.Revision,
				"map_generation_changed", time.Now().UTC())
			current, getErr := s.st.GetWorkflow(workflowID)
			if getErr == nil && current.Status == store.WorkflowStatusRunning {
				_, _ = s.st.TransitionWorkflowWithReason(workflowID, current.Revision,
					store.WorkflowStatusPaused, "map_generation_changed", time.Now().UTC())
			}
			s.publishCurrent(workflowID, "workflow.map_reference_stale")
			return
		}
		if !s.reserveTask(task) {
			continue
		}
		go s.prepareAndLaunchTask(context.WithoutCancel(parent), userID, view.Workflow, task)
	}
}

// OnRobotAvailabilityChanged 在 Robot/Ability/Skill 从不可用变为可调度时重新
// 评估已有 waiting_resource Task。这里没有引入轮询 Scheduler：Robot Service
// 只发资源边沿事件，实际候选选择仍经过实时目录和持久化 Robot 保留检查。
func (s *Service) OnRobotAvailabilityChanged(ctx context.Context, robotID string) {
	if strings.TrimSpace(robotID) == "" {
		return
	}
	s.mu.Lock()
	stopping := s.shuttingDown
	s.mu.Unlock()
	if stopping {
		return
	}
	workflows, err := s.st.ListUnfinishedWorkflows()
	if err != nil {
		return
	}
	for _, item := range workflows {
		if item.Status != store.WorkflowStatusRunning {
			continue
		}
		view, viewErr := s.st.GetWorkflowView(item.ID)
		if viewErr != nil {
			continue
		}
		waiting := false
		for _, task := range view.Tasks {
			if task.Status == store.TaskStatusPending && task.RequiredRole == "robot" &&
				task.WaitingReason == "waiting_resource" {
				waiting = true
				break
			}
		}
		if !waiting {
			continue
		}
		project, projectErr := s.st.GetProject(item.ProjectID)
		if projectErr == nil {
			s.schedule(context.WithoutCancel(ctx), project.OwnerID, item.ID)
		}
	}
}

func (s *Service) reserveTask(task store.Task) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.activeTasks[task.ID]; exists {
		return false
	}
	// nil 表示已预留但尚未启动 Run。Task ID 是首层并发门禁；真实 Robot
	// 与工作区资源在完成后绑定后再通过 claimTaskResources 原子占用。
	s.activeTasks[task.ID] = nil
	return true
}

func (s *Service) bindReservedTaskCancel(taskID string, cancel context.CancelFunc) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, reserved := s.activeTasks[taskID]; !reserved {
		cancel()
		return false
	}
	s.activeTasks[taskID] = cancel
	return true
}

func (s *Service) cancelTaskAttempt(taskID string) {
	s.mu.Lock()
	cancel := s.activeTasks[taskID]
	runID := s.activeRuns[taskID]
	s.mu.Unlock()
	if runID != "" {
		_, _ = s.st.TransitionRunStatus(runID, []string{store.RunStatusQueued,
			store.RunStatusRunning, store.RunStatusWaitingInput},
			store.RunStatusCancelling, time.Now().UTC())
	}
	if cancel != nil {
		cancel()
	}
}

func (s *Service) claimTaskResources(workflow store.Workflow, task store.Task) bool {
	keys := taskResourceKeys(workflow, task)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range keys {
		if owner := s.activeResources[key]; owner != "" && owner != task.ID {
			return false
		}
	}
	for _, key := range keys {
		s.activeResources[key] = task.ID
	}
	return true
}

func taskResourceKeys(workflow store.Workflow, task store.Task) []string {
	keys := make([]string, 0, 1)
	var requirements struct {
		WorkspaceWrite bool `json:"workspace_write"`
	}
	// Robot 保留已经由 assigned_robot_id 的持久唯一约束负责，Server 重启后
	// 仍然成立；这里仅保留没有数据库字段承载的进程内工作区写锁，避免维护
	// 两份可能不一致的 Robot 占用状态。
	if json.Unmarshal(task.ResourceRequirements, &requirements) == nil && requirements.WorkspaceWrite {
		// 工作区是 Project 资源，而不是 Workflow 私有资源。同一 Project 内即使
		// 后续允许多个 Workflow，也不能让两个写 Task 并发修改同一目录。
		keys = append(keys, "workspace:"+workflow.ProjectID)
	}
	return keys
}

func (s *Service) releaseTask(task store.Task) {
	s.mu.Lock()
	if cancel, ok := s.activeTasks[task.ID]; ok {
		if cancel != nil {
			cancel()
		}
		delete(s.activeTasks, task.ID)
	}
	delete(s.activeRuns, task.ID)
	s.mu.Unlock()
}

// releaseTaskResources 只在 Task 已取得业务终态后释放 Robot/工作区。一次
// Agent Run 结束并不代表 Task 结束，尤其 robot.run accepted 后物理执行仍在
// Pilot 中运行；过早释放会让同一 Robot 在两个 Task 之间串单。
func (s *Service) releaseTaskResources(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, owner := range s.activeResources {
		if owner == taskID {
			delete(s.activeResources, key)
		}
	}
}

func (s *Service) prepareAndLaunchTask(ctx context.Context, userID string,
	workflow store.Workflow, task store.Task, planningAnswer ...*store.Interaction) {
	// 后绑定和 Task Planning 都可能包含真实模型调用。Task 仍是 pending 时也
	// 必须可取消，否则用户停止 Workflow 后，迟到的规划结果仍会创建 SubTask。
	planningCtx, cancelPlanning := context.WithCancel(ctx)
	if !s.bindReservedTaskCancel(task.ID, cancelPlanning) {
		return
	}
	var prepared store.Task
	var err error
	if len(planningAnswer) > 0 {
		prepared, err = s.prepareRunnableTask(planningCtx, userID, workflow, task,
			planningAnswer[0])
	} else {
		prepared, err = s.prepareRunnableTask(planningCtx, userID, workflow, task)
	}
	cancelPlanning()
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, ErrTaskWaitingInput) {
			s.releaseTask(task)
			if errors.Is(err, ErrTaskWaitingInput) {
				s.publishCurrent(workflow.ID, "task.waiting_input")
			}
			return
		}
		if errors.Is(err, ErrTaskWaitingResource) || errors.Is(err, ErrTaskBusy) ||
			errors.Is(err, store.ErrRobotReserved) {
			current, getErr := s.st.GetTask(task.ID)
			if getErr == nil && current.Status == store.TaskStatusPending {
				_, _ = s.st.SetPendingTaskReason(current.ID, current.Revision,
					"waiting_resource", time.Now().UTC())
			}
			s.releaseTask(task)
			s.publishCurrent(workflow.ID, "task.waiting")
			return
		}
		// Task Planning、后绑定校验等确定性准备错误已经有对应的失败 Run。
		// 若仍把 Task 留在 pending，系统既不会重试它，也不会让 Workflow
		// 进入终态，页面只能永久显示一个没有 SubTask 的等待项。这里使用
		// 通用 failed 状态并把原错误留在 reason，资源暂不可用仍走上面的等待分支。
		s.releaseTask(task)
		s.failTask(task.ID, err)
		s.finishOrSchedule(context.WithoutCancel(ctx), userID, workflow.ID)
		return
	}
	if !s.claimTaskResources(workflow, prepared) {
		current, getErr := s.st.GetTask(prepared.ID)
		if getErr == nil && current.Status == store.TaskStatusPending {
			_, _ = s.st.SetPendingTaskReason(current.ID, current.Revision,
				"waiting_resource", time.Now().UTC())
		}
		s.releaseTask(prepared)
		s.publishCurrent(workflow.ID, "task.waiting")
		return
	}
	currentWorkflow, checkErr := s.st.GetWorkflow(workflow.ID)
	if checkErr != nil || currentWorkflow.Status != store.WorkflowStatusRunning ||
		currentWorkflow.ConfirmedRevision != currentWorkflow.Revision {
		s.releaseTask(prepared)
		return
	}
	if launchErr := s.launchReservedTask(ctx, userID, currentWorkflow, prepared, nil); launchErr != nil {
		s.releaseTask(prepared)
		return
	}
	s.appendMilestone(workflow.ID, prepared.ID, "", "task_started", store.TaskStatusRunning)
	s.publishCurrent(workflow.ID, "task.started")
}

func (s *Service) prepareRunnableTask(ctx context.Context, userID string,
	workflow store.Workflow, task store.Task, planningAnswer ...*store.Interaction) (store.Task, error) {
	robotTask := task.RequiredRole == "robot"
	// 普通 Worker 只需首次后绑定；Robot Task 在真正启动前还要复核实时目录，
	// 因为设备可能在计划批准后、Task ready 前离线或被其他 Project 占用。
	if task.AssignedAgentID == "" || robotTask {
		project, projectErr := s.st.GetProject(workflow.ProjectID)
		if projectErr != nil {
			return store.Task{}, projectErr
		}
		resolver, ok := s.planner.(TaskAssignmentResolver)
		if !ok {
			return store.Task{}, ErrTaskWaitingResource
		} else {
			assignment, err := resolver.ResolveTaskAssignment(ctx, TaskAssignmentRequest{
				Project: project, Workflow: workflow, Task: task,
			})
			if err != nil {
				return store.Task{}, err
			}
			var assigned store.Task
			if task.AssignedAgentID == "" {
				assigned, err = s.st.AssignTask(task.ID, task.Revision,
					assignment.AgentID, assignment.RobotID, time.Now().UTC())
				if err == nil {
					s.appendMilestone(workflow.ID, task.ID, "", "task_assigned", store.TaskStatusPending)
				}
			} else if task.AssignedAgentID == assignment.AgentID &&
				task.AssignedRobotID == assignment.RobotID {
				assigned = task
			} else {
				assigned, err = s.st.ReassignPendingTask(task.ID, task.Revision,
					assignment.AgentID, assignment.RobotID, time.Now().UTC())
				if err == nil {
					s.appendMilestone(workflow.ID, task.ID, "", "task_reassigned", store.TaskStatusPending)
				}
			}
			if err != nil {
				return store.Task{}, err
			}
			task = assigned
		}
	}
	subTasks, err := s.st.ListSubTasks(task.ID)
	if err != nil {
		return store.Task{}, err
	}
	if len(subTasks) != 0 {
		return task, nil
	}
	project, err := s.st.GetProject(workflow.ProjectID)
	if err != nil {
		return store.Task{}, err
	}
	conversation, err := s.st.GetChatSession(workflow.ConversationID)
	if err != nil {
		return store.Task{}, err
	}
	planning, err := s.st.SetPendingTaskReason(task.ID, task.Revision,
		"planning_subtasks", time.Now().UTC())
	if err != nil {
		return store.Task{}, err
	}
	// Task 已完成 Agent/Robot 后绑定，但模型规划可能持续几十秒。这里复用
	// pending Task 的 reason 暴露真实中间状态，避免 Studio 把“正在规划”
	// 误显示成分配后卡死；它不是新的调度状态，也不会改变 Task DAG 语义。
	task = planning
	s.publishCurrent(workflow.ID, "task.planning_subtasks")
	request := TaskPlanRequest{UserID: userID, Project: project, Conversation: conversation,
		Workflow: workflow, Task: taskDraft(task)}
	if len(planningAnswer) > 0 {
		request.Answer = planningAnswer[0]
	}
	plan := TaskPlanResult{}
	if provider, ok := s.planner.(TaskPlanResultProvider); ok {
		plan, err = provider.PlanTaskWithResult(ctx, request)
	} else {
		plan.SubTasks, err = s.planner.PlanTask(ctx, request)
	}
	if err != nil {
		return store.Task{}, err
	}
	updated, _, err := s.st.SetTaskSubTasks(task.ID, task.Revision, plan.SubTasks, time.Now().UTC())
	if err == nil {
		s.publishCurrent(workflow.ID, "task.subtasks_planned")
		s.appendTaskPlanMessage(workflow.ID, task.ID, plan.Summary, plan.SubTasks)
	}
	return updated, err
}

func taskDraft(task store.Task) store.TaskDraft {
	return store.TaskDraft{ID: task.ID, RequiredRole: task.RequiredRole,
		RequiredCapabilities: task.RequiredCapabilities,
		ResourceRequirements: task.ResourceRequirements, Goal: task.Goal,
		Input: task.Input, CompletionCriteria: task.CompletionCriteria}
}

func (s *Service) executeTask(ctx context.Context, userID string, workflow store.Workflow,
	task store.Task, answer *store.Interaction, run store.RunSession) {
	runStatus, runError := store.RunStatusFailed, "Task Runtime 未正常结束"
	defer func() {
		_, _ = s.st.FinishRunSession(run.ID, nil, runStatus, runError, time.Now().UTC())
		s.releaseTask(task)
		_ = s.continueOrFinishTask(context.Background(), task.ID)
	}()
	project, err := s.st.GetProject(workflow.ProjectID)
	if err != nil {
		s.failTask(task.ID, err)
		return
	}
	conversation, err := s.st.GetChatSession(workflow.ConversationID)
	if err != nil {
		s.failTask(task.ID, err)
		return
	}
	var result TaskResult
	robotExecutionStarted := false
	var execErr error
	func() {
		runnable, listErr := s.st.ListRunnableSubTasks(task.ID)
		if listErr != nil {
			execErr = listErr
			return
		}
		if len(runnable) == 0 {
			return
		}
		currentSubTask, transitionErr := s.st.TransitionSubTask(runnable[0].ID,
			runnable[0].Revision, store.TaskStatusRunning, "", nil, time.Now().UTC())
		if transitionErr != nil {
			execErr = transitionErr
			return
		}
		s.publishCurrent(workflow.ID, "subtask.started")
		if currentSubTask.Kind == "robot_skill" {
			// Task Planning 只保存稳定的业务意图，不能把缺少实时位姿或持物状态的
			// 部分 input 直接交给 Pilot。每个 Robot SubTask 都启动一次短 Robot
			// Agent Run，由它读取当前 Robot/Task 上下文、补齐 Pydantic 所需输入并
			// 调用 robot.run。accepted 后模型 Run 可以结束，物理终态仍由事件推进。
			result, execErr = s.executor.ExecuteTask(ctx, TaskExecution{
				UserID: userID, Project: project, Conversation: conversation,
				Workflow: workflow, Task: task, SubTasks: []store.SubTask{currentSubTask},
				Answer: answer, Run: run,
			})
			var execution store.RobotExecution
			if execErr == nil {
				execution, execErr = s.st.GetRobotExecutionBySubTask(currentSubTask.ID)
				if execErr != nil {
					execErr = fmt.Errorf("Robot Agent 未调用 robot.run 建立 Execution: %w", execErr)
				}
			}
			if execErr != nil {
				return
			}
			if _, attachErr := s.st.AttachSubTaskExecution(currentSubTask.ID,
				currentSubTask.Revision, execution.ID, time.Now().UTC()); attachErr != nil {
				// Fake 或高速真机后端可能在 Agent Run 收尾前已经回传终态，
				// OnRobotExecutionChanged 会先关联 Execution 并推进 revision。
				// 此时同一 execution_ref 的冲突表示事件已先完成收敛，不是
				// 第二次物理执行；只有关联对象不同才是真正的并发错误。
				latest, latestErr := s.st.GetSubTask(currentSubTask.ID)
				if latestErr != nil {
					execErr = fmt.Errorf("重新读取 Robot SubTask 关联: %w", latestErr)
					return
				}
				if !errors.Is(attachErr, store.ErrRevisionConflict) || latest.ExecutionRef != execution.ID {
					execErr = fmt.Errorf(
						"关联 Robot Execution %s（当前关联 %s，SubTask 状态 %s）: %w",
						execution.ID, latest.ExecutionRef, latest.Status, attachErr)
					return
				}
			}
			robotExecutionStarted = true
			// robot.run accepted 只结束本次模型决策；Task/SubTask 继续运行，
			// 后续由 Robot Execution 终态事件收敛，不能在这里伪造 completed。
			return
		}
		result, execErr = s.executor.ExecuteTask(ctx, TaskExecution{
			UserID: userID, Project: project, Conversation: conversation,
			Workflow: workflow, Task: task, SubTasks: []store.SubTask{currentSubTask},
			Answer: answer, Run: run,
		})
		if execErr != nil {
			return
		}
		latest, latestErr := s.st.GetSubTask(currentSubTask.ID)
		if latestErr != nil {
			execErr = latestErr
			return
		}
		resultBody, _ := json.Marshal(map[string]any{
			"summary": result.Summary, "evidence": json.RawMessage(result.Evidence),
		})
		if _, execErr = s.st.TransitionSubTask(latest.ID, latest.Revision,
			store.TaskStatusCompleted, "", resultBody, time.Now().UTC()); execErr != nil {
			return
		}
		s.publishCurrent(workflow.ID, "subtask.completed")
		// 一个 Agent Run 只对一个 SubTask 的结果负责。即使后续 SubTask
		// 已经满足依赖，也由持久状态驱动下一次短 Run；这样每个结果、Trace
		// 和失败边界都能精确归属，不会再由一次回复批量“完成”整张清单。
	}()
	current, getErr := s.st.GetTask(task.ID)
	if getErr != nil {
		return
	}
	if current.Status == store.TaskStatusStopping || errors.Is(execErr, context.Canceled) {
		runStatus, runError = store.RunStatusCancelled, "Task 已停止"
		_ = s.stopTaskSubTasks(context.Background(), workflow.ProjectID, task.ID)
		current, _ = s.st.GetTask(task.ID)
		if current.Status == store.TaskStatusStopping && taskSubTasksSafelyTerminal(mustSubTasks(s.st, task.ID)) {
			_, _ = s.st.TransitionTask(current.ID, current.Revision, store.TaskStatusStopped,
				"user_stopped", "", nil, time.Now().UTC())
		}
		return
	}
	if current.Status == store.TaskStatusPaused && errors.Is(execErr, ErrTaskWaitingInput) {
		runStatus, runError = store.RunStatusCompleted, ""
		s.publishCurrent(workflow.ID, "task.waiting_input")
		s.appendMilestone(workflow.ID, current.ID, "", "task_waiting_input", store.TaskStatusPaused)
		return
	}
	if execErr != nil {
		runError = execErr.Error()
		s.failTask(task.ID, execErr)
		return
	}
	if robotExecutionStarted {
		// 即便物理执行已经极快结束，也统一让 defer 中的
		// continueOrFinishTask 读取持久 SubTask 状态，决定启动下一步或完成
		// Task；这里不能沿用同步 agent_step 的“本轮返回即完成 Task”路径。
		runStatus, runError = store.RunStatusCompleted, ""
		return
	}
	if hasActiveSubTask(mustSubTasks(s.st, task.ID)) {
		runStatus, runError = store.RunStatusCompleted, ""
		return
	}
	current, getErr = s.st.GetTask(task.ID)
	if getErr == nil && current.Status == store.TaskStatusRunning {
		_, _ = s.st.TransitionTask(task.ID, current.Revision, store.TaskStatusCompleted,
			"", result.Summary, result.Evidence, time.Now().UTC())
		s.publishCurrent(workflow.ID, "task.completed")
		s.appendMilestone(workflow.ID, task.ID, "", "task_completed", store.TaskStatusCompleted)
		s.releaseTaskResources(task.ID)
	}
	runStatus, runError = store.RunStatusCompleted, ""
}

// continueOrFinishTask 是 Task Agent Run 结束和 Robot Execution 终态共同使用
// 的唯一延续入口。它只查看持久 SubTask 状态：有可运行项时创建新的短 Agent
// Run；全部完成时收敛 Task；仍有活动物理执行时保持等待，不进行模型轮询。
func (s *Service) continueOrFinishTask(parent context.Context, taskID string) error {
	task, err := s.st.GetTask(taskID)
	if err != nil {
		return err
	}
	workflow, err := s.st.GetWorkflow(task.WorkflowID)
	if err != nil {
		return err
	}
	project, err := s.st.GetProject(workflow.ProjectID)
	if err != nil {
		return err
	}
	if task.Status != store.TaskStatusRunning {
		s.finishOrSchedule(parent, project.OwnerID, workflow.ID)
		return nil
	}
	items, err := s.st.ListSubTasks(task.ID)
	if err != nil {
		return err
	}
	allCompleted := len(items) > 0
	for _, item := range items {
		allCompleted = allCompleted && subTaskSatisfiedForCompletion(item)
	}
	if allCompleted {
		completed, transitionErr := s.st.TransitionTask(task.ID, task.Revision,
			store.TaskStatusCompleted, "", "所有 SubTask 已按真实结果完成", nil,
			time.Now().UTC())
		if transitionErr != nil {
			return transitionErr
		}
		s.publishCurrent(completed.WorkflowID, "task.completed")
		s.appendMilestone(completed.WorkflowID, completed.ID, "", "task_completed", store.TaskStatusCompleted)
		s.releaseTaskResources(completed.ID)
		s.finishOrSchedule(parent, project.OwnerID, workflow.ID)
		return nil
	}
	for _, item := range items {
		if item.Status == store.TaskStatusRunning || item.Status == store.TaskStatusStopping {
			// Robot Agent 在 robot.run accepted 后即可结束本次模型 Run，但物理
			// SubTask 仍在 Pilot 中执行。此时不能因为另一个无依赖 SubTask 可运行
			// 就释放同一 Robot 的执行边界；必须等待终态事件后再推进。
			// waiting_agent 可能恰好在 Task Execution Run 释放 activeTasks 之前
			// 到达：事件处理当时无法取得同一 Task 的写门禁。Run 结束后从持久
			// Execution/Event 重新接续一次决策，既不轮询模型，也不会重复物理动作。
			if item.Status == store.TaskStatusRunning && item.Kind == "robot_skill" &&
				item.ExecutionRef != "" {
				execution, getErr := s.st.GetRobotExecution(item.ExecutionRef)
				if getErr != nil {
					return getErr
				}
				if execution.Status == "waiting_agent" {
					requested, found, requestErr := s.latestRobotAgentRequest(execution.ID)
					if requestErr != nil {
						return requestErr
					}
					if found {
						s.launchRobotAgentDecision(context.WithoutCancel(parent), task,
							item, execution, requested)
					}
				}
			}
			return nil
		}
	}
	runnable, err := s.st.ListRunnableSubTasks(task.ID)
	if err != nil || len(runnable) == 0 {
		return err
	}
	if !s.reserveTask(task) {
		return nil
	}
	if !s.claimTaskResources(workflow, task) {
		s.releaseTask(task)
		return nil
	}
	if err := s.launchRunningTask(parent, project.OwnerID, workflow, task); err != nil {
		s.releaseTask(task)
		return err
	}
	return nil
}

// launchRunningTask 为同一个 running Task 的下一个 SubTask 创建新的 Agent
// Run。Task 在整个生命周期内保持 running，避免通过 running→pending 的伪状态
// 来驱动调度；同一 Task 的 activeTasks 门禁保证只有一个可写 Agent Run。
func (s *Service) launchRunningTask(parent context.Context, userID string,
	workflow store.Workflow, task store.Task) error {
	now := time.Now().UTC()
	run := store.RunSession{ID: store.NewRunSessionID(), ProjectID: workflow.ProjectID,
		ChatSessionID: workflow.ConversationID, WorkflowID: workflow.ID, TaskID: task.ID,
		Kind: store.RunKindTaskExecution, ContextID: task.ContextID,
		AgentID: task.AssignedAgentID, AgentName: task.AssignedAgentID,
		Status: store.RunStatusQueued, StartedAt: now, UpdatedAt: now}
	if err := s.st.CreateRunSession(run); err != nil {
		return err
	}
	run, err := s.st.TransitionRunStatus(run.ID, []string{store.RunStatusQueued},
		store.RunStatusRunning, time.Now().UTC())
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(parent))
	s.mu.Lock()
	s.activeTasks[task.ID] = cancel
	s.activeRuns[task.ID] = run.ID
	s.mu.Unlock()
	go s.executeTask(runCtx, userID, workflow, task, nil, run)
	return nil
}

func subTaskSatisfiedForCompletion(item store.SubTask) bool {
	if item.Status == store.TaskStatusCompleted {
		return true
	}
	return (item.Status == store.TaskStatusFailed && item.WaitingReason == "replaced_after_failure") ||
		(item.Status == store.TaskStatusStopped && item.WaitingReason == "revised_after_failure")
}

func hasActiveSubTask(items []store.SubTask) bool {
	for _, item := range items {
		switch item.Status {
		case store.TaskStatusPending, store.TaskStatusRunning,
			store.TaskStatusPaused, store.TaskStatusStopping:
			return true
		}
	}
	return false
}

func (s *Service) launchReservedTask(parent context.Context, userID string, workflow store.Workflow,
	task store.Task, answer *store.Interaction, allowInterruptedRetry ...bool) error {
	sourceInteractionID := ""
	if answer != nil {
		sourceInteractionID = answer.ID
	}
	now := time.Now().UTC()
	run := store.RunSession{ID: store.NewRunSessionID(), ProjectID: workflow.ProjectID,
		ChatSessionID: workflow.ConversationID, WorkflowID: workflow.ID, TaskID: task.ID,
		SourceInteractionID: sourceInteractionID, Kind: store.RunKindTaskExecution,
		ContextID: task.ContextID, AgentID: task.AssignedAgentID, AgentName: task.AssignedAgentID,
		Status: store.RunStatusQueued, StartedAt: now, UpdatedAt: now}
	var runningTask store.Task
	var err error
	if answer != nil {
		retry := len(allowInterruptedRetry) > 0 && allowInterruptedRetry[0]
		var created bool
		run, runningTask, created, err = s.st.StartTaskContinuation(task.ID,
			task.Revision, run, retry, now)
		if err != nil {
			return err
		}
		if !created {
			return ErrTaskBusy
		}
	} else {
		if err = s.st.CreateRunSession(run); err != nil {
			return err
		}
		runningTask, err = s.st.TransitionTask(task.ID, task.Revision, store.TaskStatusRunning,
			"", "", nil, now)
		if err != nil {
			_, _ = s.st.FinishRunSession(run.ID, []string{store.RunStatusQueued},
				store.RunStatusFailed, "Task 状态启动失败", time.Now().UTC())
			return err
		}
	}
	run, err = s.st.TransitionRunStatus(run.ID, []string{store.RunStatusQueued},
		store.RunStatusRunning, time.Now().UTC())
	if err != nil {
		_, _ = s.st.TransitionTask(runningTask.ID, runningTask.Revision,
			store.TaskStatusPaused, "run_start_failed", "", nil, time.Now().UTC())
		return err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(parent))
	s.mu.Lock()
	s.activeTasks[task.ID] = cancel
	s.activeRuns[task.ID] = run.ID
	s.mu.Unlock()
	go s.executeTask(runCtx, userID, workflow, runningTask, answer, run)
	return nil
}

func (s *Service) failTask(taskID string, cause error) {
	current, err := s.st.GetTask(taskID)
	if err != nil {
		return
	}
	for _, subTask := range mustSubTasks(s.st, taskID) {
		if subTask.Status == store.TaskStatusRunning {
			_, _ = s.st.TransitionSubTask(subTask.ID, subTask.Revision,
				store.TaskStatusFailed, cause.Error(), nil, time.Now().UTC())
		} else if subTask.Status == store.TaskStatusPending {
			_, _ = s.st.TransitionSubTask(subTask.ID, subTask.Revision,
				store.TaskStatusStopped, "task_failed", nil, time.Now().UTC())
		}
	}
	if current.Status == store.TaskStatusPending || current.Status == store.TaskStatusRunning {
		_, _ = s.st.TransitionTask(current.ID, current.Revision, store.TaskStatusFailed,
			cause.Error(), "", nil, time.Now().UTC())
	}
	s.publishCurrent(current.WorkflowID, "task.failed")
	s.appendMilestone(current.WorkflowID, current.ID, "", "task_failed", store.TaskStatusFailed)
	s.releaseTaskResources(current.ID)
}

func (s *Service) finishOrSchedule(ctx context.Context, userID, workflowID string) {
	view, err := s.st.GetWorkflowView(workflowID)
	if err != nil {
		return
	}
	if view.Workflow.Status == store.WorkflowStatusStopping {
		_ = s.convergeStoppedWorkflow(workflowID)
		return
	}
	if view.Workflow.Status != store.WorkflowStatusRunning {
		return
	}
	allCompleted := len(view.Tasks) > 0
	failed := false
	for _, task := range view.Tasks {
		allCompleted = allCompleted && task.Status == store.TaskStatusCompleted
		failed = failed || task.Status == store.TaskStatusFailed
	}
	if failed {
		_ = s.stopPendingWork(view)
		current, getErr := s.st.GetWorkflow(workflowID)
		if getErr == nil && current.Status == store.WorkflowStatusRunning {
			terminal, transitionErr := s.st.TransitionWorkflowWithReason(workflowID, current.Revision,
				store.WorkflowStatusFailed, "task_failed", time.Now().UTC())
			if transitionErr == nil {
				s.publishCurrent(workflowID, "workflow.failed")
				s.appendMilestone(workflowID, "", "", "workflow_failed", store.WorkflowStatusFailed)
				s.resumeWorkflowTerminal(ctx, terminal.ID)
			}
		}
		return
	}
	if allCompleted {
		terminal, transitionErr := s.st.TransitionWorkflow(workflowID, view.Workflow.Revision,
			store.WorkflowStatusCompleted, time.Now().UTC())
		if transitionErr == nil {
			s.publishCurrent(workflowID, "workflow.completed")
			s.appendMilestone(workflowID, "", "", "workflow_completed", store.WorkflowStatusCompleted)
			s.resumeWorkflowTerminal(ctx, terminal.ID)
		}
		return
	}
	s.schedule(ctx, userID, workflowID)
}

// resumeWorkflowTerminal 只在 Workflow 终态事务成功后触发一次 Leader 总结。
// 固定里程碑负责即时展示，模型总结负责把 Task 的真实结果和证据组织成用户
// 可读回复；总结失败不会回滚已经完成的 Workflow，也不会重复执行任何 Task。
func (s *Service) resumeWorkflowTerminal(ctx context.Context, workflowID string) {
	if s.conversationResumer == nil {
		return
	}
	view, err := s.st.GetWorkflowView(workflowID)
	if err != nil {
		return
	}
	_ = s.conversationResumer.ResumeWorkflowTerminal(context.WithoutCancel(ctx), view)
}

func (s *Service) stopPendingWork(view store.WorkflowView) error {
	for _, subTask := range view.SubTasks {
		if subTask.Status == store.TaskStatusPending {
			if _, err := s.st.TransitionSubTask(subTask.ID, subTask.Revision,
				store.TaskStatusStopped, "workflow_stopped", nil, time.Now().UTC()); err != nil {
				return err
			}
		}
	}
	for _, task := range view.Tasks {
		if task.Status == store.TaskStatusPending {
			if _, err := s.st.TransitionTask(task.ID, task.Revision,
				store.TaskStatusStopped, "workflow_stopped", "", nil, time.Now().UTC()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) stopWorkflowTasks(ctx context.Context, projectID, workflowID string) error {
	view, err := s.st.GetWorkflowView(workflowID)
	if err != nil {
		return err
	}
	for _, task := range view.Tasks {
		switch task.Status {
		case store.TaskStatusPending, store.TaskStatusRunning,
			store.TaskStatusPaused, store.TaskStatusStopping:
			s.cancelTaskAttempt(task.ID)
		}
		task, err = s.moveTaskTowardStop(task)
		if err != nil {
			return err
		}
		if err := s.stopTaskSubTasks(ctx, projectID, task.ID); err != nil {
			return err
		}
		latest, getErr := s.st.GetTask(task.ID)
		if getErr != nil {
			return getErr
		}
		if latest.Status == store.TaskStatusStopping &&
			taskSubTasksSafelyTerminal(mustSubTasks(s.st, latest.ID)) {
			if _, err = s.st.TransitionTask(latest.ID, latest.Revision,
				store.TaskStatusStopped, "user_stopped", "", nil, time.Now().UTC()); err != nil &&
				!errors.Is(err, store.ErrRevisionConflict) {
				return err
			}
		}
	}
	return nil
}

func (s *Service) moveTaskTowardStop(task store.Task) (store.Task, error) {
	for attempt := 0; attempt < 3; attempt++ {
		var (
			updated store.Task
			err     error
		)
		switch task.Status {
		case store.TaskStatusPending:
			updated, err = s.st.TransitionTask(task.ID, task.Revision,
				store.TaskStatusStopped, "user_stopped", "", nil, time.Now().UTC())
		case store.TaskStatusRunning, store.TaskStatusPaused:
			updated, err = s.st.TransitionTask(task.ID, task.Revision,
				store.TaskStatusStopping, "user_stopped", "", nil, time.Now().UTC())
		default:
			return task, nil
		}
		if err == nil {
			return updated, nil
		}
		if !errors.Is(err, store.ErrRevisionConflict) {
			return store.Task{}, err
		}
		task, err = s.st.GetTask(task.ID)
		if err != nil {
			return store.Task{}, err
		}
	}
	return store.Task{}, store.ErrRevisionConflict
}

func (s *Service) stopTaskSubTasks(ctx context.Context, projectID, taskID string) error {
	for _, subTask := range mustSubTasks(s.st, taskID) {
		var err error
		subTask, err = s.moveSubTaskTowardStop(subTask)
		if err != nil {
			return err
		}
		if subTask.Status != store.TaskStatusStopping {
			// 另一个并发收敛者可能已经把它推进到 stopped/paused；此时本次
			// 停止请求已经被处理，不应把 revision 竞争暴露给用户。
			continue
		}
		if subTask.ExecutionRef == "" {
			if _, err := s.st.TransitionSubTask(subTask.ID, subTask.Revision,
				store.TaskStatusStopped, "task_stopped", nil, time.Now().UTC()); err != nil &&
				!errors.Is(err, store.ErrRevisionConflict) {
				return err
			}
			continue
		}
		if execution, getErr := s.st.GetRobotExecution(subTask.ExecutionRef); getErr == nil {
			handled, handleErr := s.settleTerminalExecutionForStop(taskID, subTask, execution)
			if handleErr != nil {
				return handleErr
			}
			if handled {
				continue
			}
		} else if errors.Is(getErr, store.ErrNotFound) {
			// execution_ref 已经证明 Robot Skill 曾被接受，但旧数据中的 Execution
			// 记录缺失。此时既没有可安全重放的动作，也没有可下发 stop 的目标；
			// 收敛到状态未知并保留引用，交给现场确认入口处理。
			return s.pauseStoppingRobotSubTaskUnknown(taskID, subTask,
				missingRobotExecution(subTask.ExecutionRef))
		} else {
			return getErr
		}
		if s.robotStop == nil {
			return ErrExecutorUnavailable
		}
		// 对 Robot SubTask 只锁存 stopping 并下发幂等停止命令。Server
		// 重启恢复或并发取消可能再次请求同一 execution_id，Pilot 必须返回
		// 同一停止结果；后续仍只认真实 stopped/interrupted 事件。
		if _, err := s.robotStop.Stop(ctx, projectID, subTask.ExecutionRef,
			"workflow_stopped"); err != nil {
			// stop 请求与 Pilot 终态可能交叉到达。错误返回后重新读取唯一事实：
			// 若执行已终止则按终态收敛；状态未知则回到 paused 并保留 Robot 锁。
			execution, getErr := s.st.GetRobotExecution(subTask.ExecutionRef)
			if getErr == nil {
				if handled, handleErr := s.settleTerminalExecutionForStop(
					taskID, subTask, execution); handleErr != nil {
					return handleErr
				} else if handled {
					continue
				}
				if execution.Status == "interrupted" {
					return s.pauseStoppingRobotSubTaskUnknown(taskID, subTask, execution)
				}
			} else if errors.Is(getErr, store.ErrNotFound) {
				return s.pauseStoppingRobotSubTaskUnknown(taskID, subTask,
					missingRobotExecution(subTask.ExecutionRef))
			} else {
				return getErr
			}
			return err
		}
	}
	return nil
}

func (s *Service) moveSubTaskTowardStop(subTask store.SubTask) (store.SubTask, error) {
	for attempt := 0; attempt < 3; attempt++ {
		var (
			updated store.SubTask
			err     error
		)
		switch subTask.Status {
		case store.TaskStatusPending:
			updated, err = s.st.TransitionSubTask(subTask.ID, subTask.Revision,
				store.TaskStatusStopped, "task_stopped", nil, time.Now().UTC())
		case store.TaskStatusRunning, store.TaskStatusPaused:
			updated, err = s.st.TransitionSubTask(subTask.ID, subTask.Revision,
				store.TaskStatusStopping, "task_stopped", nil, time.Now().UTC())
		default:
			return subTask, nil
		}
		if err == nil {
			return updated, nil
		}
		if !errors.Is(err, store.ErrRevisionConflict) {
			return store.SubTask{}, err
		}
		subTask, err = s.st.GetSubTask(subTask.ID)
		if err != nil {
			return store.SubTask{}, err
		}
	}
	return store.SubTask{}, store.ErrRevisionConflict
}

func (s *Service) settleTerminalExecutionForStop(taskID string, subTask store.SubTask,
	execution store.RobotExecution) (bool, error) {
	switch execution.Status {
	case "stopping":
		// 首次请求在下方下发一次 stop；显式重试和 Server 恢复也必须重新对账。
		// 若 Pilot 已离线，Robot Service 会把 Execution 变为 interrupted，随后
		// 本服务收敛到 execution_state_unknown，而不是永久停在 stopping。
		return false, nil
	case "completed":
		_, err := s.st.TransitionSubTask(subTask.ID, subTask.Revision,
			store.TaskStatusStopped, "robot_execution_completed_during_stop",
			robotExecutionResult(execution, "execution.terminal", execution.Result),
			time.Now().UTC())
		return true, err
	case "failed":
		safe := safelyStoppedRobotExecution(execution)
		started, err := s.robotExecutionMayHavePhysicalAction(execution.ID)
		if err != nil {
			return true, err
		}
		held, err := s.robotExecutionSafelyHeld(execution.ID)
		if err != nil {
			return true, err
		}
		if safe || held {
			// Ability 已证明机器人安全保持（含策略超时/异常自终止）：任务失败
			// 是业务结论，物理状态却可判定，直接收敛 stopped 而不是要求人工对账。
			_, err = s.st.TransitionSubTask(subTask.ID, subTask.Revision,
				store.TaskStatusStopped, "robot_execution_failed_but_held",
				robotExecutionResult(execution, "execution.terminal", execution.Error),
				time.Now().UTC())
			return true, err
		}
		if !started {
			// Worker 在物理 Action 前失败（可能已拍照/查询）时，Robot
			// 没有活动物理命令。对一个 terminal execution 再发 robot.stop
			// 只会得到 ErrExecutionNotActive，并把 Workflow 永久卡在 stopping。
			_, err = s.st.TransitionSubTask(subTask.ID, subTask.Revision,
				store.TaskStatusStopped, "robot_execution_failed_before_action",
				robotExecutionResult(execution, "execution.terminal", execution.Error),
				time.Now().UTC())
			return true, err
		}
		return true, s.pauseStoppingRobotSubTaskUnknown(taskID, subTask, execution)
	case "stopped", "cancelled":
		if safelyStoppedRobotExecution(execution) {
			_, err := s.st.TransitionSubTask(subTask.ID, subTask.Revision,
				store.TaskStatusStopped, "robot_execution_stopped",
				robotExecutionResult(execution, "execution.stop_confirmed", execution.Result),
				time.Now().UTC())
			return true, err
		}
		return true, s.pauseStoppingRobotSubTaskUnknown(taskID, subTask, execution)
	default:
		return false, nil
	}
}

func missingRobotExecution(executionID string) store.RobotExecution {
	return store.RobotExecution{
		ID: executionID, Status: "interrupted",
		Error: map[string]any{
			"code":    "EXECUTION_RECORD_MISSING",
			"message": "SubTask 引用的 Robot Execution 记录不存在",
		},
	}
}

func (s *Service) robotExecutionStartedPhysicalAction(executionID string) (bool, error) {
	events, err := s.st.ListRobotExecutionEvents(executionID, 0, 1000)
	if err != nil {
		return false, err
	}
	for _, item := range events {
		if item.Type == "action.started" {
			started, _ := item.Payload["physical_started"].(bool)
			if started {
				return true, nil
			}
		}
	}
	return false, nil
}

func (s *Service) robotExecutionMayHavePhysicalAction(executionID string) (bool, error) {
	// Only explicitly non-physical actions may be excluded. Old/missing metadata
	// or an action without its start record still requires safety reconciliation.
	readOnly := make(map[string]bool)
	var after int64
	for {
		events, err := s.st.ListRobotExecutionEvents(executionID, after, 1000)
		if err != nil {
			return false, err
		}
		for _, item := range events {
			after = item.Sequence
			if !strings.HasPrefix(item.Type, "action.") {
				continue
			}
			id, _ := item.Payload["action_id"].(string)
			if id == "" {
				return true, nil
			}
			if _, exists := readOnly[id]; !exists {
				readOnly[id] = false
			}
			if item.Type == "action.started" {
				physical, physicalKnown := item.Payload["physical"].(bool)
				started, startedKnown := item.Payload["physical_started"].(bool)
				if !physicalKnown || !startedKnown || physical || started {
					return true, nil
				}
				readOnly[id] = true
			}
		}
		if len(events) < 1000 {
			break
		}
	}
	for _, known := range readOnly {
		if !known {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) pauseStoppingRobotSubTaskUnknown(taskID string, subTask store.SubTask,
	execution store.RobotExecution) error {
	task, err := s.st.GetTask(taskID)
	if err != nil {
		return err
	}
	current, err := s.st.GetSubTask(subTask.ID)
	if err != nil {
		return err
	}
	return s.pauseRobotSubTask(task, current, "execution_state_unknown",
		robotExecutionResult(execution, "execution.state_unknown", execution.Error))
}

func (s *Service) convergeStoppedWorkflow(workflowID string) error {
	view, err := s.st.GetWorkflowView(workflowID)
	if err != nil || view.Workflow.Status != store.WorkflowStatusStopping {
		return err
	}
	for _, task := range view.Tasks {
		if task.Status == store.TaskStatusRunning || task.Status == store.TaskStatusStopping || task.Status == store.TaskStatusPaused {
			return nil
		}
		s.releaseTaskResources(task.ID)
	}
	// 保留 user_stopped、execution_state_unknown 或人工确认原因，终态历史才能
	// 解释这次停止如何完成；TransitionWorkflow 会把 reason 无条件清空。
	_, err = s.st.TransitionWorkflowWithReason(workflowID, view.Workflow.Revision,
		store.WorkflowStatusStopped, view.Workflow.Reason, time.Now().UTC())
	if err == nil {
		s.publishCurrent(workflowID, "workflow.stopped")
	}
	return err
}

func taskSubTasksSafelyTerminal(items []store.SubTask) bool {
	for _, item := range items {
		if item.Status != store.TaskStatusCompleted && item.Status != store.TaskStatusStopped &&
			item.Status != store.TaskStatusFailed {
			return false
		}
	}
	return true
}

func (s *Service) resumePausedTasks(parent context.Context, userID, workflowID string) error {
	view, err := s.st.GetWorkflowView(workflowID)
	if err != nil {
		return err
	}
	completed := make(map[string]bool, len(view.Tasks))
	for _, task := range view.Tasks {
		completed[task.ID] = task.Status == store.TaskStatusCompleted
	}
	for _, task := range view.Tasks {
		if task.Status != store.TaskStatusPaused || !dependenciesCompleted(task.ID, view.Dependencies, completed) {
			continue
		}
		if task.WaitingReason != "server_restarted" {
			return store.ErrInvalidState
		}
		if !s.reserveTask(task) {
			continue
		}
		for _, subTask := range view.SubTasks {
			if subTask.TaskID != task.ID || subTask.Status != store.TaskStatusPaused {
				continue
			}
			if subTask.WaitingReason != "server_restarted" || subTask.ExecutionRef != "" {
				s.releaseTask(task)
				return store.ErrInvalidState
			}
			if _, resumeErr := s.st.ResumePausedAgentStep(subTask.ID, subTask.Revision, time.Now().UTC()); resumeErr != nil {
				s.releaseTask(task)
				return resumeErr
			}
		}
		if launchErr := s.launchReservedTask(parent, userID, view.Workflow, task, nil, true); launchErr != nil {
			s.releaseTask(task)
			return launchErr
		}
	}
	return nil
}

func dependenciesCompleted(taskID string, deps []store.TaskDependency, completed map[string]bool) bool {
	for _, dependency := range deps {
		if dependency.TaskID == taskID && !completed[dependency.DependsOnTaskID] {
			return false
		}
	}
	return true
}

func (s *Service) resumeTaskFromInteraction(parent context.Context, userID string, answer store.Interaction) error {
	if existing, err := s.st.GetRunSessionBySourceInteraction(answer.ID); err == nil {
		task, taskErr := s.st.GetTask(answer.TaskID)
		if taskErr == nil && task.Status == store.TaskStatusRunning &&
			(existing.Status == store.RunStatusQueued || existing.Status == store.RunStatusRunning ||
				existing.Status == store.RunStatusWaitingInput) {
			return nil
		}
		return store.ErrContinuationInterrupted
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	view, err := s.st.GetWorkflowView(answer.WorkflowID)
	if err != nil || view.Workflow.Status != store.WorkflowStatusRunning {
		return store.ErrInvalidState
	}
	task, err := s.st.GetTask(answer.TaskID)
	if err != nil || task.WorkflowID != view.Workflow.ID {
		return store.ErrNotFound
	}
	if answer.SourceRevision <= 0 || answer.SourceRevision > task.Revision {
		return store.ErrRevisionConflict
	}
	if task.Status != store.TaskStatusPaused || task.WaitingReason != "waiting_input" {
		return store.ErrInvalidState
	}
	if answer.RunID != "" {
		sourceRun, runErr := s.st.GetRunSession(answer.RunID)
		if runErr != nil {
			return runErr
		}
		if sourceRun.TaskID != task.ID || sourceRun.WorkflowID != view.Workflow.ID {
			return store.ErrInvalidState
		}
		if sourceRun.Kind == store.RunKindTaskPlanning {
			return s.resumeTaskPlanningFromInteraction(parent, userID, answer, view.Workflow, task)
		}
		if sourceRun.Kind != store.RunKindTaskExecution {
			return store.ErrInvalidState
		}
		// task_execution既承载普通SubTask推进，也承载同一Robot Agent的
		// waiting_agent决策与失败Recovery。回答必须根据当前Execution事实回到
		// 原调用路径，不能把三者都当作“重新执行当前SubTask”。
		for _, subTask := range view.SubTasks {
			if subTask.TaskID != task.ID || subTask.Status != store.TaskStatusPaused ||
				subTask.WaitingReason != "waiting_input" || subTask.ExecutionRef == "" {
				continue
			}
			execution, executionErr := s.st.GetRobotExecution(subTask.ExecutionRef)
			if executionErr != nil {
				return executionErr
			}
			switch execution.Status {
			case "waiting_agent":
				return s.resumeRobotDecisionFromInteraction(parent, answer, task)
			case "failed":
				return s.resumeTaskRecoveryFromInteraction(parent, answer, task)
			}
		}
	}
	for _, subTask := range view.SubTasks {
		if subTask.TaskID != task.ID || subTask.Status != store.TaskStatusPaused {
			continue
		}
		if subTask.WaitingReason != "waiting_input" {
			return store.ErrInvalidState
		}
		if _, resumeErr := s.st.ResumePausedAgentStep(subTask.ID, subTask.Revision, time.Now().UTC()); resumeErr != nil {
			return resumeErr
		}
	}
	if !s.reserveTask(task) {
		return ErrTaskBusy
	}
	if err := s.launchReservedTask(parent, userID, view.Workflow, task, &answer); err != nil {
		s.releaseTask(task)
		if _, existingErr := s.st.GetRunSessionBySourceInteraction(answer.ID); existingErr == nil {
			return nil
		}
		return err
	}
	return nil
}

func (s *Service) resumeTaskPlanningFromInteraction(parent context.Context, userID string,
	answer store.Interaction, workflowValue store.Workflow, task store.Task) error {
	pending, err := s.st.ResumePausedTaskPlanning(task.ID, task.Revision, time.Now().UTC())
	if err != nil {
		return err
	}
	if !s.reserveTask(pending) {
		return ErrTaskBusy
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.prepareAndLaunchTask(context.WithoutCancel(parent), userID, workflowValue,
			pending, &answer)
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if _, runErr := s.st.GetRunSessionBySourceInteraction(answer.ID); runErr == nil {
				return nil
			} else if !errors.Is(runErr, store.ErrNotFound) {
				return runErr
			}
		case <-done:
			if _, runErr := s.st.GetRunSessionBySourceInteraction(answer.ID); runErr == nil {
				return nil
			}
			latest, getErr := s.st.GetTask(task.ID)
			if getErr != nil {
				return getErr
			}
			return fmt.Errorf("Task Planning continuation 未启动（状态=%s，原因=%s）: %w",
				latest.Status, latest.WaitingReason, store.ErrInvalidState)
		case <-parent.Done():
			return parent.Err()
		}
	}
}

// PauseTaskForInteraction 在创建 Task 级结构化 Interaction 前建立持久安全边界。
// 调用方必须使用返回 Task 的 revision 作为 Interaction.source_revision。
func (s *Service) PauseTaskForInteraction(userID, projectID, workflowID, taskID string,
	expectedRevision int64) (store.Task, error) {
	view, _, _, err := s.requireWorkflow(userID, projectID, workflowID)
	if err != nil {
		return store.Task{}, err
	}
	if view.Workflow.Status != store.WorkflowStatusRunning {
		return store.Task{}, store.ErrInvalidState
	}
	task, err := s.st.GetTask(taskID)
	if err != nil || task.WorkflowID != workflowID || task.Revision != expectedRevision {
		return store.Task{}, store.ErrRevisionConflict
	}
	if task.Status == store.TaskStatusPaused && task.WaitingReason == "recovery_running" {
		var candidate *store.SubTask
		for _, item := range mustSubTasks(s.st, taskID) {
			if item.Status == store.TaskStatusPaused && item.WaitingReason == "recovery_running" &&
				item.Kind == "robot_skill" && item.ExecutionRef != "" {
				if candidate != nil {
					return store.Task{}, store.ErrInvalidState
				}
				copy := item
				candidate = &copy
			}
		}
		if candidate == nil {
			return store.Task{}, store.ErrInvalidState
		}
		paused, _, updateErr := s.st.UpdatePausedRobotWaitReason(task.ID, task.Revision,
			candidate.ID, candidate.Revision, "recovery_running", "waiting_input",
			time.Now().UTC())
		if updateErr == nil {
			s.publishCurrent(workflowID, "task.waiting_input")
		}
		return paused, updateErr
	}
	if task.Status != store.TaskStatusRunning &&
		!(task.Status == store.TaskStatusPending && task.WaitingReason == "planning_subtasks") {
		return store.Task{}, store.ErrRevisionConflict
	}
	paused, err := s.st.TransitionTask(task.ID, task.Revision, store.TaskStatusPaused,
		"waiting_input", "", nil, time.Now().UTC())
	if err != nil {
		return store.Task{}, err
	}
	for _, subTask := range mustSubTasks(s.st, taskID) {
		if subTask.Status == store.TaskStatusRunning {
			_, _ = s.st.TransitionSubTask(subTask.ID, subTask.Revision,
				store.TaskStatusPaused, "waiting_input", nil, time.Now().UTC())
		}
	}
	s.publishCurrent(workflowID, "task.waiting_input")
	return paused, nil
}

// ResumeTaskAfterInteractionFailure 只用于“Task 已暂停但 Interaction 持久化失败”
// 的补偿路径。它恢复原 Run 可继续收敛为 failed，避免遗留无法回答的 paused Task。
func (s *Service) ResumeTaskAfterInteractionFailure(userID, projectID, workflowID, taskID string,
	expectedRevision int64) error {
	if _, _, _, err := s.requireWorkflow(userID, projectID, workflowID); err != nil {
		return err
	}
	task, err := s.st.GetTask(taskID)
	if err != nil || task.Revision != expectedRevision || task.Status != store.TaskStatusPaused ||
		task.WaitingReason != "waiting_input" {
		return store.ErrRevisionConflict
	}
	subTasks := mustSubTasks(s.st, taskID)
	for _, subTask := range subTasks {
		if subTask.Status != store.TaskStatusPaused || subTask.WaitingReason != "waiting_input" ||
			subTask.ExecutionRef == "" {
			continue
		}
		execution, executionErr := s.st.GetRobotExecution(subTask.ExecutionRef)
		if executionErr == nil && execution.Status == "failed" {
			_, _, updateErr := s.st.UpdatePausedRobotWaitReason(task.ID, task.Revision,
				subTask.ID, subTask.Revision, "waiting_input", "recovery_running",
				time.Now().UTC())
			if updateErr == nil {
				s.publishCurrent(workflowID, "task.recovery_running")
			}
			return updateErr
		}
	}
	if len(subTasks) == 0 {
		_, err = s.st.ResumePausedTaskPlanning(task.ID, task.Revision, time.Now().UTC())
		if err != nil {
			return err
		}
	} else {
		if _, err := s.st.TransitionTask(task.ID, task.Revision, store.TaskStatusRunning,
			"", "", nil, time.Now().UTC()); err != nil {
			return err
		}
	}
	for _, subTask := range subTasks {
		if subTask.Status == store.TaskStatusPaused && subTask.WaitingReason == "waiting_input" {
			_, _ = s.st.TransitionSubTask(subTask.ID, subTask.Revision,
				store.TaskStatusRunning, "", nil, time.Now().UTC())
		}
	}
	s.publishCurrent(workflowID, "task.interaction_failed")
	return nil
}

// RecoverInterruptedWorkflows 在 Agent Runtime 已把旧 Run 标记为 failed/cancelled
// 后调用。从未启动的 pending Task 可安全重新调度；被中断的 running attempt
// 收敛为 paused 并等待用户显式 Resume；waiting_input 的已落库回答随后由
// Interaction recovery 原子创建新的 continuation Run。
func (s *Service) RecoverInterruptedWorkflows(now time.Time) error {
	workflows, err := s.st.ListUnfinishedWorkflows()
	if err != nil {
		return err
	}
	for _, workflow := range workflows {
		view, getErr := s.st.GetWorkflowView(workflow.ID)
		if getErr != nil {
			return getErr
		}
		switch workflow.Status {
		case store.WorkflowStatusRunning, store.WorkflowStatusPaused:
			interrupted := false
			unknownRobotTasks := make(map[string]bool)
			for _, subTask := range view.SubTasks {
				if subTask.Status == store.TaskStatusRunning {
					interrupted = true
					reason := "server_restarted"
					if subTask.Kind == "robot_skill" {
						current := subTask
						if current.ExecutionRef == "" {
							if execution, executionErr := s.st.GetRobotExecutionBySubTask(current.ID); executionErr == nil {
								if attached, attachErr := s.st.AttachSubTaskExecution(current.ID,
									current.Revision, execution.ID, now); attachErr == nil {
									current = attached
								}
							}
						}
						if current.ExecutionRef != "" {
							// Server 无法仅凭进程重启判断已接受的物理 Action 是否仍在
							// Robot 上运行。保留 execution_ref 和 Robot 分配，等待 Pilot
							// 按同一 execution ID 对账，绝不能用 Resume 自动重放。
							reason = "execution_state_unknown"
							unknownRobotTasks[current.TaskID] = true
						}
						subTask = current
					}
					_, _ = s.st.TransitionSubTask(subTask.ID, subTask.Revision,
						store.TaskStatusPaused, reason, nil, now)
				}
			}
			for _, task := range view.Tasks {
				if task.Status == store.TaskStatusRunning {
					interrupted = true
					reason := "server_restarted"
					if unknownRobotTasks[task.ID] {
						reason = "execution_state_unknown"
					}
					_, _ = s.st.TransitionTask(task.ID, task.Revision,
						store.TaskStatusPaused, reason, "", nil, now)
				}
			}
			currentView, currentErr := s.st.GetWorkflowView(workflow.ID)
			if currentErr != nil {
				return currentErr
			}
			pausedForAnotherReason := false
			workflowPauseReason := "server_restarted"
			for _, task := range currentView.Tasks {
				if task.Status == store.TaskStatusPaused {
					if task.WaitingReason != "waiting_input" {
						pausedForAnotherReason = true
					}
					if task.WaitingReason == "execution_state_unknown" {
						workflowPauseReason = "execution_state_unknown"
					}
				}
			}
			if workflow.Status == store.WorkflowStatusRunning &&
				(interrupted || pausedForAnotherReason || len(view.Tasks) == 0) {
				current, _ := s.st.GetWorkflow(workflow.ID)
				_, err = s.st.TransitionWorkflowWithReason(workflow.ID, current.Revision,
					store.WorkflowStatusPaused, workflowPauseReason, now)
			} else if workflow.Status == store.WorkflowStatusRunning {
				project, projectErr := s.st.GetProject(workflow.ProjectID)
				if projectErr != nil {
					return projectErr
				}
				s.schedule(context.Background(), project.OwnerID, workflow.ID)
			}
		case store.WorkflowStatusStopping:
			err = s.stopWorkflowTasks(context.Background(), workflow.ProjectID, workflow.ID)
			if err == nil {
				err = s.convergeStoppedWorkflow(workflow.ID)
			}
		}
		if err != nil {
			return fmt.Errorf("恢复 Workflow %s 失败: %w", workflow.ID, err)
		}
		s.publishCurrent(workflow.ID, "workflow.recovered")
	}
	return nil
}

type workflowMapBinding struct {
	MapID      string `json:"map_id"`
	Generation int64  `json:"generation"`
	Selections []struct {
		Kind     string    `json:"kind"`
		EntityID string    `json:"entity_id,omitempty"`
		FrameID  string    `json:"frame_id,omitempty"`
		Position []float64 `json:"position,omitempty"`
	} `json:"selections"`
}

func (s *Service) validateWorkflowMapBinding(workflow store.Workflow) error {
	if len(workflow.MapScope) == 0 || string(workflow.MapScope) == "{}" || string(workflow.MapScope) == "null" {
		return nil
	}
	return s.validateMapScope(workflow.ProjectID, workflow.MapScope)
}

func (s *Service) validateMapScope(projectID string, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "{}" || string(raw) == "null" {
		return nil
	}
	var binding workflowMapBinding
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&binding); err != nil || !store.ValidMapSlot(binding.MapID) ||
		binding.Generation <= 0 || len(binding.Selections) == 0 {
		return store.ErrInvalidState
	}
	ids := make([]string, 0, len(binding.Selections))
	for _, selection := range binding.Selections {
		switch selection.Kind {
		case "entity", "region":
			if strings.TrimSpace(selection.EntityID) == "" {
				return store.ErrInvalidState
			}
			ids = append(ids, selection.EntityID)
		case "point":
			if strings.TrimSpace(selection.FrameID) == "" || len(selection.Position) != 3 {
				return store.ErrInvalidState
			}
			for _, value := range selection.Position {
				if math.IsNaN(value) || math.IsInf(value, 0) {
					return store.ErrInvalidState
				}
			}
		default:
			return store.ErrInvalidState
		}
	}
	return s.st.ValidateMapReference(projectID, binding.MapID, binding.Generation, ids)
}

func mustSubTasks(st *store.Store, taskID string) []store.SubTask {
	items, _ := st.ListSubTasks(taskID)
	return items
}
